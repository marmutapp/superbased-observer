package store

import (
	"context"
	"testing"
	"time"
)

// TestSelectSessionCacheSummaries_AggregatesByBucket seeds cache_events for
// two sessions and asserts the (session_id, model, tier, kind, cause,
// zero_usage) bucketing: same-bucket events collapse into one row with
// summed counts/tokens, and a genuine zero-usage mispredict is distinguished
// from a real one.
func TestSelectSessionCacheSummaries_AggregatesByBucket(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	events := []CacheEventRow{
		// sA: two "hit" events in the same bucket — must collapse to one row
		// with Events=2 and summed tokens.
		{
			SessionID: "sA", Tier: "proxy", Timestamp: now.Add(-time.Hour),
			Model: "claude-opus-4-7", Kind: "hit", Cause: "suffix_growth",
			TokensRead: 9000, TokensWritten: 0,
		},
		{
			SessionID: "sA", Tier: "proxy", Timestamp: now.Add(-30 * time.Minute),
			Model: "claude-opus-4-7", Kind: "hit", Cause: "suffix_growth",
			TokensRead: 5000, TokensWritten: 0,
		},
		// sA: a real mispredict (non-zero usage) — distinct bucket from a
		// zero-usage one below.
		{
			SessionID: "sA", Tier: "proxy", Timestamp: now.Add(-20 * time.Minute),
			Model: "claude-opus-4-7", Kind: "mispredict", Cause: "unexpected_write",
			TokensRead: 100, TokensWritten: 50,
		},
		// sA: a zero-usage mispredict — must land in its own ZeroUsage=true
		// bucket, distinct from the real mispredict above even though both
		// share (model, kind, cause).
		{
			SessionID: "sA", Tier: "proxy", Timestamp: now.Add(-10 * time.Minute),
			Model: "claude-opus-4-7", Kind: "mispredict", Cause: "unexpected_write",
			TokensRead: 0, TokensWritten: 0,
		},
		// sB: a flagged rewrite cause on a different session — must not
		// bleed into sA's rows.
		{
			SessionID: "sB", Tier: "transcript", Timestamp: now.Add(-time.Hour),
			Model: "claude-sonnet-5", Kind: "invalidation_rewrite", Cause: "tools_changed",
			TokensRead: 200, TokensWritten: 40000,
		},
		// sC: outside the 7-day recompute window — must be excluded entirely.
		{
			SessionID: "sC", Tier: "proxy", Timestamp: now.AddDate(0, 0, -30),
			Model: "claude-opus-4-7", Kind: "hit", Cause: "suffix_growth",
			TokensRead: 1000, TokensWritten: 0,
		},
	}
	if _, err := s.InsertCacheEvents(ctx, events); err != nil {
		t.Fatalf("seed InsertCacheEvents: %v", err)
	}

	got, err := s.SelectSessionCacheSummaries(ctx)
	if err != nil {
		t.Fatalf("SelectSessionCacheSummaries: %v", err)
	}

	bySession := map[string]int{}
	for _, r := range got {
		bySession[r.SessionID]++
	}
	if bySession["sC"] != 0 {
		t.Errorf("sC has %d rows, want 0 (outside the recompute window)", bySession["sC"])
	}
	// sA: hit bucket (1) + real mispredict bucket (1) + zero-usage mispredict
	// bucket (1) = 3 rows.
	if bySession["sA"] != 3 {
		t.Errorf("sA rows = %d, want 3 (hit collapsed + 2 distinct mispredict buckets)", bySession["sA"])
	}
	if bySession["sB"] != 1 {
		t.Errorf("sB rows = %d, want 1", bySession["sB"])
	}

	var hitRow, realMispredict, zeroMispredict *struct {
		Events, Read, Written int64
		ZeroUsage             bool
	}
	for _, r := range got {
		if r.SessionID != "sA" {
			continue
		}
		switch {
		case r.Kind == "hit":
			hitRow = &struct {
				Events, Read, Written int64
				ZeroUsage             bool
			}{r.Events, r.TokensRead, r.TokensWritten, r.ZeroUsage}
		case r.Kind == "mispredict" && !r.ZeroUsage:
			realMispredict = &struct {
				Events, Read, Written int64
				ZeroUsage             bool
			}{r.Events, r.TokensRead, r.TokensWritten, r.ZeroUsage}
		case r.Kind == "mispredict" && r.ZeroUsage:
			zeroMispredict = &struct {
				Events, Read, Written int64
				ZeroUsage             bool
			}{r.Events, r.TokensRead, r.TokensWritten, r.ZeroUsage}
		}
	}
	if hitRow == nil {
		t.Fatal("no hit row found for sA")
	}
	if hitRow.Events != 2 || hitRow.Read != 14000 || hitRow.Written != 0 {
		t.Errorf("hit row = %+v, want Events=2 Read=14000 Written=0", *hitRow)
	}
	if realMispredict == nil {
		t.Fatal("no real (non-zero-usage) mispredict row found for sA")
	}
	if realMispredict.Events != 1 || realMispredict.Read != 100 || realMispredict.Written != 50 {
		t.Errorf("real mispredict row = %+v, want Events=1 Read=100 Written=50", *realMispredict)
	}
	if zeroMispredict == nil {
		t.Fatal("no zero-usage mispredict row found for sA")
	}
	if zeroMispredict.Events != 1 || zeroMispredict.Read != 0 || zeroMispredict.Written != 0 {
		t.Errorf("zero-usage mispredict row = %+v, want Events=1 Read=0 Written=0", *zeroMispredict)
	}

	for _, r := range got {
		if r.SessionID == "sB" {
			if r.Cause != "tools_changed" || r.Tier != "transcript" || r.Kind != "invalidation_rewrite" {
				t.Errorf("sB row = %+v, want cause=tools_changed tier=transcript kind=invalidation_rewrite", r)
			}
			if r.Events != 1 || r.TokensRead != 200 || r.TokensWritten != 40000 {
				t.Errorf("sB row totals = %+v, want Events=1 Read=200 Written=40000", r)
			}
		}
	}
}

// TestSelectSessionCacheSummaries_NoEvents returns an empty (never nil)
// slice when cache_events is empty.
func TestSelectSessionCacheSummaries_NoEvents(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	got, err := s.SelectSessionCacheSummaries(ctx)
	if err != nil {
		t.Fatalf("SelectSessionCacheSummaries: %v", err)
	}
	if got == nil {
		t.Error("got nil slice, want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0", len(got))
	}
}
