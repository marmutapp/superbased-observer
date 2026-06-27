package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/guard/compile"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Native-dialect compilation orchestration (guard spec §13.2 / G11).
// dialectRunner is the I/O sequencer between the pure guard/compile
// translators, the guard engine and the store's one-owner guard
// helpers — the mcpsecRunner pattern: compile computes, guard
// evaluates, the store persists, and THIS file only sequences. Three
// triggers share the drift-check flow: the `observer start` baseline
// pass, the watcher-ingest config-change re-check (debounced, riding
// the same guard config-watch trigger mcpsec uses), and the
// `observer guard compile --diff` CLI. Writers live HERE, never in
// the pure package (module rule 1).

// dialectMu serializes compile/drift passes per process: a
// watcher-triggered re-check and a CLI compile may otherwise
// interleave their read-apply-write cycles.
var dialectMu sync.Mutex

// dialectRunner sequences one store+guard pair through §13.2 flows.
// st may be nil (init without an openable DB): file compilation still
// works and pinning degrades to a reported issue.
type dialectRunner struct {
	st     *store.Store
	g      *guard.Guard
	logger *slog.Logger
	home   string
	// targets mirrors [guard.dialects].targets (empty = all
	// implemented dialects).
	targets []string
}

// configGuardDialects is the [guard.dialects] subset the runner needs.
type configGuardDialects struct {
	Compile bool
	Targets []string
}

// newDialectRunner builds a runner; home resolution failure degrades
// to an inert runner (nil), logged — never a daemon failure.
func newDialectRunner(cfg configGuardDialects, st *store.Store, g *guard.Guard, logger *slog.Logger) *dialectRunner {
	if g == nil || !cfg.Compile {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("guard compile: home dir unavailable; dialect compilation inert", "err", err)
		return nil
	}
	return &dialectRunner{st: st, g: g, logger: logger, home: home, targets: cfg.Targets}
}

// candidateTargets resolves which implemented targets a pass covers:
// the explicit names when given (unknown/deferred names become
// issues), else every implemented target allowed by the config
// filter.
func (r *dialectRunner) candidateTargets(names []string) ([]compile.Target, []string) {
	var issues []string
	if len(names) > 0 {
		var out []compile.Target
		for _, name := range names {
			t, ok := compile.TargetFor(name)
			switch {
			case !ok:
				issues = append(issues, fmt.Sprintf("unknown dialect %q", name))
			case !t.Implemented:
				issues = append(issues, fmt.Sprintf("dialect %q is deferred: %s", name, t.Reason))
			default:
				out = append(out, t)
			}
		}
		return out, issues
	}
	allow := map[string]bool{}
	for _, name := range r.targets {
		allow[name] = true
	}
	var out []compile.Target
	for _, t := range compile.Targets() {
		if !t.Implemented {
			continue
		}
		if len(allow) > 0 && !allow[string(t.Dialect)] {
			continue
		}
		out = append(out, t)
	}
	return out, issues
}

// watchPaths returns the candidate targets' config paths for the
// guard config-watch trigger (SetDialectRescan).
func (r *dialectRunner) watchPaths() []string {
	targets, _ := r.candidateTargets(nil)
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Path(r.home))
	}
	return out
}

// dialectReport is one target's outcome from a compile pass.
type dialectReport struct {
	Target  compile.Target
	Path    string
	Rows    []compile.RowResult
	Entries int
	Added   []compile.Entry
	Removed []compile.Entry
	// Wrote / Pinned report what the pass persisted; Skipped is the
	// non-empty reason when the target was left untouched.
	Wrote   bool
	Pinned  bool
	Skipped string
	Issues  []string
}

// InSync reports whether the native config already matches policy.
func (d dialectReport) InSync() bool {
	return d.Skipped == "" && len(d.Issues) == 0 && len(d.Added) == 0 && len(d.Removed) == 0
}

