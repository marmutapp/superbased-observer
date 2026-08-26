package cline

import (
	"context"
	"testing"
)

// TestCacheObservations_FromFixture proves ParseSessionFile wires
// buildCacheObservations end-to-end against the real
// testdata/cline/api_conversation_history.json fixture (the same
// fixture TestParseClineTask pins for the TokenEvent path — that
// test asserts CacheReadTokens == 1500 on the single TokenEvent, so
// the single CacheTurnObservation this produces must carry the same
// count).
func TestCacheObservations_FromFixture(t *testing.T) {
	t.Parallel()
	path := copyFixture(t, "abc123")
	a := New()

	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.CacheObservations) != 1 {
		t.Fatalf("CacheObservations: %d want 1", len(res.CacheObservations))
	}
	obs := res.CacheObservations[0]
	if obs.SessionID != "abc123" {
		t.Errorf("SessionID = %q, want abc123", obs.SessionID)
	}
	if obs.Usage.CacheReadTokens != 1500 {
		t.Errorf("CacheReadTokens = %d, want 1500", obs.Usage.CacheReadTokens)
	}
	if obs.SourceFile != path {
		t.Errorf("SourceFile = %q, want %q", obs.SourceFile, path)
	}
}

// TestCacheObservations_Idempotent pins re-parse stability (the R3
// byte-stability invariant): parsing the same file twice must
// produce identical observations (same SourceEventID, same
// canonical bytes).
func TestCacheObservations_Idempotent(t *testing.T) {
	t.Parallel()
	path := copyFixture(t, "def456")
	a := New()

	first, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile (1st): %v", err)
	}
	second, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile (2nd): %v", err)
	}
	if len(first.CacheObservations) != len(second.CacheObservations) {
		t.Fatalf("observation count changed: %d vs %d", len(first.CacheObservations), len(second.CacheObservations))
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
