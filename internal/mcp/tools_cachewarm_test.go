package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// cacheWarmServer builds a server with [cachewarm] enabled + a seeded
// live cache_entries row expiring soon, so cache_status has something to
// report. Returns the server.
func cacheWarmServer(t *testing.T, keepwarmMode string) *Server {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(dir, "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	now := time.Now().UTC()
	st := store.New(database)
	if _, err := st.UpsertCacheEntries(context.Background(), []store.CacheEntryRow{{
		Model: "claude-opus-4-8", CacheScope: "default", SessionID: "sess-A",
		PrefixHash: "h", TokenCount: 100000, TTLTier: "5m", Tier: "proxy",
		CreatedAt: now.Add(-time.Minute), LastRefreshAt: now.Add(-30 * time.Second),
		ExpiresAt: now.Add(20 * time.Second), State: "live",
	}}); err != nil {
		t.Fatalf("seed cache entry: %v", err)
	}

	cw := config.Default().CacheWarm
	cw.Keepwarm.Mode = keepwarmMode
	s, err := New(Options{DB: database, ServerName: "test", ServerVersion: "0", CacheWarm: cw})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	return s
}

func TestCacheStatusTool_ReportsExpiringCacheWithAdvice(t *testing.T) {
	s := cacheWarmServer(t, "advise")
	out := callTool(t, s, "cache_status", map[string]any{})

	if enabled, _ := out["enabled"].(bool); !enabled {
		t.Fatalf("expected enabled=true, got %+v", out)
	}
	if mode, _ := out["keepwarm_mode"].(string); mode != "advise" {
		t.Errorf("keepwarm_mode = %q, want advise", mode)
	}
	windows, _ := out["windows"].([]any)
	if len(windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(windows))
	}
	w := windows[0].(map[string]any)
	if sev, _ := w["severity"].(string); sev != "critical" {
		t.Errorf("severity = %q, want critical (20s left)", sev)
	}
	rec, _ := w["recommendation"].(map[string]any)
	if action, _ := rec["action"].(string); action != "use_1h_tier" {
		t.Errorf("recommendation.action = %q, want use_1h_tier", action)
	}
}

func TestCacheStatusTool_DisabledState(t *testing.T) {
	// Default Options leaves CacheWarm zero-valued (Enabled=false).
	s, _, _ := testServer(t)
	out := callTool(t, s, "cache_status", map[string]any{})
	if enabled, _ := out["enabled"].(bool); enabled {
		t.Errorf("expected enabled=false when [cachewarm] unset, got %+v", out)
	}
}
