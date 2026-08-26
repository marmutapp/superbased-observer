package cowork

import (
	"context"
	"testing"
)

// TestCacheObservations_FromFixture proves ParseSessionFile wires
// emitCacheObservationsTier2 end-to-end against the real
// testdata/cowork fixture, which carries a `result` record with TWO
// models in modelUsage (claude-opus-4-6 + claude-haiku-4-5-20251001
// — the same multi-model-per-turn shape
// TestParseSessionFile_ModelUsageEmitsTokenEventPerModel pins for
// TokenEvents). One CacheTurnObservation per model is expected, in
// the same sorted order the TokenEvent loop uses; only the first
// (alphabetically, "claude-haiku..." < "claude-opus...") carries the
// turn's accumulated BlockHashes since Accumulator.Emit drains the
// pending delta.
func TestCacheObservations_FromFixture(t *testing.T) {
	t.Parallel()
	root := fixturePath(t, "")
	auditPath := fixturePath(t, "cowork-aaaa/dev-bbbb/local_cccc-dddd-eeee/audit.jsonl")
	a := NewWithOptions(nil, root)

	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.CacheObservations) != 2 {
		t.Fatalf("CacheObservations=%d want 2 (have: %#v)", len(res.CacheObservations), res.CacheObservations)
	}

	haiku := res.CacheObservations[0]
	if haiku.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("obs[0].Model = %q, want claude-haiku-4-5-20251001", haiku.Model)
	}
	if haiku.Usage.NetInputTokens != 4500 || haiku.Usage.OutputTokens != 120 {
		t.Errorf("obs[0].Usage = %+v, want input=4500 output=120", haiku.Usage)
	}
	if haiku.Usage.CacheReadTokens != 0 || haiku.Usage.CacheCreationTokens != 0 {
		t.Errorf("obs[0].Usage cache fields = %+v, want both 0", haiku.Usage)
	}
	if len(haiku.BlockHashes) != 5 {
		t.Errorf("obs[0].BlockHashes = %d, want 5 (1 user text + 2 asst blocks + 1 tool_result + 1 asst text)", len(haiku.BlockHashes))
	}
	if haiku.SourceEventID != "cachetrack:result:ures-0001:claude-haiku-4-5-20251001" {
		t.Errorf("obs[0].SourceEventID = %q", haiku.SourceEventID)
	}

	opus := res.CacheObservations[1]
	if opus.Model != "claude-opus-4-6" {
		t.Errorf("obs[1].Model = %q, want claude-opus-4-6", opus.Model)
	}
	if opus.Usage.CacheReadTokens != 12000 {
		t.Errorf("obs[1].Usage.CacheReadTokens = %d, want 12000", opus.Usage.CacheReadTokens)
	}
	if opus.Usage.CacheCreationTokens != 2000 {
		t.Errorf("obs[1].Usage.CacheCreationTokens = %d, want 2000", opus.Usage.CacheCreationTokens)
	}
	// tier1hFrac derived from the result's top-level usage:
	// ephemeral_1h_input_tokens(1500) / cache_creation_input_tokens(2000) = 0.75.
	if opus.Usage.CacheCreation1hTokens != 1500 {
		t.Errorf("obs[1].Usage.CacheCreation1hTokens = %d, want 1500", opus.Usage.CacheCreation1hTokens)
	}
	if len(opus.BlockHashes) != 0 {
		t.Errorf("obs[1].BlockHashes = %d, want 0 (delta already drained by obs[0])", len(opus.BlockHashes))
	}
	if opus.SourceEventID != "cachetrack:result:ures-0001:claude-opus-4-6" {
		t.Errorf("obs[1].SourceEventID = %q", opus.SourceEventID)
	}
}

// TestCacheObservations_Idempotent pins re-parse stability (the R3
// byte-stability invariant): parsing the same file twice must
// produce identical observations (same SourceEventID, same
// canonical bytes, same count).
func TestCacheObservations_Idempotent(t *testing.T) {
	t.Parallel()
	root := fixturePath(t, "")
	auditPath := fixturePath(t, "cowork-aaaa/dev-bbbb/local_cccc-dddd-eeee/audit.jsonl")
	a := NewWithOptions(nil, root)

	first, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile (1st): %v", err)
	}
	second, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile (2nd): %v", err)
	}
	if len(first.CacheObservations) != len(second.CacheObservations) {
		t.Fatalf("counts differ: first=%d second=%d", len(first.CacheObservations), len(second.CacheObservations))
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
