package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func newCacheStatusServer(t *testing.T, cw config.CacheWarmConfig, seed bool) *Server {
	t.Helper()
	tdir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	if seed {
		now := time.Now().UTC()
		if _, err := store.New(database).UpsertCacheEntries(context.Background(), []store.CacheEntryRow{{
			Model: "claude-opus-4-8", CacheScope: "default", SessionID: "sA",
			PrefixHash: "h", TokenCount: 100000, TTLTier: "5m", Tier: "proxy",
			CreatedAt: now.Add(-time.Minute), LastRefreshAt: now.Add(-30 * time.Second),
			ExpiresAt: now.Add(20 * time.Second), State: "live",
		}}); err != nil {
			t.Fatal(err)
		}
	}

	server, err := New(Options{DB: database, CacheWarm: cw})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestHandleCacheStatus_ReportsExpiring(t *testing.T) {
	cw := config.Default().CacheWarm
	cw.Keepwarm.Mode = "advise"
	s := newCacheStatusServer(t, cw, true)

	rr := httptest.NewRecorder()
	s.handleCacheStatus(rr, httptest.NewRequest(http.MethodGet, "/api/cache/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp CacheStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled || resp.KeepwarmMode != "advise" {
		t.Fatalf("enabled/mode wrong: %+v", resp)
	}
	if len(resp.Windows) != 1 {
		t.Fatalf("got %d windows, want 1", len(resp.Windows))
	}
	if resp.Windows[0].Severity != "critical" {
		t.Errorf("severity = %q, want critical", resp.Windows[0].Severity)
	}
	if resp.Windows[0].Recommendation.Action != "use_1h_tier" {
		t.Errorf("recommendation = %q, want use_1h_tier", resp.Windows[0].Recommendation.Action)
	}
}

func TestHandleCacheStatus_DisabledWhenConfigOff(t *testing.T) {
	// Zero-value CacheWarm → Enabled false.
	s := newCacheStatusServer(t, config.CacheWarmConfig{}, false)
	rr := httptest.NewRecorder()
	s.handleCacheStatus(rr, httptest.NewRequest(http.MethodGet, "/api/cache/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp CacheStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Errorf("expected disabled when config off")
	}
}
