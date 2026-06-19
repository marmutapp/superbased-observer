package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestAcquireProcessGuard_SharedPerDBPath pins the daemon-wide guard
// invariant (G9): the proxy build and the watcher build — separate
// Store handles over the same observer.db — must receive the SAME
// Guard instance, so proxy-marked taint is visible to the watcher
// ingest seam's T-5xx rules. Distinct DB paths (distinct daemons in
// one test process) get distinct instances; disabled/off configs get
// nil.
func TestAcquireProcessGuard_SharedPerDBPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	openStore := func(path string) *store.Store {
		t.Helper()
		database, err := db.Open(ctx, db.Options{Path: path})
		if err != nil {
			t.Fatalf("db.Open: %v", err)
		}
		t.Cleanup(func() { _ = database.Close() })
		return store.New(database)
	}
	cfgFor := func(dbPath string) config.Config {
		cfg := config.Default()
		cfg.Observer.DBPath = dbPath
		return cfg
	}

	pathA := filepath.Join(t.TempDir(), "observer.db")
	stProxy := openStore(pathA)
	stWatcher := openStore(pathA)

	g1 := acquireProcessGuard(ctx, cfgFor(pathA), stProxy, logger)
	if g1 == nil {
		t.Fatal("acquireProcessGuard returned nil for an enabled config")
	}
	g2 := acquireProcessGuard(ctx, cfgFor(pathA), stWatcher, logger)
	if g1 != g2 {
		t.Fatal("two acquires over the same observer.db returned distinct Guards — taint state would split between proxy and watcher")
	}

	pathB := filepath.Join(t.TempDir(), "observer.db")
	g3 := acquireProcessGuard(ctx, cfgFor(pathB), openStore(pathB), logger)
	if g3 == nil || g3 == g1 {
		t.Fatal("a different observer.db must get its own Guard instance")
	}

	off := cfgFor(filepath.Join(t.TempDir(), "observer.db"))
	off.Guard.Mode = "off"
	if g := acquireProcessGuard(ctx, off, stProxy, logger); g != nil {
		t.Fatal("mode=off must not construct a Guard")
	}
	disabled := cfgFor(filepath.Join(t.TempDir(), "observer.db"))
	disabled.Guard.Enabled = false
	if g := acquireProcessGuard(ctx, disabled, stProxy, logger); g != nil {
		t.Fatal("enabled=false must not construct a Guard")
	}
}
