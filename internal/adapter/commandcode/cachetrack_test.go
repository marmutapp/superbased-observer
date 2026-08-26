package commandcode

import "testing"

// TestCacheObservations_FromFixture proves ParseSessionFile wires
// emitCacheObservation end-to-end against the real session-sample.jsonl
// fixture, which carries non-zero cacheReadTokens on multiple assistant
// usage envelopes (see testdata/commandcode/session-sample.jsonl).
func TestCacheObservations_FromFixture(t *testing.T) {
	res, _ := stageFixture(t, "session-sample.jsonl")

	if len(res.TokenEvents) == 0 {
		t.Fatal("precondition: expected TokenEvents from the real fixture")
	}
	if len(res.CacheObservations) == 0 {
		t.Fatal("CacheObservations is empty, want at least one (the fixture has assistant usage envelopes with non-zero cacheReadTokens)")
	}

	first := res.CacheObservations[0]
	if first.Usage.CacheReadTokens == 0 {
		t.Errorf("first observation CacheReadTokens = 0, want non-zero (fixture's first assistant usage has cacheReadTokens=7424)")
	}
	if first.SessionID == "" {
		t.Error("SessionID is empty")
	}
	if first.Model == "" {
		t.Error("Model is empty")
	}
	if len(first.BlockHashes) == 0 {
		t.Error("first observation has no BlockHashes, want at least one content block")
	}

	// Cross-check against the sibling TokenEvent stream: each cache
	// observation's SourceEventID must be the cachetrack:-prefixed form of
	// some TokenEvent's own SourceEventID.
	tokenIDs := make(map[string]bool, len(res.TokenEvents))
	for _, te := range res.TokenEvents {
		tokenIDs["cachetrack:"+te.SourceEventID] = true
	}
	for i, obs := range res.CacheObservations {
		if !tokenIDs[obs.SourceEventID] {
			t.Errorf("observation %d SourceEventID %q has no matching TokenEvent", i, obs.SourceEventID)
		}
	}
}

// TestCacheObservations_Idempotent pins re-parse stability: parsing the
// same fixture twice from offset 0 must reproduce byte-identical
// SourceEventIDs and canonical block bytes, since a command-code session
// transcript is append-only and each parse walks the same records in the
// same order.
func TestCacheObservations_Idempotent(t *testing.T) {
	first, _ := stageFixture(t, "session-sample.jsonl")
	second, _ := stageFixture(t, "session-sample.jsonl")

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

// TestCacheObservations_WindowsFixture proves the same wiring holds for
// the Windows-path fixture (distinct slug encoding, same message shape).
func TestCacheObservations_WindowsFixture(t *testing.T) {
	res, _ := stageFixture(t, "windows-session-sample.jsonl")

	if len(res.CacheObservations) == 0 {
		t.Fatal("CacheObservations is empty, want at least one")
	}
	for i, obs := range res.CacheObservations {
		if obs.SessionID == "" {
			t.Errorf("obs %d: SessionID is empty", i)
		}
	}
}
