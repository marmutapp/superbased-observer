package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// entry is a terse CacheEntryRow builder for the LoadLiveCacheWindows
// tests. expiresOffset is relative to t0Cache.
func cacheEntry(scope, session, hash, state string, expiresOffset time.Duration) CacheEntryRow {
	return CacheEntryRow{
		Model: "claude-opus-4-8", CacheScope: scope, SessionID: session,
		PrefixHash: hash, TokenCount: 20000, TTLTier: "5m", Tier: "proxy",
		CreatedAt:     t0Cache,
		LastRefreshAt: t0Cache,
		ExpiresAt:     t0Cache.Add(expiresOffset),
		State:         state,
	}
}

func TestLoadLiveCacheWindows_LiveAndUnverifiedReturned(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	rows := []CacheEntryRow{
		cacheEntry("scope-A", "sA", "h1", "live", 5*time.Minute),
		cacheEntry("scope-A", "sA", "h2", "unverified", 4*time.Minute),
		cacheEntry("scope-A", "sA", "h3", "invalidated", 5*time.Minute), // excluded
		cacheEntry("scope-A", "sA", "h4", "expired", -2*time.Minute),    // excluded (no grace)
	}
	if _, err := s.UpsertCacheEntries(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.LoadLiveCacheWindows(ctx, CacheWindowOpts{Now: t0Cache})
	if err != nil {
		t.Fatalf("LoadLiveCacheWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2 (live + unverified only)", len(got))
	}
	// ORDER BY expires_at ASC → h2 (4m) before h1 (5m).
	if got[0].PrefixHash != "h2" || got[1].PrefixHash != "h1" {
		t.Errorf("order = [%s, %s], want [h2, h1] (soonest expiry first)", got[0].PrefixHash, got[1].PrefixHash)
	}
}

func TestLoadLiveCacheWindows_ColdGraceIncludesRecentlyExpired(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	rows := []CacheEntryRow{
		cacheEntry("scope-A", "sA", "live1", "live", 5*time.Minute),
		cacheEntry("scope-A", "sA", "cold-recent", "expired", -1*time.Minute), // within 2m grace
		cacheEntry("scope-A", "sA", "cold-old", "expired", -10*time.Minute),   // outside grace
	}
	if _, err := s.UpsertCacheEntries(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := s.LoadLiveCacheWindows(ctx, CacheWindowOpts{Now: t0Cache, ColdGrace: 2 * time.Minute})
	if err != nil {
		t.Fatalf("LoadLiveCacheWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (live + recently-cold within grace)", len(got))
	}
	have := map[string]bool{}
	for _, r := range got {
		have[r.PrefixHash] = true
	}
	if !have["live1"] || !have["cold-recent"] || have["cold-old"] {
		t.Errorf("grace filter wrong: %+v", have)
	}
}

func TestLoadLiveCacheWindows_SessionFilterAndLimit(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	rows := []CacheEntryRow{
		cacheEntry("scope-A", "sA", "a1", "live", 5*time.Minute),
		cacheEntry("scope-A", "sA", "a2", "live", 6*time.Minute),
		cacheEntry("scope-B", "sB", "b1", "live", 5*time.Minute),
	}
	if _, err := s.UpsertCacheEntries(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Session filter.
	got, err := s.LoadLiveCacheWindows(ctx, CacheWindowOpts{Now: t0Cache, SessionID: "sA"})
	if err != nil {
		t.Fatalf("LoadLiveCacheWindows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("session filter: got %d, want 2", len(got))
	}
	for _, r := range got {
		if r.SessionID != "sA" {
			t.Errorf("session filter leaked %s", r.SessionID)
		}
	}

	// Limit.
	got, err = s.LoadLiveCacheWindows(ctx, CacheWindowOpts{Now: t0Cache, Limit: 1})
	if err != nil {
		t.Fatalf("LoadLiveCacheWindows limit: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("limit: got %d, want 1", len(got))
	}
}

// TestLoadLiveCacheWindows_StaleLiveRowsExcludedAndDoNotStarveLimit is the
// regression for the empty-status-surface bug: the engine only sweeps
// state→'expired' on an observed turn, so an idle session strands
// long-dead entries at state='live'. With expires_at ASC + a Limit, those
// stale rows sorted FIRST and consumed the whole limit, so a live session
// with genuinely-warm caches got an empty cache-status card. The query
// must gate recency on expires_at, not the state column.
func TestLoadLiveCacheWindows_StaleLiveRowsExcludedAndDoNotStarveLimit(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	var rows []CacheEntryRow
	// 60 long-dead entries the engine never swept off state='live'
	// (expired ~10 days ago). Under expires_at ASC these sort first.
	for i := 0; i < 60; i++ {
		rows = append(rows, cacheEntry("scope-A", "sA",
			fmt.Sprintf("dead%02d", i), "live", -240*time.Hour))
	}
	// 3 genuinely-warm caches expiring in the future.
	rows = append(
		rows,
		cacheEntry("scope-A", "sA", "warm1", "live", 50*time.Minute),
		cacheEntry("scope-A", "sA", "warm2", "live", 55*time.Minute),
		cacheEntry("scope-A", "sA", "warm3", "live", 59*time.Minute),
	)
	if _, err := s.UpsertCacheEntries(ctx, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Limit smaller than the dead pile — the pre-fix bug returned 50
	// dead rows and zero warm ones, so the status surface was empty.
	got, err := s.LoadLiveCacheWindows(ctx, CacheWindowOpts{Now: t0Cache, Limit: 50})
	if err != nil {
		t.Fatalf("LoadLiveCacheWindows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d windows, want 3 warm (stale live rows must be excluded, not starve the limit)", len(got))
	}
	for _, r := range got {
		if !r.ExpiresAt.After(t0Cache) {
			t.Errorf("returned a stale/expired entry %s (expires %v <= now)", r.PrefixHash, r.ExpiresAt)
		}
	}
}

// TestLoadImplicitCacheActivity covers the OpenAI/Codex window source:
// implicit_hit/implicit_write events collapse to the latest activity per
// (session, model), implicit_miss is ignored, and the prefix-at-risk is the
// latest row's larger token side.
func TestLoadImplicitCacheActivity(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)

	ins := func(session, model, kind string, off time.Duration, read, written int64) {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO cache_events(session_id, tier, timestamp, model, kind, tokens_read, tokens_written)
			 VALUES(?,?,?,?,?,?,?)`,
			session, "transcript", timestamp(now.Add(off)), model, kind, read, written); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Codex session: write then two hits; latest hit is the warm prefix.
	ins("codexA", "gpt-5.5", "implicit_write", -8*time.Minute, 0, 1200)
	ins("codexA", "gpt-5.5", "implicit_hit", -5*time.Minute, 40000, 0)
	ins("codexA", "gpt-5.5", "implicit_hit", -2*time.Minute, 50000, 300)
	// A miss must NOT count as warm evidence on its own.
	ins("codexB", "gpt-5.5", "implicit_miss", -1*time.Minute, 0, 0)
	// Ancient activity beyond the 24h ceiling is excluded.
	ins("codexC", "gpt-5.5", "implicit_hit", -30*time.Hour, 9999, 0)

	got, err := s.LoadImplicitCacheActivity(ctx, CacheWindowOpts{Now: now})
	if err != nil {
		t.Fatalf("LoadImplicitCacheActivity: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (codexA only; miss-only + ancient excluded)", len(got))
	}
	a := got[0]
	if a.SessionID != "codexA" {
		t.Errorf("session = %s, want codexA", a.SessionID)
	}
	if !a.LastActivity.Equal(now.Add(-2 * time.Minute)) {
		t.Errorf("last activity = %v, want %v (latest hit)", a.LastActivity, now.Add(-2*time.Minute))
	}
	if a.PrefixTokens != 50000 {
		t.Errorf("prefix tokens = %d, want 50000 (latest row, larger side)", a.PrefixTokens)
	}

	// Session filter.
	scoped, err := s.LoadImplicitCacheActivity(ctx, CacheWindowOpts{Now: now, SessionID: "codexB"})
	if err != nil {
		t.Fatalf("scoped: %v", err)
	}
	if len(scoped) != 0 {
		t.Errorf("codexB has only a miss → want 0, got %d", len(scoped))
	}
}

func TestLoadLiveCacheWindows_EmptyIsNotError(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	got, err := s.LoadLiveCacheWindows(context.Background(), CacheWindowOpts{Now: t0Cache})
	if err != nil {
		t.Fatalf("LoadLiveCacheWindows: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d, want 0", len(got))
	}
}
