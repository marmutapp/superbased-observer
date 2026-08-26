package muse

import "testing"

// TestCacheObservations_FromFixture proves ParseSessionFile wires
// emitCacheObservation end-to-end against the real simple-session.jsonl
// fixture, which carries 8 real model_completed records with non-zero
// cache_read_tokens on every one — see testdata/muse/README.md.
func TestCacheObservations_FromFixture(t *testing.T) {
	res, _ := parseFixture(t, "simple-session.jsonl")

	if len(res.TokenEvents) == 0 {
		t.Fatal("precondition: expected TokenEvents from the real fixture")
	}
	if len(res.CacheObservations) != len(res.TokenEvents) {
		t.Fatalf("CacheObservations = %d, want %d (one per TokenEvent — every model_completed record in this fixture carries non-zero usage)",
			len(res.CacheObservations), len(res.TokenEvents))
	}

	first := res.CacheObservations[0]
	if first.Usage.CacheReadTokens == 0 {
		t.Errorf("first observation CacheReadTokens = 0, want non-zero (fixture's first model_completed has cache_read_tokens=15665)")
	}
	if first.SourceEventID != "cachetrack:"+res.TokenEvents[0].SourceEventID {
		t.Errorf("SourceEventID = %q, want cachetrack:%s", first.SourceEventID, res.TokenEvents[0].SourceEventID)
	}
	if first.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if first.Model == "" {
		t.Error("Model is empty")
	}
	// The run's prompt (a "started" event) plus any assistant messages /
	// tool calls / tool results committed before the first model_completed
	// record should have joined the accumulator, so the first observation
	// carries at least one content block.
	if len(first.BlockHashes) == 0 {
		t.Error("first observation has no BlockHashes, want at least the run's prompt block")
	}
}

// TestCacheObservations_Idempotent pins re-parse stability: parsing the
// same fixture twice from offset 0 must reproduce byte-identical
// SourceEventIDs and canonical block bytes, since Muse's session.jsonl is
// append-only and each parse walks the same records in the same order.
func TestCacheObservations_Idempotent(t *testing.T) {
	first, _ := parseFixture(t, "simple-session.jsonl")
	second, _ := parseFixture(t, "simple-session.jsonl")

	if len(first.CacheObservations) != len(second.CacheObservations) {
		t.Fatalf("observation count changed across re-parse: %d vs %d", len(first.CacheObservations), len(second.CacheObservations))
	}
	for i := range first.CacheObservations {
		a, b := first.CacheObservations[i], second.CacheObservations[i]
		if a.SourceEventID != b.SourceEventID {
			t.Errorf("obs %d: SourceEventID changed: %q vs %q", i, a.SourceEventID, b.SourceEventID)
		}
		if len(a.BlockHashes) != len(b.BlockHashes) {
			t.Errorf("obs %d: BlockHashes len changed: %d vs %d", i, len(a.BlockHashes), len(b.BlockHashes))
			continue
		}
		for j := range a.BlockHashes {
			if string(a.BlockHashes[j].CanonicalBytes) != string(b.BlockHashes[j].CanonicalBytes) {
				t.Errorf("obs %d block %d: canonical bytes changed", i, j)
			}
		}
	}
}

// TestCacheObservations_MalformedFixtureSkipsInvalidLines proves the
// malformed fixture's unparseable/truncated lines are silently skipped
// (never crash the parser or fabricate a spurious observation) while its
// one genuinely valid model_completed record still produces exactly one
// CacheTurnObservation.
func TestCacheObservations_MalformedFixtureSkipsInvalidLines(t *testing.T) {
	res, _ := parseFixture(t, "malformed.jsonl")
	if len(res.CacheObservations) != 1 {
		t.Errorf("CacheObservations = %d, want 1 (the fixture's one valid model_completed record, no more, no less)", len(res.CacheObservations))
	}
}
