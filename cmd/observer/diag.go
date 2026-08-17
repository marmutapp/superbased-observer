package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/diag"
)

func newDoctorCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "doctor [tool]",
		Short: "Run health checks on the observer (DB, hooks, MCP)",
		Long: "Verifies database integrity, hook checksums, MCP registrations, and\n" +
			"the running binary's path against what was recorded by `observer init`.\n" +
			"Exits non-zero if any check fails.\n\n" +
			"Pass an optional tool name to scope the output to one integration,\n" +
			"e.g. `observer doctor opencode` (provider-compatibility probe) or\n" +
			"`observer doctor org` (enrolment). The name is matched as a\n" +
			"substring against check ids.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			binary, err := absoluteBinaryPath()
			if err != nil {
				return err
			}
			report := diag.Run(cmd.Context(), diag.DoctorOptions{
				Config:     cfg,
				DB:         database,
				BinaryPath: binary,
				// Fold the obs-plane admission health checks (judge
				// reachability + audit-chain verify) into doctor, built in the
				// one obs wiring file so diag never imports internal/obs.
				ExtraChecks: obsAdmissionDoctorChecks(cmd.Context(), cfg, database, slog.Default()),
			})
			if len(args) == 1 {
				// A known adapter name gets a focused, per-adapter capture
				// check (detection + watch path + proxy-routing for the
				// routable ones). Otherwise fall back to a substring filter
				// over the general checks (org, hooks, db, …).
				if c, ok := diag.CheckAdapter(args[0], cfg); ok {
					report = diag.Report{Checks: []diag.Check{c}}
				} else {
					report = report.Filter(args[0])
					if len(report.Checks) == 0 {
						return fmt.Errorf("no doctor checks or adapters match %q (adapters: claude-code, codex, opencode, cursor, cline, copilot, gemini-cli, …; checks: org, hooks, db, mcp, governance)", args[0])
					}
				}
			}
			if jsonOut {
				body, _ := json.MarshalIndent(report, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
			} else {
				printReport(cmd.OutOrStdout(), report)
			}
			if report.Failed() {
				return errors.New("one or more checks failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON instead of formatted output")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show observer DB stats and recent activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			snap, err := diag.Snapshot(cmd.Context(), database, cfg.Observer.DBPath)
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(snap, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			fmt.Fprint(cmd.OutOrStdout(), diag.FormatStatus(snap))
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}

func newTailCmd() *cobra.Command {
	var (
		configPath   string
		intervalSecs int
		pageSize     int
		sinceMinutes int
	)
	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Live-stream captured actions to the terminal",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			_, database, cleanup, err := loadConfigAndDB(ctx, configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			opts := diag.TailOptions{
				Interval: time.Duration(intervalSecs) * time.Second,
				PageSize: pageSize,
			}
			if sinceMinutes > 0 {
				opts.Since = time.Now().UTC().Add(-time.Duration(sinceMinutes) * time.Minute)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "tail running — ctrl-c to stop")
			if err := diag.Tail(ctx, database, cmd.OutOrStdout(), opts); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().IntVar(&intervalSecs, "interval", 1, "Poll interval in seconds")
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Max rows to read per poll")
	cmd.Flags().IntVar(&sinceMinutes, "since", 0, "Replay actions from the last N minutes before tailing live")
	return cmd
}

// loadConfigAndDB centralizes the config + DB open boilerplate that every
// command in this package needs. Caller must invoke cleanup() to close the DB.
//
// There is deliberately only ONE of these. It used to have a
// `loadConfigAndDBFast` twin that differed solely in passing
// SkipIntegrityCheck, and the split was the bug: `PRAGMA quick_check`
// checksums every page of the file, so the "slow" variant made every
// read-only reporting command's cost scale with the size of the database
// rather than with the query. On the reference 14.7 GB install that was
// >120s to print a table. Callers picked the wrong twin four separate times
// (the MCP server, the daemon, `observer run`, and then the ~85 sites
// reaching this function) because nothing at the call site signals which one
// is correct.
//
// db.Open no longer verifies by default (see db.Options.IntegrityCheck), so
// this is now uniformly the fast path. The probe still runs where it belongs:
// once per daemon via db.RunStartupMaintenance, off the readiness path, and
// as `observer doctor`'s reported `db.integrity` check.
func loadConfigAndDB(ctx context.Context, configPath string) (config.Config, *sql.DB, func(), error) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return config.Config{}, nil, func() {}, fmt.Errorf("load config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
		return config.Config{}, nil, func() {}, fmt.Errorf("ensure db dir: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return config.Config{}, nil, func() {}, fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}
	cleanup := func() { _ = database.Close() }
	return cfg, database, cleanup, nil
}

// runStartupDBMaintenance runs the daemon's one-time DB integrity probe and
// schema-034 path-hash backfill in the background, OFF the readiness path.
// db.Open never verifies by default (fast bind); this pays
// the multi-GB `PRAGMA quick_check` exactly once, after the listener is
// already serving. Called from a single goroutine per daemon process
// (`observer start` / `observer proxy start` / `observer watch`).
//
// Fail-soft: a corruption result is logged loudly (Error, pointing at
// `observer doctor`) but never cancels the caller — the daemon is already up,
// and a false alarm from a transient read must not take it down. Uses its own
// short-lived handle so it doesn't outlive on a component's pool.
func runStartupDBMaintenance(ctx context.Context, configPath string) {
	cfg, database, cleanup, err := loadConfigAndDB(ctx, configPath)
	if err != nil {
		return
	}
	defer cleanup()
	logger := newLogger(cfg.Observer.LogLevel)
	started := time.Now()
	if err := db.RunStartupMaintenance(ctx, database); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		logger.Error("db startup maintenance failed — POSSIBLE CORRUPTION; run `observer doctor`",
			"err", err, "elapsed_ms", time.Since(started).Milliseconds())
		return
	}
	logger.Info("db integrity check ok (background)", "elapsed_ms", time.Since(started).Milliseconds())
}

// printReport renders a diag.Report as one line per check, with optional
// indented detail lines for warn/fail entries.
func printReport(w io.Writer, report diag.Report) {
	for _, c := range report.Checks {
		symbol := "✓"
		switch c.Status {
		case diag.StatusWarn:
			symbol = "⚠"
		case diag.StatusFail:
			symbol = "✗"
		}
		fmt.Fprintf(w, "%s %-20s %s\n", symbol, c.Name, c.Message)
		for _, d := range c.Details {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
	ok, warn, fail := report.Counts()
	fmt.Fprintf(w, "\n%d ok, %d warn, %d fail\n", ok, warn, fail)
	if fail == 0 && warn == 0 {
		// The one celebratory line the CLI allows itself (§9 voice
		// note, calm register): an all-green doctor run earns a star.
		fmt.Fprintf(w, "all checks passed ★\n")
	}
}
