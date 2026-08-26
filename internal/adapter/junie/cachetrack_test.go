package junie

import "testing"

// TestCacheObservations_FromFixture proves ParseSessionFile wires
// emitCacheObservations end-to-end against the real
// testdata/junie/session-260816-220304-lrfz/events.jsonl fixture, which
// TestParseFixtureCounts already pins to 22 TokenEvents. Every one of the
// fixture's 22 LlmResponseMetadataEvent records carries a non-zero
// modelUsage entry (confirmed by direct inspection of the fixture), so
// exactly 22 CacheTurnObservations must come out too, in the same order as
// their sibling TokenEvents.
//
// Nine of those observations carry a non-empty content-block delta —
// one per Tier-2-accumulating event in the fixture (1 user prompt, 2
// thoughts, 3 distinct Terminal blocks' first COMPLETED/FAILED
// transition, 2 distinct FileChanges blocks' first COMPLETED transition,
// and 1 Result block) — drained by whichever LlmResponseMetadataEvent
// comes next after each. The completion-rebroadcast occurrences (lines
// 207-212 of the fixture) must NOT re-accumulate: obs[21] (the final
// observation, drained after the rebroadcast burst) proves this by
// carrying only the Result block's single delta, not a re-accumulation
// of all five already-drained blocks.
func TestCacheObservations_FromFixture(t *testing.T) {
	t.Parallel()
	res, path := parseFixture(t)

	if len(res.TokenEvents) != 22 {
		t.Fatalf("precondition: got %d TokenEvents, want 22", len(res.TokenEvents))
	}
	if len(res.CacheObservations) != 22 {
		t.Fatalf("CacheObservations = %d, want 22 (one per non-zero-usage modelUsage entry)", len(res.CacheObservations))
	}

	// obs[0]: the very first LLM call, drains the single accumulated user
	// prompt block.
	obs0 := res.CacheObservations[0]
	if obs0.Model != "gpt-4.1-2025-04-14" {
		t.Errorf("obs[0].Model = %q, want gpt-4.1-2025-04-14", obs0.Model)
	}
	if obs0.Usage.NetInputTokens != 1138 {
		t.Errorf("obs[0].Usage.NetInputTokens = %d, want 1138", obs0.Usage.NetInputTokens)
	}
	if obs0.Usage.OutputTokens != 7 {
		t.Errorf("obs[0].Usage.OutputTokens = %d, want 7", obs0.Usage.OutputTokens)
	}
	if obs0.Usage.CacheReadTokens != 0 || obs0.Usage.CacheCreationTokens != 0 {
		t.Errorf("obs[0].Usage cache fields = (%d, %d), want (0, 0)", obs0.Usage.CacheReadTokens, obs0.Usage.CacheCreationTokens)
	}
	if len(obs0.BlockHashes) != 1 {
		t.Errorf("obs[0].BlockHashes = %d, want 1 (the user prompt block)", len(obs0.BlockHashes))
	}
	if obs0.SourceFile != path {
		t.Errorf("obs[0].SourceFile = %q, want %q", obs0.SourceFile, path)
	}
	// SourceEventID linkage to the sibling TokenEvent is checked in the
	// loop below (message-ids are keyed by byte offset, not line number).

	// obs[9]: the first Terminal block's FAILED transition (line 68 of the
	// fixture) drains into the immediately-following LLM call.
	obs9 := res.CacheObservations[9]
	if obs9.Usage.NetInputTokens != 3 || obs9.Usage.OutputTokens != 206 {
		t.Errorf("obs[9].Usage = (in=%d, out=%d), want (3, 206)", obs9.Usage.NetInputTokens, obs9.Usage.OutputTokens)
	}
	if obs9.Usage.CacheReadTokens != 13269 || obs9.Usage.CacheCreationTokens != 647 {
		t.Errorf("obs[9].Usage cache fields = (%d, %d), want (13269, 647)", obs9.Usage.CacheReadTokens, obs9.Usage.CacheCreationTokens)
	}
	if len(obs9.BlockHashes) != 1 {
		t.Errorf("obs[9].BlockHashes = %d, want 1 (the failed Terminal block)", len(obs9.BlockHashes))
	}

	// obs[21]: the last observation, drained after the completion
	// rebroadcast burst (lines 207-212). Only the Result block (line 202,
	// creation branch) contributes — the five rebroadcast occurrences at
	// 207-211 must be no-ops (cacheDone already set), and the Result
	// block's own update-in-place occurrence at 212 must also be a no-op
	// (only its creation branch accumulates).
	obs21 := res.CacheObservations[21]
	if obs21.Model != "gpt-4.1-mini-2025-04-14" {
		t.Errorf("obs[21].Model = %q, want gpt-4.1-mini-2025-04-14", obs21.Model)
	}
	if obs21.Usage.NetInputTokens != 841 || obs21.Usage.OutputTokens != 5 {
		t.Errorf("obs[21].Usage = (in=%d, out=%d), want (841, 5)", obs21.Usage.NetInputTokens, obs21.Usage.OutputTokens)
	}
	if len(obs21.BlockHashes) != 1 {
		t.Errorf("obs[21].BlockHashes = %d, want 1 (the Result block, not a 5-block rebroadcast)", len(obs21.BlockHashes))
	}

	// Total block deltas across every observation must equal 9: the 9
	// distinct accumulating events in the fixture (1 prompt + 2 thoughts +
	// 3 Terminal completions + 2 FileChanges completions + 1 Result),
	// each counted exactly once despite the completion-rebroadcast.
	totalBlocks := 0
	for _, o := range res.CacheObservations {
		totalBlocks += len(o.BlockHashes)
	}
	if totalBlocks != 9 {
		t.Errorf("sum of BlockHashes across all observations = %d, want 9", totalBlocks)
	}

	// Every emitted observation shares the same message-id linkage as its
	// sibling TokenEvent (both keyed off "llm:<lineStart>:<i>").
	for i := range res.CacheObservations {
		want := "cachetrack:" + res.TokenEvents[i].SourceEventID
		if res.CacheObservations[i].SourceEventID != want {
			t.Errorf("obs[%d].SourceEventID = %q, want %q (TokenEvents[%d].SourceEventID = %q)",
				i, res.CacheObservations[i].SourceEventID, want, i, res.TokenEvents[i].SourceEventID)
		}
	}
}

// TestCacheObservations_Idempotent pins re-parse stability (the R3
// byte-stability invariant): re-parsing the same fixture from offset 0
// twice must produce identical observations — the same determinism
// TestIncrementalParseMatchesWhole already pins for ToolEvents/TokenEvents.
func TestCacheObservations_Idempotent(t *testing.T) {
	t.Parallel()
	first, _ := parseFixture(t)
	second, _ := parseFixture(t)

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
