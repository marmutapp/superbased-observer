package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
	"github.com/marmutapp/superbased-observer/internal/config"
)

// NOTE: these tests never launch claude/codex and never send a request through
// the proxy — no model call, no spend. The ephemeral daemon binds a kernel-
// allocated FREE loopback port (127.0.0.1:0) with a temp DB; it is never the
// operator's live daemon on :8820 or their real ~/.observer/observer.db.

// --- auto-engage decision (capability-based, no I/O) ---

func TestShouldEngageEphemeralDaemon(t *testing.T) {
	t.Parallel()
	drivers := newBenchmarkDrivers("", "") // capability probe: dbPath irrelevant

	cc := benchmark.Spec{Configs: []benchmark.Config{{ID: "cc", Harness: "claude-code", Model: "claude-haiku-4-5"}}}
	codex := benchmark.Spec{Configs: []benchmark.Config{{ID: "cx", Harness: "codex", Model: "gpt-5"}}}
	mixed := benchmark.Spec{Configs: []benchmark.Config{
		{ID: "cx", Harness: "codex", Model: "gpt-5"},
		{ID: "cc", Harness: "claude-code", Model: "claude-haiku-4-5"},
	}}

	tests := []struct {
		name       string
		flag       bool
		configPath string
		spec       benchmark.Spec
		want       bool
	}{
		{name: "claude-code + no config → engage", spec: cc, want: true},
		{name: "mixed spec + no config → engage", spec: mixed, want: true},
		{name: "codex only + no config → no engage", spec: codex, want: false},
		{name: "claude-code but operator passed --config → no engage", spec: cc, configPath: "/tmp/my.toml", want: false},
		{name: "flag forces engage even for codex", spec: codex, flag: true, want: true},
		{name: "flag forces engage even with --config", spec: cc, flag: true, configPath: "/tmp/my.toml", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldEngageEphemeralDaemon(tc.flag, tc.configPath, tc.spec, drivers); got != tc.want {
				t.Errorf("shouldEngageEphemeralDaemon(flag=%v, cfg=%q) = %v, want %v", tc.flag, tc.configPath, got, tc.want)
			}
		})
	}
}

// --- isolated config shape ---

func TestWriteEphemeralBenchmarkConfigRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	cfgPath := filepath.Join(dir, "config.toml")
	if err := writeEphemeralBenchmarkConfig(cfgPath, dbPath, 54321); err != nil {
		t.Fatalf("writeEphemeralBenchmarkConfig: %v", err)
	}
	cfg, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("re-load written config: %v", err)
	}
	if cfg.Observer.DBPath != dbPath {
		t.Errorf("DBPath = %q, want %q", cfg.Observer.DBPath, dbPath)
	}
	if cfg.Proxy.Port != 54321 {
		t.Errorf("Proxy.Port = %d, want 54321", cfg.Proxy.Port)
	}
	// Empty allow-list must survive the load merge so the watcher idles instead
	// of scanning the operator's real dirs into the throwaway DB.
	if len(cfg.Observer.Watch.EnabledAdapters) != 0 {
		t.Errorf("EnabledAdapters = %v, want empty (watcher idle)", cfg.Observer.Watch.EnabledAdapters)
	}
	if cfg.Observer.Retention.PruneOnStartup {
		t.Errorf("PruneOnStartup = true, want false (ephemeral daemon must not prune)")
	}
	if cfg.CodeIntel.Enabled {
		t.Errorf("CodeIntel.Enabled = true, want false")
	}
}

// --- full lifecycle: create → ready → yield → teardown ---

func TestEphemeralDaemonLifecycle(t *testing.T) {
	// Not parallel: binds a real (free) listener + opens SQLite files.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	eph, err := startEphemeralDaemon(ctx)
	if err != nil {
		t.Fatalf("startEphemeralDaemon: %v", err)
	}

	// --- ready: the proxy accepts connections on a free loopback port ---
	addr := strings.TrimPrefix(eph.ProxyURL, "http://")
	conn, derr := net.DialTimeout("tcp", addr, 2*time.Second)
	if derr != nil {
		eph.Close()
		t.Fatalf("proxy not reachable at %s: %v", addr, derr)
	}
	_ = conn.Close()

	// --- yield: temp DB + config live under the temp dir, not the real one ---
	if !strings.HasPrefix(eph.DBPath, eph.Dir) || !strings.HasPrefix(eph.ConfigPath, eph.Dir) {
		t.Fatalf("expected DB/config under temp dir %q, got db=%q cfg=%q", eph.Dir, eph.DBPath, eph.ConfigPath)
	}
	if def, err := defaultObserverDBPath(); err == nil && sameDBPath(eph.DBPath, def) {
		t.Fatalf("ephemeral DB must NOT be the operator default %q", def)
	}
	if _, err := os.Stat(eph.DBPath); err != nil {
		t.Fatalf("ephemeral DB not created: %v", err)
	}

	// --- Preflight interplay: the fresh temp DB passes the isolation gate ---
	cfg, err := config.Load(config.LoadOptions{GlobalPath: eph.ConfigPath})
	if err != nil {
		eph.Close()
		t.Fatalf("load ephemeral config: %v", err)
	}
	d := newClaudeCodeDriver("", cfg.Observer.DBPath)
	if err := d.Preflight(); err != nil {
		eph.Close()
		t.Fatalf("claude-code Preflight against ephemeral daemon = %v, want nil", err)
	}

	// --- teardown: temp dir removed, port freed ---
	dir := eph.Dir
	eph.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("temp dir %q not removed after Close (stat err = %v)", dir, err)
	}
	// The listener is released — a fresh dial to the old addr must fail.
	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		_ = c.Close()
		t.Errorf("proxy port %s still accepting after Close — listener leaked", addr)
	}
}

// TestEphemeralDaemonCloseIsSafeOnNil guards the deferred-Close ergonomics.
func TestEphemeralDaemonCloseIsSafeOnNil(t *testing.T) {
	t.Parallel()
	var d *ephemeralDaemon
	d.Close() // must not panic
}
