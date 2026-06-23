package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestIngest_CountsCacheObservations covers the C6 plumbing:
// IngestOptions.CacheObservations is threaded through to
// IngestResult.CacheObservationsSeen so the watcher → Ingest
// path is testable end-to-end before C7 wires the engine. Per
// the spec: "additive only — adapters that don't populate it
// leave it nil; the watcher / store pass-through silently no-
// ops on an empty slice." Three cases: nil (no-op), empty
// slice (no-op), populated (counted).
func TestIngest_CountsCacheObservations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		obs  []models.CacheTurnObservation
		want int
	}{
		{"nil → no-op", nil, 0},
		{"empty → no-op", []models.CacheTurnObservation{}, 0},
		{
			name: "populated → counted",
			obs: []models.CacheTurnObservation{
				{
					SourceFile:    "/x.jsonl",
					SourceEventID: "evt-1",
					SessionID:     "sA",
					MessageID:     "msg_a",
					Timestamp:     t0Cache,
					Model:         "claude-opus-4-7",
					Fast:          false,
					Usage:         models.CacheUsage{CacheReadTokens: 1000},
				},
				{
					SourceFile:    "/x.jsonl",
					SourceEventID: "evt-2",
					SessionID:     "sA",
					MessageID:     "msg_b",
					Timestamp:     t0Cache.Add(time.Second),
					Model:         "claude-opus-4-7",
				},
			},
			want: 2,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestStore(t)
			ctx := context.Background()

			got, err := s.Ingest(ctx, nil, nil, IngestOptions{
				CacheObservations: tt.obs,
			})
			if err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if got.CacheObservationsSeen != tt.want {
				t.Errorf("CacheObservationsSeen = %d, want %d", got.CacheObservationsSeen, tt.want)
			}
			// Confirm pass-through doesn't accidentally write cache_* rows
			// (C6 contract — no emitter yet).
			var segN, evtN, entN int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_segments`).Scan(&segN); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_events`).Scan(&evtN); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_entries`).Scan(&entN); err != nil {
				t.Fatal(err)
			}
			if segN != 0 || evtN != 0 || entN != 0 {
				t.Errorf("C6 contract violated: cache_segments=%d cache_events=%d cache_entries=%d, want all 0 (no emitter yet)", segN, evtN, entN)
			}
		})
	}
}

// TestCacheTurnObservation_ShapeStable pins the public field
// surface of the models.CacheTurnObservation type. Adding fields
// is permitted; renaming/removing is a breaking change for the
// 17 adapters that may populate it in C7+. The test reads each
// field by name; a rename fails compile.
func TestCacheTurnObservation_ShapeStable(t *testing.T) {
	t.Parallel()
	o := models.CacheTurnObservation{
		SourceFile:    "/x.jsonl",
		SourceEventID: "evt-1",
		SessionID:     "sA",
		MessageID:     "msg_a",
		Timestamp:     t0Cache,
		Model:         "claude-opus-4-7",
		Fast:          true,
		BlockHashes: []models.CacheBlockMeta{
			{LevelLabel: "message", Kind: "text", CanonicalBytes: []byte(`{"t":"hi"}`), Role: "user"},
		},
		Usage: models.CacheUsage{
			NetInputTokens:        100,
			OutputTokens:          50,
			CacheReadTokens:       9000,
			CacheCreationTokens:   500,
			CacheCreation1hTokens: 500,
		},
		CompactionSeen: false,
	}
	// Trivially read every field — guard against rename.
	if o.SourceFile == "" || o.SourceEventID == "" || o.SessionID == "" || o.MessageID == "" {
		t.Error("required string fields missing")
	}
	if o.Timestamp.IsZero() || o.Model == "" {
		t.Error("Timestamp / Model required")
	}
	if !o.Fast {
		t.Error("Fast did not round-trip")
	}
	if len(o.BlockHashes) != 1 || o.BlockHashes[0].LevelLabel != "message" || o.BlockHashes[0].Role != "user" {
		t.Error("BlockHashes shape regression")
	}
	if string(o.BlockHashes[0].CanonicalBytes) != `{"t":"hi"}` {
		t.Error("CanonicalBytes round-trip failed")
	}
	if o.Usage.CacheCreation1hTokens != 500 {
		t.Error("CacheUsage.CacheCreation1hTokens regression")
	}
	if o.CompactionSeen {
		t.Error("CompactionSeen default regression")
	}
}
