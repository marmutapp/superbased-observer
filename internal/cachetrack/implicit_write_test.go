package cachetrack

import (
	"testing"
	"time"
)

// TestObserveTurn_ImplicitCache_ThreadsWriteCount is the end-to-end pin for
// the GPT-5.6 metered-lane fix (2026-07-16): a nonzero cache_write_tokens
// (parsed at internal/proxy into CacheUsageObserved.CacheCreationTokens) must
// reach the emitted implicit cache_events row as TokensWritten, so dashboards
// / advisor / cachewarm stop underreporting implicit writes. Before the fix
// observeImplicit hardcoded TokensWritten:0 and the pass-through was dropped.
//
// This drives the REAL ObserveTurn dispatch (not just ObserveInput
// construction). EventOut maps 1:1 to the persisted cache_events row via
// store.PersistCacheObservation (row.TokensWritten = ev.TokensWritten), so
// asserting the EventOut proves the persisted row carries the write count.
// TokensWritten1h stays 0 — OpenAI writes are untiered (no 1h subset).
func TestObserveTurn_ImplicitCache_ThreadsWriteCount(t *testing.T) {
	t.Parallel()
	eng := NewEngine(0)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	caps := Capabilities{
		UsageObserved:       true,
		BlocksAreCumulative: true,
		ImplicitCache:       true,
	}

	// Metered-lane bootstrap write turn: cold read, real cache_write_tokens.
	res := eng.ObserveTurn(ObserveInput{
		SessionID: "sess-implicit-write",
		Model:     "gpt-5.6-sol",
		Scope:     "default",
		Tier:      TierProxy,
		Caps:      caps,
		MessageID: "req-write-1",
		Now:       now,
		Usage: CacheUsageObserved{
			NetInputTokens:  2048,
			OutputTokens:    100,
			CacheReadTokens: 0, // cold on the bootstrap write turn
			// cache_write_tokens threaded from the proxy.
			CacheCreationTokens: 641,
			// Even if a bogus 1h value arrived it must NOT survive on the
			// implicit lane — OpenAI writes are untiered.
			CacheCreation1hTokens: 99,
		},
		APITurnID: 1,
	})
	if len(res.Events) != 1 {
		t.Fatalf("write turn: events=%d, want 1", len(res.Events))
	}
	ev := res.Events[0]
	if ev.Outcome.Kind != KindImplicitWrite {
		t.Errorf("write turn kind = %q, want implicit_write", ev.Outcome.Kind)
	}
	if ev.TokensWritten != 641 {
		t.Errorf("TokensWritten = %d, want 641 (cache_write_tokens threaded through observeImplicit)", ev.TokensWritten)
	}
	if ev.TokensWritten1h != 0 {
		t.Errorf("TokensWritten1h = %d, want 0 (OpenAI writes untiered — 1h must never survive the implicit lane)", ev.TokensWritten1h)
	}

	// A zero-write continuation turn keeps TokensWritten=0 — byte-identical
	// to the pre-fix ChatGPT-plan lane behaviour (no write count reported).
	now2 := now.Add(time.Second)
	res2 := eng.ObserveTurn(ObserveInput{
		SessionID: "sess-implicit-write",
		Model:     "gpt-5.6-sol",
		Scope:     "default",
		Tier:      TierProxy,
		Caps:      caps,
		MessageID: "req-write-2",
		Now:       now2,
		Usage: CacheUsageObserved{
			NetInputTokens:      3072,
			OutputTokens:        100,
			CacheReadTokens:     2048, // warm hit
			CacheCreationTokens: 0,    // ChatGPT-plan lane reports no write
		},
		APITurnID: 2,
	})
	if len(res2.Events) != 1 {
		t.Fatalf("continuation turn: events=%d, want 1", len(res2.Events))
	}
	if res2.Events[0].TokensWritten != 0 {
		t.Errorf("zero-write continuation TokensWritten = %d, want 0 (unchanged ChatGPT-plan behaviour)", res2.Events[0].TokensWritten)
	}
}
