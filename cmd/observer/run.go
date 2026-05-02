package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/compression/shell"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// newRunCmd implements `observer run` — spawn a command, stream its stdout
// through the shell filter engine, and exit with the child's exit code while
// reporting compression stats to stderr and observer_log (spec §10 Layer 1).
func newRunCmd() *cobra.Command {
	var (
		configPath string
		noFilter   bool
		quiet      bool
		jsonStats  bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] <command> [args...]",
		Short: "Run a command, streaming its stdout through the shell filter",
		Long: "Runs the given command with its stdout passed through a per-command\n" +
			"filter chain (spec §10 Layer 1). Stderr and exit code are forwarded\n" +
			"unchanged. Stats are logged to observer_log and printed to stderr.\n" +
			"\n" +
			"Pass-through (no filtering) when [compression.shell].enabled is false,\n" +
			"when the command's basename is in [compression.shell].exclude_commands,\n" +
			"or when --no-filter is set.\n" +
			"\n" +
			"Use -- to separate observer flags from the wrapped command:\n" +
			"    observer run --quiet -- git --no-pager log -n5",
		Args:          cobra.MinimumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd.Context(), args, runOptions{
				configPath: configPath,
				noFilter:   noFilter,
				quiet:      quiet,
				jsonStats:  jsonStats,
				stderr:     cmd.ErrOrStderr(),
				stdout:     cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&noFilter, "no-filter", false, "Pass-through without applying any filter")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "Don't print savings stats on exit")
	cmd.Flags().BoolVar(&jsonStats, "json-stats", false, "Emit end-of-run stats as JSON to stderr")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

type runOptions struct {
	configPath string
	noFilter   bool
	quiet      bool
	jsonStats  bool
	stdout     io.Writer
	stderr     io.Writer
}

// runStats is the JSON shape emitted by --json-stats and the observer_log
// `details` column for `run` entries.
type runStats struct {
	Command    []string `json:"command"`
	Filter     string   `json:"filter,omitempty"` // spec command (e.g. "git" or "go test")
	PassThru   bool     `json:"pass_through"`
	BytesIn    int64    `json:"bytes_in"`
	BytesOut   int64    `json:"bytes_out"`
	LinesIn    int64    `json:"lines_in"`
	LinesOut   int64    `json:"lines_out"`
	ReductionR float64  `json:"reduction_ratio"`
	DurationMs int64    `json:"duration_ms"`
	ExitCode   int      `json:"exit_code"`
}

func runRun(ctx context.Context, argv []string, opts runOptions) error {
	cfg, database, cleanup, err := loadConfigAndDB(ctx, opts.configPath)
	if err != nil {
		return err
	}
	defer cleanup()

	engine, err := buildShellEngine(cfg)
	if err != nil {
		return fmt.Errorf("shell engine: %w", err)
	}

	passThrough := opts.noFilter ||
		!cfg.Compression.Shell.Enabled ||
		isExcludedCommand(cfg.Compression.Shell.ExcludeCommands, argv)

	// Wire the child's stdout to either our stdout directly (pass-through) or
	// through a filter stream that writes to our stdout.
	var (
		stream     *shell.Stream
		stdoutSink io.Writer = opts.stdout
	)
	if !passThrough {
		s, err := engine.Filter(argv, opts.stdout)
		if err != nil {
			return fmt.Errorf("shell filter: %w", err)
		}
		stream = s
		stdoutSink = s
	}

	// Block SIGINT/SIGTERM delivery from terminating the parent so the child
	// (which also receives these via the shared process group) can clean up
	// and return an exit code. We drain the channel but take no action — the
	// child is the one that should respond.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			// swallow
		}
	}()

	start := time.Now()
	child := exec.Command(argv[0], argv[1:]...)
	child.Stdout = stdoutSink
	child.Stderr = opts.stderr
	child.Stdin = os.Stdin
	runErr := child.Run()
	elapsed := time.Since(start)
	exitCode := exitCodeFrom(runErr)

	// Always flush the stream before returning so the final lines reach the
	// underlying stdout even if the child was killed.
	var stats shell.Stats
	var specName string
	if stream != nil {
		if closeErr := stream.Close(); closeErr != nil {
			fmt.Fprintf(opts.stderr, "warn: filter close: %v\n", closeErr)
		}
		stats = stream.Stats()
		if sp := stream.Spec(); sp != nil {
			specName = specDisplayName(sp)
		}
	}

	rs := runStats{
		Command:    argv,
		Filter:     specName,
		PassThru:   passThrough,
		BytesIn:    stats.BytesIn,
		BytesOut:   stats.BytesOut,
		LinesIn:    stats.LinesIn,
		LinesOut:   stats.LinesOut,
		ReductionR: stats.ReductionRatio(),
		DurationMs: elapsed.Milliseconds(),
		ExitCode:   exitCode,
	}

	if err := logRun(ctx, store.New(database), rs); err != nil {
		fmt.Fprintf(opts.stderr, "warn: observer_log: %v\n", err)
	}

	if !opts.quiet {
		reportStats(opts.stderr, rs, opts.jsonStats)
	}

	// Match the child's exit code. Startup failures (binary not found, etc.)
	// land here too — exitCodeFrom returns 127 for those, matching shells.
	if exitCode != 0 {
		return exitErr(exitCode)
	}
	return nil
}

