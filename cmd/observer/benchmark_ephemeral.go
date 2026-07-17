package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
	"github.com/marmutapp/superbased-observer/internal/config"
)

// benchmark_ephemeral.go stands up an isolated, throwaway observer daemon so a
// claude-code benchmark FIRST-RUN "just works" without the operator hand-wiring
// an isolated `--config`. The claude-code driver's Preflight refuses to run
// against the operator's real ~/.observer/observer.db (a large shared DB makes
// the auto-registered PreToolUse hook stall ~100 s/Bash call — re-spike §Q2), so
// before this a first run failed with a wall-of-text. The ephemeral daemon is
// the RUNNER's-job path the driver comment always pointed at: a fresh temp DB +
// a FREE proxy port + heavy features off, built through the SAME
// buildProxy/buildWatcher seams the real daemon uses, torn down at the end.
//
// The correlation seam for claude-code is HOOKS + PROXY, not the watcher (the
// sandbox transcripts live under the per-attempt temp home the watcher never
// sees — see benchmark_provision.go::prepareClaude). So the ephemeral watcher
// runs with an EMPTY enabled-adapters allow-list: it enters idle mode and never
// scans (or ingests) the operator's real ~/.claude etc. into the throwaway DB.

// isolatedDaemonDriver is implemented by a HarnessDriver whose Preflight
// requires a dedicated isolated observer daemon (its own small DB). The runner
// keys the ephemeral-daemon auto-engage decision on THIS capability, not on the
// harness name (CLAUDE.md #3): claude-code implements it, codex (no Preflight)
// does not.
type isolatedDaemonDriver interface {
	NeedsIsolatedDaemon() bool
}

// newBenchmarkDrivers is the one owner of the harness→driver registry the CLI
// wires into the runner. dbPath is the observer DB the claude-code driver's
// Preflight gates on; pass "" for a capability-only probe (NeedsIsolatedDaemon
// is a property of the driver type, independent of the DB path).
func newBenchmarkDrivers(observerBin, dbPath string) map[string]HarnessDriver {
	return map[string]HarnessDriver{
		"codex":       codexDriver{observerBin: observerBin},
		"claude-code": newClaudeCodeDriver("", dbPath),
	}
}

// shouldEngageEphemeralDaemon decides whether to stand up an isolated ephemeral
// daemon for this run. The operator's explicit --ephemeral-daemon always wins;
// otherwise auto-engage only when they gave NO --config (so we won't override a
// daemon they wired themselves) AND the spec uses a harness whose Preflight
// needs daemon isolation. Pure so the decision is unit-tested without I/O.
func shouldEngageEphemeralDaemon(flag bool, configPath string, spec benchmark.Spec, drivers map[string]HarnessDriver) bool {
	if flag {
		return true
	}
	if strings.TrimSpace(configPath) != "" {
		return false
	}
	return specNeedsIsolatedDaemon(spec, drivers)
}

// specNeedsIsolatedDaemon reports whether any harness in the spec is driven by a
// driver that needs an isolated daemon — i.e. whether an auto-provisioned
// ephemeral daemon would let the run pass Preflight instead of wall-of-texting.
func specNeedsIsolatedDaemon(spec benchmark.Spec, drivers map[string]HarnessDriver) bool {
	seen := map[string]bool{}
	for _, c := range spec.Configs {
		if seen[c.Harness] {
			continue
		}
		seen[c.Harness] = true
		if d, ok := drivers[c.Harness]; ok {
			if idd, ok := d.(isolatedDaemonDriver); ok && idd.NeedsIsolatedDaemon() {
				return true
			}
		}
	}
	return false
}

// ephemeralDaemon is a running isolated proxy+watcher pair plus the temp dir +
// config the benchmark runner should point at. Close() tears the whole thing
// down (cancel → drain goroutines → close DB handles → remove temp dir) and is
// safe to defer for the success, error, and ctrl-c paths.
type ephemeralDaemon struct {
	// ConfigPath is the temp config.toml the runner threads everywhere (the
	// claude-code hooks write its DB; the driver's Preflight reads its db_path;
	// the store reads its DB). ProxyURL is the isolated proxy base; DBPath +
	// Dir are surfaced for the honest "your real data is untouched" log line.
	ConfigPath string
	ProxyURL   string
	DBPath     string
	Dir        string

	cancel   context.CancelFunc
	done     chan struct{} // closed once the proxy+watcher goroutines exit
	cleanups []func()      // DB-close closures, run AFTER the goroutines drain
}

