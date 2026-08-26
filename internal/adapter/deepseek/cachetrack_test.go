package deepseek

import "testing"

// TestCacheObservations_FromFixture proves ParseSessionFile wires
// emitCacheObservation end-to-end against the real testdata/deepseek
// session.jsonl.zstd fixture, which TestParseSessionFileHappyPath
// already pins to 3 TokenEvents — row index 1 (cacheRow) carries
// CacheReadTokens=7680 with InputTokens=4011 (proving InputTokens is
// already NET). Every non-zero-usage assistant/message should produce
// exactly one CacheTurnObservation, in TokenEvent order, and
// CacheCreationTokens must stay 0 throughout — DeepSeek Harness never
// reports cache-creation tokens on the wire.
func TestCacheObservations_FromFixture(t *testing.T) {
	t.Parallel()
	a, path := fixture(t, "session.jsonl.zstd")
	res := parse(t, a, path, 0)

	if len(res.TokenEvents) != 3 {
		t.Fatalf("precondition: got %d TokenEvents, want 3", len(res.TokenEvents))
	}
	if len(res.CacheObservations) != 3 {
		t.Fatalf("CacheObservations = %d, want 3 (one per non-zero-usage assistant/message)", len(res.CacheObservations))
	}

	cacheObs := res.CacheObservations[1]
	if cacheObs.SessionID != "session-019f1111-2222-7333-8444-555555555555" {
		t.Errorf("obs[1].SessionID = %q", cacheObs.SessionID)
	}
	if cacheObs.Usage.NetInputTokens != 4011 {
		t.Errorf("obs[1].Usage.NetInputTokens = %d, want 4011", cacheObs.Usage.NetInputTokens)
	}
	if cacheObs.Usage.OutputTokens != 96 {
		t.Errorf("obs[1].Usage.OutputTokens = %d, want 96", cacheObs.Usage.OutputTokens)
	}
	if cacheObs.Usage.CacheReadTokens != 7680 {
		t.Errorf("obs[1].Usage.CacheReadTokens = %d, want 7680", cacheObs.Usage.CacheReadTokens)
	}
	if cacheObs.Usage.CacheCreationTokens != 0 {
		t.Errorf("obs[1].Usage.CacheCreationTokens = %d, want 0 (never reported by DeepSeek Harness)", cacheObs.Usage.CacheCreationTokens)
	}
	if cacheObs.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("obs[1].Model = %q, want deepseek/deepseek-v4-flash", cacheObs.Model)
	}
	if cacheObs.SourceFile != path {
		t.Errorf("obs[1].SourceFile = %q, want %q", cacheObs.SourceFile, path)
	}

	// Every emitted observation shares the same source-event-id shape as
	// its sibling TokenEvent (both keyed off env.Seq via "tok:<seq>").
	for i := range res.CacheObservations {
		if res.CacheObservations[i].SourceEventID != "cachetrack:tok:"+res.TokenEvents[i].SourceEventID[len("tok:"):] {
			t.Errorf("obs[%d].SourceEventID = %q, TokenEvents[%d].SourceEventID = %q: message-id linkage broken",
				i, res.CacheObservations[i].SourceEventID, i, res.TokenEvents[i].SourceEventID)
		}
	}
}

// TestCacheObservations_Idempotent pins re-parse stability (the R3
// byte-stability invariant): re-parsing the same whole-file-rewrite
// fixture twice must produce identical observations — the same
// determinism TestParseSessionFileWholeFileRescanIsDeterministic already
// pins for ToolEvents/TokenEvents.
func TestCacheObservations_Idempotent(t *testing.T) {
	t.Parallel()
	a, path := fixture(t, "session.jsonl.zstd")

	first := parse(t, a, path, 0)
	second := parse(t, a, path, 0)

	if len(first.CacheObservations) != len(second.CacheObservations) {
		t.Fatalf("observation count changed across re-parses: %d vs %d", len(first.CacheObservations), len(second.CacheObservations))
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

// TestCacheObservations_AbortedTurnStillEmits proves the aborted-turn
// fixture (which still has one non-zero-usage assistant/message before
// the abort) also produces a CacheTurnObservation, mirroring its single
// TokenEvent.
func TestCacheObservations_AbortedTurnStillEmits(t *testing.T) {
	t.Parallel()
	a, path := fixture(t, "session-aborted.jsonl.zstd")
	res := parse(t, a, path, 0)

	if len(res.TokenEvents) != 1 {
		t.Fatalf("precondition: got %d TokenEvents, want 1", len(res.TokenEvents))
	}
	if len(res.CacheObservations) != 1 {
		t.Fatalf("CacheObservations = %d, want 1", len(res.CacheObservations))
	}
	if res.CacheObservations[0].Model != "deepseek/deepseek-v4-pro" {
		t.Errorf("obs[0].Model = %q, want deepseek/deepseek-v4-pro", res.CacheObservations[0].Model)
	}
}