// buildShellEngine returns an engine seeded with the embedded defaults plus
// any *.toml files found under ~/.observer/filters (or the equivalent user
// override dir derived from the config's DBPath).
func buildShellEngine(cfg config.Config) (*shell.Engine, error) {
	e, err := shell.NewEngineWithDefaults()
	if err != nil {
		return nil, err
	}
	userDir := userFilterDir(cfg)
	if userDir != "" {
		if err := e.LoadDir(userDir); err != nil {
			return nil, fmt.Errorf("user filters: %w", err)
		}
	}
	return e, nil
}

// userFilterDir resolves <db_dir>/filters — a sibling directory to the DB so
// test configs point at /tmp/<...>/filters without polluting $HOME.
func userFilterDir(cfg config.Config) string {
	dbPath := cfg.Observer.DBPath
	if dbPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "filters")
}

func isExcludedCommand(excluded []string, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	base := filepath.Base(argv[0])
	for _, e := range excluded {
		if e == base {
			return true
		}
	}
	return false
}

func specDisplayName(s *shell.FilterSpec) string {
	if len(s.Subcommands) == 0 {
		return s.Command
	}
	return s.Command + " " + strings.Join(s.Subcommands, "|")
}

// exitCodeFrom translates exec.Cmd.Run errors into an integer exit code.
// 127 (command-not-found) mirrors POSIX shells; signals use 128+signum.
func exitCodeFrom(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	var pe *os.PathError
	if errors.As(err, &pe) || errors.Is(err, exec.ErrNotFound) {
		return 127
	}
	return 1
}

func reportStats(w io.Writer, rs runStats, asJSON bool) {
	if asJSON {
		body, _ := json.Marshal(rs)
		fmt.Fprintln(w, string(body))
		return
	}
	label := runLabel(rs)
	if rs.BytesIn == 0 {
		fmt.Fprintf(w, "[observer run] %s — no output (%dms, exit=%d)\n",
			label, rs.DurationMs, rs.ExitCode)
		return
	}
	fmt.Fprintf(w,
		"[observer run] %s — %d→%d lines, %s→%s bytes (%.0f%% saved) in %dms, exit=%d\n",
		label,
		rs.LinesIn, rs.LinesOut,
		humanBytes(rs.BytesIn), humanBytes(rs.BytesOut),
		rs.ReductionR*100,
		rs.DurationMs, rs.ExitCode,
	)
}

// runLabel returns a short descriptor of what happened for the run. "bypass"
// means we never asked the engine; "no-filter" means the engine had no spec
// for argv[0]; otherwise the matched spec's display name.
func runLabel(rs runStats) string {
	switch {
	case rs.PassThru:
		return "bypass"
	case rs.Filter == "":
		return "no-filter"
	default:
		return rs.Filter
	}
}

func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * 1024
	)
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1fMB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.1fKB", float64(n)/kb)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func logRun(ctx context.Context, st *store.Store, rs runStats) error {
	// Scrub the argv before it lands in observer_log — commands may embed
	// API keys or tokens. Message is the one-line summary; details is the
	// scrubbed JSON blob.
	scrubber := scrub.New()
	scrubbed := make([]string, len(rs.Command))
	for i, s := range rs.Command {
		scrubbed[i] = scrubber.String(s)
	}
	loggedRS := rs
	loggedRS.Command = scrubbed

	body, err := json.Marshal(loggedRS)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("%s exit=%d bytes=%d→%d", runLabel(rs), rs.ExitCode, rs.BytesIn, rs.BytesOut)
	return st.InsertObserverLog(ctx, "info", "run", msg, string(body))
}

// exitErr bubbles a specific process exit code up to main(), which maps it to
// os.Exit. cobra's SilenceErrors suppresses the normal error banner.
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit code %d", int(e)) }
