package cachetrack

import (
	"testing"
	"time"
)

// TestEngine_ObserveTurn_FirstTurnReanchor confirms the first
// turn for a fresh (session, model, scope) tuple is attributed
// reanchor and creates a write entry when CacheCreationTokens>0.
func TestEngine_ObserveTurn_FirstTurnReanchor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	out := eng.ObserveTurn(ObserveInput{
		SessionID: "sA",
		Model:     "claude-opus-4-7",
		Scope:     "s",
		Tier:      TierProxy,
		MessageID: "msg_1",
		Now:       now,
		Blocks: []ObserveBlock{
			{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys"}`)},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"hi"}`)},
		},
		Breakpoints: []ObserveBreakpoint{
			{BlockIndex: 1, Level: LevelMessage, TTL: TTL1h},
		},
		Usage: CacheUsageObserved{
			NetInputTokens:        100,
			OutputTokens:          50,
			CacheCreationTokens:   500,
			CacheCreation1hTokens: 500,
		},
		APITurnID: 42,
	})

	if len(out.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(out.Events))
	}
	if out.Events[0].Outcome.Cause != CauseReanchor {
		t.Errorf("cause = %q, want reanchor (first turn)", out.Events[0].Outcome.Cause)
	}
	if len(out.Segments) == 0 {
		t.Error("expected segments to be emitted")
	}
	// The marked block should appear as IsBreakpoint=true.
	var sawBP bool
	for _, s := range out.Segments {
		if s.IsBreakpoint {
			sawBP = true
			if s.TTL != TTL1h {
				t.Errorf("breakpoint segment TTL = %v, want TTL1h", s.TTL)
			}
		}
	}
	if !sawBP {
		t.Error("no breakpoint segment emitted despite an explicit marker")
	}
}

// TestEngine_ObserveTurn_SecondTurnHitsCachedPrefix exercises a
// read hit on the second turn: same prefix chain, observed
// cache_read > 0 → kind=hit cause=suffix_growth (the baseline).
func TestEngine_ObserveTurn_SecondTurnHitsCachedPrefix(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	// First turn: writes a 1h-tier entry at the marked block.
	first := eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: []ObserveBlock{
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u1"}`)},
		},
		Breakpoints: []ObserveBreakpoint{{BlockIndex: 0, Level: LevelMessage, TTL: TTL1h}},
		Usage:       CacheUsageObserved{CacheCreationTokens: 500, CacheCreation1hTokens: 500},
		APITurnID:   1,
	})
	if first.Events[0].Outcome.Cause != CauseReanchor {
		t.Fatalf("first turn cause = %q, want reanchor", first.Events[0].Outcome.Cause)
	}
	if len(first.Entries) != 1 {
		t.Fatalf("first turn entries = %d, want 1", len(first.Entries))
	}

	// Second turn: SAME chain extended by one block; observed
	// cache_read > 0 indicates the provider hit the prefix.
	later := now.Add(time.Minute)
	second := eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_2", Now: later,
		Blocks: []ObserveBlock{
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u2"}`)},
		},
		Breakpoints: []ObserveBreakpoint{{BlockIndex: 0, Level: LevelMessage, TTL: TTL1h}},
		Usage:       CacheUsageObserved{CacheReadTokens: 500},
		APITurnID:   2,
	})

	if second.Events[0].Outcome.Cause != CauseSuffixGrowth {
		t.Errorf("second turn cause = %q, want suffix_growth (clean read)", second.Events[0].Outcome.Cause)
	}
	if second.Events[0].Outcome.Kind != KindHit {
		t.Errorf("second turn kind = %q, want hit", second.Events[0].Outcome.Kind)
	}
}

// TestEngine_ObserveTurn_CompactionInvalidates: a turn with
// CompactionSeen=true resets the chain and emits cause=
// context_compacted.
func TestEngine_ObserveTurn_CompactionInvalidates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	// First turn establishes prior state.
	eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: []ObserveBlock{
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u1"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 500},
	})

	// Second turn: compaction.
	out := eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID:      "msg_2",
		Now:            now.Add(time.Minute),
		CompactionSeen: true,
		Blocks: []ObserveBlock{
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u2-compacted"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 50000},
	})
	if out.Events[0].Outcome.Cause != CauseContextCompacted {
		t.Errorf("cause = %q, want context_compacted", out.Events[0].Outcome.Cause)
	}
}

// TestEngine_ObserveTurn_ScopeIsolation proves that two
// observations under different scopes do NOT share entries.
func TestEngine_ObserveTurn_ScopeIsolation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	blocks := []ObserveBlock{
		{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u"}`)},
	}
	a := eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "scope-A", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: blocks,
		Usage:  CacheUsageObserved{CacheCreationTokens: 500},
	})
	b := eng.ObserveTurn(ObserveInput{
		SessionID: "sB", Model: "m1", Scope: "scope-B", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: blocks,
		Usage:  CacheUsageObserved{CacheCreationTokens: 500},
	})
	// Both create their own entries (different scopes → different
	// engine sessions; no shared state).
	if len(a.Entries) != 1 || len(b.Entries) != 1 {
		t.Errorf("entries: a=%d b=%d, want both = 1 (scope isolation)", len(a.Entries), len(b.Entries))
	}
	if a.Events[0].Outcome.Cause != CauseReanchor || b.Events[0].Outcome.Cause != CauseReanchor {
		t.Errorf("both should reanchor on first turn under their scope")
	}
}

// TestEngine_LRU_EvictsOldestOverCap proves the session map
// honors maxSessions by evicting the oldest unused (session,
// model, scope) tuple.
func TestEngine_LRU_EvictsOldestOverCap(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(2)
	eng.Clock = func() time.Time { return now }

	mkInput := func(sid string, when time.Time) ObserveInput {
		return ObserveInput{
			SessionID: sid, Model: "m1", Scope: "s", Tier: TierProxy,
			MessageID: "msg_1", Now: when,
			Blocks: []ObserveBlock{
				{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u"}`)},
			},
			Usage: CacheUsageObserved{CacheCreationTokens: 1},
		}
	}

	eng.ObserveTurn(mkInput("sA", now))
	eng.ObserveTurn(mkInput("sB", now.Add(time.Second)))
	// Third session evicts sA (the oldest).
	eng.ObserveTurn(mkInput("sC", now.Add(2*time.Second)))

	if _, ok := eng.sessions[engineSessionKey{SessionID: "sA", Model: "m1", Scope: "s"}]; ok {
		t.Error("sA should have been evicted as the oldest")
	}
	if _, ok := eng.sessions[engineSessionKey{SessionID: "sB", Model: "m1", Scope: "s"}]; !ok {
		t.Error("sB should still be present")
	}
	if _, ok := eng.sessions[engineSessionKey{SessionID: "sC", Model: "m1", Scope: "s"}]; !ok {
		t.Error("sC should be present (just added)")
	}
}