// startEphemeralDaemon writes an isolated benchmark config, builds the
// proxy+watcher through the shared daemon seams, waits until the proxy accepts
// connections, and returns a handle. Every failure path before the handle is
// returned removes the temp dir and stops any goroutines already started, so a
// half-built daemon never leaks.
func startEphemeralDaemon(ctx context.Context) (*ephemeralDaemon, error) {
	dir, err := os.MkdirTemp("", "sbo-bench-daemon-*")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()

	port, err := freeTCPPort()
	if err != nil {
		return nil, fmt.Errorf("allocate free proxy port: %w", err)
	}
	dbPath := filepath.Join(dir, "observer.db")
	configPath := filepath.Join(dir, "config.toml")
	if err := writeEphemeralBenchmarkConfig(configPath, dbPath, port); err != nil {
		return nil, fmt.Errorf("write isolated config: %w", err)
	}

	// The daemon owns a context derived from the run's ctx so Close() (or a
	// run-ctx cancel / ctrl-c) stops the proxy+watcher.
	dctx, cancel := context.WithCancel(ctx)

	p, pCleanup, addr, _, _, err := buildProxy(dctx, configPath, "", 0, "127.0.0.1")
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build proxy: %w", err)
	}
	w, wCleanup, err := buildWatcher(dctx, configPath)
	if err != nil {
		cancel()
		pCleanup()
		return nil, fmt.Errorf("build watcher: %w", err)
	}

	done := make(chan struct{})
	g, gctx := errgroup.WithContext(dctx)
	g.Go(func() error {
		if err := p.ListenAndServe(gctx, addr); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("proxy: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := w.Watch(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("watcher: %w", err)
		}
		return nil
	})
	go func() {
		_ = g.Wait()
		close(done)
	}()

	// Full teardown used both on a readiness failure below and by Close().
	teardown := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		// Close DB handles AFTER the goroutines that use them have drained.
		wCleanup()
		pCleanup()
	}

	if err := waitProxyReady(dctx, addr, 15*time.Second); err != nil {
		teardown()
		return nil, fmt.Errorf("proxy never became ready on %s: %w", addr, err)
	}

	d := &ephemeralDaemon{
		ConfigPath: configPath,
		ProxyURL:   "http://" + addr,
		DBPath:     dbPath,
		Dir:        dir,
		cancel:     cancel,
		done:       done,
		cleanups:   []func(){wCleanup, pCleanup},
	}
	success = true
	return d, nil
}

// Close stops the daemon and removes its temp dir. Idempotent-ish: safe to call
// once via defer. Order matters — cancel the context, wait for the goroutines
// to release the DB, THEN close the handles and delete the dir.
func (d *ephemeralDaemon) Close() {
	if d == nil {
		return
	}
	d.cancel()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
	}
	for _, c := range d.cleanups {
		if c != nil {
			c()
		}
	}
	_ = os.RemoveAll(d.Dir)
}

// writeEphemeralBenchmarkConfig writes a minimal, isolated config.toml: its own
// DB in the temp dir, a free proxy port, retention prune off, code-intel and
// conversation compression off (fast + measures raw billed tokens), and an
// EMPTY watch allow-list so the watcher idles instead of scanning the
// operator's real session dirs into the throwaway DB.
func writeEphemeralBenchmarkConfig(configPath, dbPath string, port int) error {
	cfg := config.Default()
	cfg.Observer.DBPath = dbPath
	cfg.Observer.LogLevel = "error"
	cfg.Observer.Retention.PruneOnStartup = false
	// Empty allow-list ⇒ registry.Detected returns nothing ⇒ the watcher enters
	// idle mode and never scans/ingests the operator's real dirs. The
	// claude-code correlation seam is hooks+proxy, not the watcher.
	cfg.Observer.Watch.EnabledAdapters = []string{}
	cfg.Proxy.Port = port
	cfg.CodeIntel.Enabled = false
	cfg.Compression.Conversation.Enabled = false
	return config.WriteToml(configPath, cfg)
}

// freeTCPPort asks the kernel for an unused loopback TCP port. There is a small
// TOCTOU window between close and the proxy re-binding it; acceptable for a
// short-lived benchmark daemon (the proxy bind is the authority — a lost race
// surfaces as a readiness timeout, not silent corruption).
func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitProxyReady dials addr until a TCP connection succeeds (the proxy
// goroutine has bound it) or the deadline/ctx elapses.
func waitProxyReady(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	for {
		conn, err := dialer.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// announceEphemeralDaemon prints the honest first-run banner that replaces the
// old Preflight wall-of-text.
func announceEphemeralDaemon(out io.Writer, d *ephemeralDaemon) {
	fmt.Fprintf(out,
		"benchmark: started an isolated ephemeral daemon at %s (proxy %s) — your real Observer data is untouched.\n",
		d.Dir, d.ProxyURL)
}