// CompileTargets runs one compile pass. write=false is the --diff
// mode: nothing is written or pinned, the reports carry what WOULD
// change. requireExisting skips targets whose config file does not
// exist yet (the no-explicit-selection CLI default: absence of
// ~/.config/opencode/opencode.json means the client is not in use —
// creating it would strand managed junk); explicit selection (init
// tool flags, --target) creates files.
func (r *dialectRunner) CompileTargets(ctx context.Context, names []string, write, requireExisting bool) []dialectReport {
	if r == nil {
		return nil
	}
	dialectMu.Lock()
	defer dialectMu.Unlock()

	targets, issues := r.candidateTargets(names)
	var reports []dialectReport
	for _, issue := range issues {
		reports = append(reports, dialectReport{Issues: []string{issue}})
	}
	effective := r.g.EffectiveRules()
	now := time.Now().UTC()
	for _, t := range targets {
		rep := dialectReport{Target: t, Path: t.Path(r.home)}
		compiled := compile.Compile(t, effective)
		rep.Rows = compiled.Rows
		rep.Entries = len(compiled.Entries)

		existing, err := os.ReadFile(rep.Path)
		exists := err == nil
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			rep.Issues = append(rep.Issues, fmt.Sprintf("read %s: %v", rep.Path, err))
			reports = append(reports, rep)
			continue
		}
		if requireExisting && !exists {
			rep.Skipped = "config file not present (client not in use?) — select the tool explicitly to create it"
			reports = append(reports, rep)
			continue
		}
		res, err := t.Apply(existing, compiled.Entries)
		if err != nil {
			rep.Issues = append(rep.Issues, fmt.Sprintf("cannot rewrite %s safely: %v", rep.Path, err))
			reports = append(reports, rep)
			continue
		}
		rep.Added, rep.Removed = res.Added, res.Removed
		if write {
			if res.Updated != nil {
				if err := writeDialectFile(rep.Path, res.Updated, existing, exists); err != nil {
					rep.Issues = append(rep.Issues, err.Error())
					reports = append(reports, rep)
					continue
				}
				rep.Wrote = true
			}
			if r.st == nil {
				rep.Issues = append(rep.Issues, "pin skipped: observer DB unavailable — drift detection starts after the next `observer guard compile` with the daemon DB reachable")
			} else if err := r.st.UpsertGuardPin(ctx, store.GuardPinRow{
				Kind: "native_dialect", Name: string(t.Dialect), Client: t.Client,
				PinHash:   compile.HashEntries(compiled.Entries),
				FirstSeen: now, LastVerified: now, Status: "pinned",
			}); err != nil {
				rep.Issues = append(rep.Issues, fmt.Sprintf("pin upsert failed: %v", err))
			} else {
				rep.Pinned = true
			}
		}
		reports = append(reports, rep)
	}
	return reports
}

