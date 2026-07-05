package cachetrack

import (
	"testing"
	"time"
)

// TestEngine_LevelHash_ToolsChangedFires proves §7 row 4 actually
// fires turn-over-turn now that the engine tracks the chain hash
// at end-of-tools. Setup: turn A has tools array T1; turn B has
// tools array T2 (different bytes), system unchanged, message
// new. Result: turn B emits cause=tools_changed.
//
// Without LastToolsLevelHash tracking (pre-this-commit), turn B
// would mis-attribute to block_diverged or unknown, depressing
// the §10 mispredict-rate health metric for a missing feature
// rather than a real model bug.
func TestEngine_LevelHash_ToolsChangedFires(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	turnA := ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: []ObserveBlock{
			{Level: LevelTools, Kind: "tool", CanonicalBytes: []byte(`{"name":"Read"}`)},
			{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys"}`)},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u1"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 500},
	}
	a := eng.ObserveTurn(turnA)
	if a.Events[0].Outcome.Cause != CauseReanchor {
		t.Fatalf("turn A: cause = %q, want reanchor", a.Events[0].Outcome.Cause)
	}

	turnB := ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_2", Now: now.Add(time.Minute),
		Blocks: []ObserveBlock{
			// Tools CHANGED — different name.
			{Level: LevelTools, Kind: "tool", CanonicalBytes: []byte(`{"name":"Write"}`)},
			// System unchanged from turn A.
			{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys"}`)},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u2"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 50000},
	}
	b := eng.ObserveTurn(turnB)
	if b.Events[0].Outcome.Cause != CauseToolsChanged {
		t.Errorf("turn B: cause = %q, want tools_changed (level-hash tracking should have caught the tools-block change)", b.Events[0].Outcome.Cause)
	}
}

// TestEngine_LevelHash_SystemChangedFires proves §7 row 5 fires
// when tools are unchanged but system content shifts. R1(a)
// confirmed Claude Code uses 2 system breakpoints because system
// content changes at different frequencies — system_changed is a
// recurring live event the soak measurement needs to attribute
// correctly.
func TestEngine_LevelHash_SystemChangedFires(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	turnA := ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: []ObserveBlock{
			{Level: LevelTools, Kind: "tool", CanonicalBytes: []byte(`{"name":"Read"}`)},
			{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys A"}`)},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u1"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 500},
	}
	eng.ObserveTurn(turnA)

	turnB := ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_2", Now: now.Add(time.Minute),
		Blocks: []ObserveBlock{
			// Tools unchanged.
			{Level: LevelTools, Kind: "tool", CanonicalBytes: []byte(`{"name":"Read"}`)},
			// System CHANGED.
			{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys B (updated)"}`)},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u2"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 30000},
	}
	b := eng.ObserveTurn(turnB)
	if b.Events[0].Outcome.Cause != CauseSystemChanged {
		t.Errorf("turn B: cause = %q, want system_changed", b.Events[0].Outcome.Cause)
	}
}

// TestEngine_LevelHash_StableToolsAndSystemAttributesToWriteOrHit
// is the precedence safety net: when tools + system are BOTH
// unchanged and only the messages grow, the outcome must be the
// suffix_growth baseline (or hit on a read), NOT tools/system_changed.
// Otherwise the level-hash tracking would fire spuriously.
func TestEngine_LevelHash_StableToolsAndSystemAttributesToWriteOrHit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	tools := []byte(`{"name":"Read"}`)
	sys := []byte(`{"text":"sys"}`)

	eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_1", Now: now,
		Blocks: []ObserveBlock{
			{Level: LevelTools, Kind: "tool", CanonicalBytes: tools},
			{Level: LevelSystem, Kind: "text", CanonicalBytes: sys},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u1"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 500},
	})

	b := eng.ObserveTurn(ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierProxy,
		MessageID: "msg_2", Now: now.Add(time.Minute),
		Blocks: []ObserveBlock{
			{Level: LevelTools, Kind: "tool", CanonicalBytes: tools}, // same
			{Level: LevelSystem, Kind: "text", CanonicalBytes: sys},  // same
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u2"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 800}, // small incremental write
	})
	if b.Events[0].Outcome.Cause == CauseToolsChanged || b.Events[0].Outcome.Cause == CauseSystemChanged {
		t.Errorf("stable tools+system spuriously fired %q; want suffix_growth or hit", b.Events[0].Outcome.Cause)
	}
}

// TestEngine_LevelHash_CapsGateRows4and5 verifies that without
// caps.ToolsVisible / caps.SystemVisible, the level-hash signals
// don't fire even when present. Tier-2 (transcript) is the
// canonical case — the engine should NOT misattribute Tier-2
// turns to tools_changed/system_changed even if the chain math
// happens to differ.
func TestEngine_LevelHash_CapsGateRows4and5(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	eng := NewEngine(64)
	eng.Clock = func() time.Time { return now }

	turnA := ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierTranscript,
		MessageID: "msg_1", Now: now,
		Blocks: []ObserveBlock{
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u1"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 500},
	}
	eng.ObserveTurn(turnA)

	// Turn B carries hypothetical tools/system blocks just to confirm
	// the gate; a real Tier-2 source wouldn't see them, but if a future
	// caller mis-populates the input, capabilities should still gate.
	turnB := ObserveInput{
		SessionID: "sA", Model: "m1", Scope: "s", Tier: TierTranscript,
		MessageID: "msg_2", Now: now.Add(time.Minute),
		Blocks: []ObserveBlock{
			{Level: LevelTools, Kind: "tool", CanonicalBytes: []byte(`{"name":"Read"}`)},
			{Level: LevelSystem, Kind: "text", CanonicalBytes: []byte(`{"text":"sys"}`)},
			{Level: LevelMessage, Kind: "text", CanonicalBytes: []byte(`{"text":"u2"}`)},
		},
		Usage: CacheUsageObserved{CacheCreationTokens: 50000},
	}
	b := eng.ObserveTurn(turnB)
	// Tier-2 caps hide tools+system; rows 4-5 cannot fire. The
	// residual rule (tools_or_system_changed) catches the rewrite.
	if b.Events[0].Outcome.Cause == CauseToolsChanged || b.Events[0].Outcome.Cause == CauseSystemChanged {
		t.Errorf("Tier-2 caps allowed row 4/5 to fire: cause = %q", b.Events[0].Outcome.Cause)
	}
}
