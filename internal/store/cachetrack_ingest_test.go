package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/cachetrack"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestIngest_CacheEngine_WritesRowsAndIsIdempotent is the C10
// load-bearing test: with a cachetrack.Engine wired via
// SetCacheEngine, Ingest's CacheObservations slice flows through
// the engine and lands cache_segments / cache_entries /
// cache_events rows. A second Ingest of the same observations is
// a no-op — the CacheEventExistsForMessage dedup gate fires per
// observation, skipping engine.ObserveTurn (so internal
// CacheModel state stays consistent) and skipping persistence.
//
// This is the spec §12 backfill invariant ("idempotent re-runs")
// at the store layer; the --cache-rescan flag triggers the same
// code path via watcher Rescan.
func TestIngest_CacheEngine_WritesRowsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	s.SetCacheEngine(cachetrack.NewEngine(8))
	ctx := context.Background()

	canon := func(s string) []byte { return []byte(s) }
	obs := []models.CacheTurnObservation{
		{
			SourceFile:    "/transcripts/sA.jsonl",
			SourceEventID: "evt-1",
			SessionID:     "sA",
			MessageID:     "msg_aaa",
			Timestamp:     t0Cache,
			Model:         "claude-opus-4-8",
			BlockHashes: []models.CacheBlockMeta{
				{LevelLabel: "system", Kind: "text", CanonicalBytes: canon(`{"text":"system","type":"text"}`), Role: "system"},
				{LevelLabel: "message", Kind: "text", CanonicalBytes: canon(`{"content":"hi","role":"user","type":"text"}`), Role: "user"},
			},
			Usage: models.CacheUsage{
				CacheCreationTokens: 5000,
			},
		},
		{
			SourceFile:    "/transcripts/sA.jsonl",
			SourceEventID: "evt-2",
			SessionID:     "sA",
			MessageID:     "msg_bbb",
			Timestamp:     t0Cache.Add(10 * time.Second),
			Model:         "claude-opus-4-8",
			BlockHashes: []models.CacheBlockMeta{
				{LevelLabel: "system", Kind: "text", CanonicalBytes: canon(`{"text":"system","type":"text"}`), Role: "system"},
				{LevelLabel: "message", Kind: "text", CanonicalBytes: canon(`{"content":"hi","role":"user","type":"text"}`), Role: "user"},
				{LevelLabel: "message", Kind: "text", CanonicalBytes: canon(`{"content":"ack","role":"assistant","type":"text"}`), Role: "assistant"},
				{LevelLabel: "message", Kind: "text", CanonicalBytes: canon(`{"content":"more","role":"user","type":"text"}`), Role: "user"},
			},
			Usage: models.CacheUsage{
				CacheReadTokens:     4500,
				CacheCreationTokens: 600,
			},
		},
	}

	// First ingest: emits Tier-2 cache rows.
	if _, err := s.Ingest(ctx, nil, nil, IngestOptions{CacheObservations: obs}); err != nil {
		t.Fatalf("first Ingest: %v", err)
	}

	seg1 := countRows(t, ctx, s, "cache_segments")
	evt1 := countRows(t, ctx, s, "cache_events")
	ent1 := countRows(t, ctx, s, "cache_entries")
	if evt1 == 0 {
		t.Fatalf("expected cache_events > 0 on first ingest; got 0 (engine wire dead?)")
	}
	if seg1 == 0 {
		t.Errorf("expected cache_segments > 0; got 0")
	}
	if ent1 == 0 {
		t.Errorf("expected cache_entries > 0; got 0")
	}

	// Second ingest with the SAME observations: dedup gate must
	// fire on every msg_id, so row counts stay identical.
	if _, err := s.Ingest(ctx, nil, nil, IngestOptions{CacheObservations: obs}); err != nil {
		t.Fatalf("second Ingest: %v", err)
	}
	seg2 := countRows(t, ctx, s, "cache_segments")
	evt2 := countRows(t, ctx, s, "cache_events")
	ent2 := countRows(t, ctx, s, "cache_entries")
	if seg2 != seg1 {
		t.Errorf("cache_segments doubled on idempotent re-run: %d → %d (dedup gate dead?)", seg1, seg2)
	}
	if evt2 != evt1 {
		t.Errorf("cache_events doubled on idempotent re-run: %d → %d", evt1, evt2)
	}
	if ent2 != ent1 {
		t.Errorf("cache_entries grew on idempotent re-run: %d → %d (UNIQUE-on-(model,scope,prefix_hash) dead?)", ent1, ent2)
	}
}

// TestIngest_CacheEngine_NilEngineIsNoOp pins that an unwired
// engine (default Store state, or explicit SetCacheEngine(nil))
// leaves the existing C6 no-op behavior intact — no rows written
// even when observations are populated. Guards against an
// accidental "always emit" regression.
func TestIngest_CacheEngine_NilEngineIsNoOp(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	// Explicit nil — equivalent to the default zero state.
	s.SetCacheEngine(nil)
	ctx := context.Background()

	obs := []models.CacheTurnObservation{
		{
			SourceFile:    "/transcripts/sA.jsonl",
			SourceEventID: "evt-1",
			SessionID:     "sA",
			MessageID:     "msg_aaa",
			Timestamp:     t0Cache,
			Model:         "claude-opus-4-8",
			BlockHashes: []models.CacheBlockMeta{
				{LevelLabel: "message", Kind: "text", CanonicalBytes: []byte(`{"x":1}`), Role: "user"},
			},
			Usage: models.CacheUsage{CacheCreationTokens: 100},
		},
	}
	if _, err := s.Ingest(ctx, nil, nil, IngestOptions{CacheObservations: obs}); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"cache_segments", "cache_events", "cache_entries"} {
		if n := countRows(t, ctx, s, tbl); n != 0 {
			t.Errorf("%s = %d, want 0 (nil engine must be no-op)", tbl, n)
		}
	}
}

func countRows(t *testing.T, ctx context.Context, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}