// writeDialectFile writes the rewritten config, creating parent dirs
// and preserving the existing file's permission bits (0600 for new
// files — settings can carry sensitive entries).
func writeDialectFile(path string, content, _ []byte, existed bool) error {
	mode := fs.FileMode(0o600)
	if existed {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// dialectDriftSummary reports one drift-check pass.
type dialectDriftSummary struct {
	Checked        int
	Drifted        []string // "claude-code: 2 entries missing" lines
	EventsRecorded int
	Issues         []string
}

// CheckDrift re-verifies every pinned native-dialect artifact against
// the CURRENT effective policy (§13.2 drift = the file is missing
// entries policy compiles to, whichever side changed). Only PINNED
// targets are checked — never-compiled installs stay silent (the
// baseline-quiet posture). R-204 fires once per drift state: a pin
// already marked drifted against the same policy hash stays silent
// until recompile or further change. Unparseable files surface as
// issues, never events. Compliant pins are re-stamped (hash refresh
// after a policy change that the file already satisfies).
func (r *dialectRunner) CheckDrift(ctx context.Context) dialectDriftSummary {
	var sum dialectDriftSummary
	if r == nil || r.st == nil {
		return sum
	}
	dialectMu.Lock()
	defer dialectMu.Unlock()

	pins, err := r.st.LoadGuardPins(ctx, "native_dialect")
	if err != nil {
		r.logger.Warn("guard compile: pin load failed; drift check skipped", "err", err)
		sum.Issues = append(sum.Issues, err.Error())
		return sum
	}
	if len(pins) == 0 {
		return sum
	}
	effective := r.g.EffectiveRules()
	now := time.Now().UTC()
	var findings []policy.PostureFinding
	for _, pin := range pins {
		t, ok := compile.TargetFor(pin.Name)
		if !ok || !t.Implemented {
			continue // pin from a different/newer observer version
		}
		sum.Checked++
		path := t.Path(r.home)
		compiled := compile.Compile(t, effective)
		wantHash := compile.HashEntries(compiled.Entries)

		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			sum.Issues = append(sum.Issues, fmt.Sprintf("read %s: %v", path, err))
			continue
		}
		res, err := t.Apply(existing, compiled.Entries)
		if err != nil {
			sum.Issues = append(sum.Issues, fmt.Sprintf("cannot verify %s: %v", path, err))
			continue
		}
		if len(res.Added) == 0 {
			// Compliant (stale-stricter leftovers don't drift —
			// `guard compile` retires them). Refresh the pin when the
			// policy hash moved or a prior drift resolved by hand.
			if pin.Status != "pinned" || pin.PinHash != wantHash {
				r.upsertPin(ctx, t, wantHash, "pinned", now)
			}
			continue
		}
		detail := fmt.Sprintf("%d compiled entr%s missing (e.g. %s %s)",
			len(res.Added), pluralIES(len(res.Added)), res.Added[0].Action, res.Added[0].Value)
		sum.Drifted = append(sum.Drifted, fmt.Sprintf("%s: %s", t.Dialect, detail))
		if pin.Status == "drifted" && pin.PinHash == wantHash {
			continue // already recorded against this policy state
		}
		findings = append(findings, policy.PostureFinding{
			Kind: policy.PostureFindingDialectDrift, Client: t.Client,
			Target: path, Detail: detail,
		})
		r.upsertPin(ctx, t, wantHash, "drifted", now)
	}
	if len(findings) == 0 {
		return sum
	}
	verdicts := r.g.EvaluatePostureFindings(findings, now)
	n, err := r.st.PersistGuardVerdicts(ctx, verdicts)
	if err != nil {
		r.logger.Warn("guard compile: verdict persist failed", "recorded", n, "err", err)
	}
	sum.EventsRecorded = n
	for i := range verdicts {
		r.g.MaybeAlert(verdicts[i])
	}
	return sum
}

// upsertPin stamps a native_dialect pin row, WARN-and-continue.
func (r *dialectRunner) upsertPin(ctx context.Context, t compile.Target, hash, status string, now time.Time) {
	if err := r.st.UpsertGuardPin(ctx, store.GuardPinRow{
		Kind: "native_dialect", Name: string(t.Dialect), Client: t.Client,
		PinHash: hash, FirstSeen: now, LastVerified: now, Status: status,
	}); err != nil {
		r.logger.Warn("guard compile: pin upsert failed", "dialect", t.Dialect, "err", err)
	}
}

// pluralIES is the entry/entries suffix helper.
func pluralIES(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// debouncedDialectRescan coalesces watcher-triggered drift re-checks
// (an editor save loop on settings.json shouldn't run N passes).
func debouncedDialectRescan(r *dialectRunner, wait time.Duration) func() {
	var mu sync.Mutex
	var timer *time.Timer
	return func() {
		mu.Lock()
		defer mu.Unlock()
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(wait, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sum := r.CheckDrift(ctx)
			if len(sum.Drifted) > 0 {
				r.logger.Info("guard compile: config-change drift check", "drifted", len(sum.Drifted))
			}
		})
	}
}

// ---- CLI surface (§13.2: observer guard compile [--diff]) ----

func newGuardCompileCmd() *cobra.Command {
	var (
		configPath string
		diff       bool
		targetSel  []string
	)
	cmd := &cobra.Command{
		Use:   "compile",
		Short: "Compile the effective policy into each client's NATIVE permission rules",
		Long: "Translates the effective guard policy into native enforcement config\n" +
			"(Claude Code permissions.deny/ask; OpenCode permission.bash in\n" +
			"last-match-wins order) so the client itself blocks even when the\n" +
			"observer daemon is down — defense-in-depth from one policy\n" +
			"definition. Writes are ADDITIVE: user entries are never clobbered;\n" +
			"managed entries are recognised by value and retired when policy\n" +
			"stops wanting them. Compiled entries reflect ENFORCE-mode decisions\n" +
			"regardless of the current guard mode (the native plane is the\n" +
			"always-on backstop); broadened approximations are demoted to ask and\n" +
			"documented per rule (--diff prints them). Each artifact is pinned\n" +
			"(guard_pins kind=native_dialect); later drift fires R-204.\n\n" +
			"--diff reports divergence without writing (exit 1 when out of sync)\n" +
			"— the R-204 remediation surface.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			cfg, g, err := buildCLIGuard(configPath)
			if err != nil {
				return err
			}
			if !cfg.Guard.Dialects.Compile {
				return fmt.Errorf("dialect compilation is disabled ([guard.dialects] compile = false)")
			}
			database, err := db.Open(cmd.Context(), db.Options{Path: cfg.Observer.DBPath})
			var st *store.Store
			if err != nil {
				fmt.Fprintf(out, "WARN: observer DB unavailable (%v) — compiling without pinning\n", err)
			} else {
				defer database.Close()
				st = store.New(database)
			}
			r := newDialectRunner(configGuardDialects{
				Compile: cfg.Guard.Dialects.Compile,
				Targets: cfg.Guard.Dialects.Targets,
			}, st, g, newLogger(cfg.Observer.LogLevel))
			if r == nil {
				return fmt.Errorf("dialect compilation unavailable (home directory unresolvable)")
			}
			reports := r.CompileTargets(cmd.Context(), targetSel,
				!diff, len(targetSel) == 0)
			outOfSync := printDialectReports(out, reports, diff)
			if diff && outOfSync {
				// Exit 1 (the guard lint precedent) — composable into
				// scripts and the R-204 remediation loop.
				return fmt.Errorf("native configs out of sync with policy — run `observer guard compile`")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&diff, "diff", false, "report divergence between policy and the native configs without writing; exit 1 when out of sync")
	cmd.Flags().StringArrayVar(&targetSel, "target", nil, "compile only the named dialect(s) (claude-code, opencode); creates the config file if absent")
	return cmd
}

// printDialectReports renders compile-pass reports; returns whether
// anything is out of sync.
func printDialectReports(out io.Writer, reports []dialectReport, diff bool) bool {
	outOfSync := false
	for _, rep := range reports {
		if rep.Target.Dialect == "" {
			for _, issue := range rep.Issues {
				fmt.Fprintf(out, "ISSUE: %s\n", issue)
			}
			outOfSync = true
			continue
		}
		switch {
		case rep.Skipped != "":
			fmt.Fprintf(out, "%-12s skipped: %s\n", rep.Target.Dialect, rep.Skipped)
		case diff && rep.InSync():
			fmt.Fprintf(out, "%-12s in sync (%d managed entr%s) — %s\n",
				rep.Target.Dialect, rep.Entries, pluralIES(rep.Entries), rep.Path)
		case diff:
			outOfSync = true
			fmt.Fprintf(out, "%-12s OUT OF SYNC — %s\n", rep.Target.Dialect, rep.Path)
			for _, e := range rep.Added {
				fmt.Fprintf(out, "  missing: %-4s %s\n", e.Action, e.Value)
			}
			for _, e := range rep.Removed {
				fmt.Fprintf(out, "  stale:   %-4s %s (no longer compiled; `observer guard compile` retires it)\n", e.Action, e.Value)
			}
		default:
			verb := "unchanged"
			if rep.Wrote {
				verb = fmt.Sprintf("wrote %d added / %d retired", len(rep.Added), len(rep.Removed))
			}
			pin := ""
			if rep.Pinned {
				pin = ", pinned"
			}
			fmt.Fprintf(out, "%-12s %s (%d managed entr%s%s) — %s\n",
				rep.Target.Dialect, verb, rep.Entries, pluralIES(rep.Entries), pin, rep.Path)
		}
		if diff {
			for _, row := range rep.Rows {
				if len(row.Emitted) > 0 && row.Fidelity == compile.FidelityApprox {
					fmt.Fprintf(out, "  note %s: %s\n", row.RuleID, row.Note)
				}
			}
		}
		for _, issue := range rep.Issues {
			fmt.Fprintf(out, "  ISSUE: %s\n", issue)
			outOfSync = true
		}
	}
	return outOfSync
}
