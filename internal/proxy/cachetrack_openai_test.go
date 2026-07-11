package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/cachetrack"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestBuildCacheObserveInput_Anthropic verifies the Anthropic path
// still returns the same shape as the pre-§15.3 code: marker-aware,
// Capabilities unset (the engine boundary-backfills via
// CapabilitiesFor(TierProxy)).
func TestBuildCacheObserveInput_Anthropic(t *testing.T) {
	t.Parallel()
	p := newTestProxyMinimal()
	turn := models.APITurn{
		SessionID:             "sess-anthro",
		Model:                 "claude-opus-4-8",
		RequestID:             "req-1",
		InputTokens:           100,
		OutputTokens:          50,
		CacheReadTokens:       10,
		CacheCreationTokens:   90,
		CacheCreation1hTokens: 90,
		Fast:                  true,
	}
	reqShape := requestShape{
		Model: "claude-opus-4-8",
		CacheBlocks: []cachetrack.ObserveBlock{
			{Level: cachetrack.LevelTools, Kind: "tools", CanonicalBytes: []byte("[]")},
		},
		CacheBreakpoints: []cachetrack.ObserveBreakpoint{
			{BlockIndex: 0, Level: cachetrack.LevelTools, TTL: cachetrack.TTL1h},
		},
	}
	in, ok := p.buildCacheObserveInput(turn, reqShape, models.ProviderAnthropic, 42)
	if !ok {
		t.Fatalf("Anthropic path returned ok=false; expected true")
	}
	if in.Caps.ImplicitCache {
		t.Errorf("Anthropic Caps.ImplicitCache = true, want false")
	}
	if in.Tier != cachetrack.TierProxy {
		t.Errorf("Tier = %v, want TierProxy", in.Tier)
	}
	if len(in.Blocks) != 1 || len(in.Breakpoints) != 1 {
		t.Errorf("expected Blocks/Breakpoints to be carried through; got blocks=%d breakpoints=%d",
			len(in.Blocks), len(in.Breakpoints))
	}
	if in.Usage.CacheCreationTokens != 90 {
		t.Errorf("CacheCreationTokens not threaded; got %d", in.Usage.CacheCreationTokens)
	}
}

// TestBuildCacheObserveInput_Anthropic_DropsWhenNoBlocks confirms
// an Anthropic turn whose request shape has zero CacheBlocks is
// dropped — the marker-aware engine path needs the chain to run.
func TestBuildCacheObserveInput_Anthropic_DropsWhenNoBlocks(t *testing.T) {
	t.Parallel()
	p := newTestProxyMinimal()
	_, ok := p.buildCacheObserveInput(
		models.APITurn{SessionID: "s", Model: "claude-opus-4-8"},
		requestShape{Model: "claude-opus-4-8"},
		models.ProviderAnthropic,
		1,
	)
	if ok {
		t.Errorf("Anthropic with empty CacheBlocks returned ok=true; want false")
	}
}

// TestBuildCacheObserveInput_OpenAI verifies §15.3 wiring: an
// OpenAI turn returns an ObserveInput with Capabilities.ImplicitCache=
// true, no Blocks, no Breakpoints, and the cached_tokens scalar
// carried through CacheReadTokens.
func TestBuildCacheObserveInput_OpenAI(t *testing.T) {
	t.Parallel()
	p := newTestProxyMinimal()
	turn := models.APITurn{
		SessionID:       "sess-openai-1",
		Model:           "gpt-5",
		RequestID:       "req-openai-1",
		InputTokens:     2048, // NET non-cached (already netted by parseOpenAIResponse)
		OutputTokens:    100,
		CacheReadTokens: 1024, // cached_tokens scalar
	}
	in, ok := p.buildCacheObserveInput(turn, requestShape{Model: "gpt-5"}, models.ProviderOpenAI, 7)
	if !ok {
		t.Fatalf("OpenAI path returned ok=false; want true (the implicit-cache surface DOES grade these)")
	}
	if !in.Caps.ImplicitCache {
		t.Errorf("OpenAI Caps.ImplicitCache = false, want true — engine MUST dispatch to the reduced attribution path")
	}
	if in.Tier != cachetrack.TierProxy {
		t.Errorf("OpenAI Tier = %v, want TierProxy", in.Tier)
	}
	if len(in.Blocks) != 0 {
		t.Errorf("OpenAI in.Blocks = %d, want 0 — implicit-cache path skips the chain", len(in.Blocks))
	}
	if len(in.Breakpoints) != 0 {
		t.Errorf("OpenAI in.Breakpoints = %d, want 0", len(in.Breakpoints))
	}
	if in.Usage.CacheReadTokens != 1024 {
		t.Errorf("OpenAI in.Usage.CacheReadTokens = %d, want 1024 (the cached_tokens scalar)", in.Usage.CacheReadTokens)
	}
	if in.Usage.CacheCreationTokens != 0 || in.Usage.CacheCreation1hTokens != 0 {
		t.Errorf("OpenAI must NOT carry cache_creation tokens; got creation=%d 1h=%d",
			in.Usage.CacheCreationTokens, in.Usage.CacheCreation1hTokens)
	}
	// Capability sanity: the boundary overlay starts from the
	// TierProxy defaults so any future capability the proxy gains
	// (e.g. a new MarkersVisible-driven path) is inherited.
	if !in.Caps.UsageObserved || !in.Caps.BlocksAreCumulative {
		t.Errorf("OpenAI Caps should inherit Tier-1 defaults; got UsageObserved=%v BlocksAreCumulative=%v",
			in.Caps.UsageObserved, in.Caps.BlocksAreCumulative)
	}
}

// TestBuildCacheObserveInput_OpenAI_DropsWhenIdentityIncomplete
// confirms an OpenAI turn missing session_id OR model is dropped —
// the engine can't group implicit-cache events without identity.
func TestBuildCacheObserveInput_OpenAI_DropsWhenIdentityIncomplete(t *testing.T) {
	t.Parallel()
	p := newTestProxyMinimal()
	cases := []models.APITurn{
		{SessionID: "", Model: "gpt-5", CacheReadTokens: 100},
		{SessionID: "s", Model: "", CacheReadTokens: 100},
	}
	for _, turn := range cases {
		if _, ok := p.buildCacheObserveInput(turn, requestShape{}, models.ProviderOpenAI, 1); ok {
			t.Errorf("OpenAI with incomplete identity (session=%q model=%q) returned ok=true; want false", turn.SessionID, turn.Model)
		}
	}
}

// TestBuildCacheObserveInput_UnknownProviderDropped pins the closed-
// set: an unknown provider string returns ok=false (no silent
// emission to bucketSkipped).
func TestBuildCacheObserveInput_UnknownProviderDropped(t *testing.T) {
	t.Parallel()
	p := newTestProxyMinimal()
	turn := models.APITurn{SessionID: "s", Model: "m", CacheReadTokens: 100}
	if _, ok := p.buildCacheObserveInput(turn, requestShape{}, "vertex-ai", 1); ok {
		t.Errorf("unknown provider returned ok=true; want false")
	}
}

// captureCacheSink is a stub CacheSink that captures every
// PersistCacheObservation call for end-to-end assertions.
type captureCacheSink struct {
	calls []captureCall
}

type captureCall struct {
	In           cachetrack.ObserveInput
	Result       cachetrack.ObserveResult
	APITurnID    int64
	TokenUsageID int64
}

func (c *captureCacheSink) PersistCacheObservation(_ context.Context, in cachetrack.ObserveInput, result cachetrack.ObserveResult, apiTurnID, tokenUsageID int64) error {
	c.calls = append(c.calls, captureCall{In: in, Result: result, APITurnID: apiTurnID, TokenUsageID: tokenUsageID})
	return nil
}

// TestProxyOpenAIFlowsEndToEnd_RealAssembly drives the §15.3
// "OpenAI path flags a seeded prefix-churn fixture" acceptance
// criterion through the real assembly path: build → engine →
// sink. Confirms the engine emits the expected kind+cause for
// each turn and the §10 Anthropic G2 counter stays at zero.
func TestProxyOpenAIFlowsEndToEnd_RealAssembly(t *testing.T) {
	t.Parallel()
	eng := cachetrack.NewEngine(0)
	sink := &captureCacheSink{}
	p := &Proxy{
		now:         func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
		cacheEngine: eng,
		cacheSink:   sink,
	}

	// Helper: directly drive ObserveTurn through buildCacheObserveInput
	// (mirrors the insertTurnAndCacheDetached flow without needing a
	// SQL sink for the api_turn insert — that's a separate concern).
	feed := func(turn models.APITurn) {
		in, ok := p.buildCacheObserveInput(turn, requestShape{}, models.ProviderOpenAI, turn.InputTokens)
		if !ok {
			t.Fatalf("buildCacheObserveInput returned ok=false for turn %+v", turn)
		}
		result := eng.ObserveTurn(in)
		_ = sink.PersistCacheObservation(context.Background(), in, result, 1, 0)
	}

	// Turn 1 — bootstrap (no prior). Engine emits KindImplicitWrite +
	// CauseReanchor; ImplicitPrefixTokens seeds from InputTokens=2048
	// → quantize to 2048.
	feed(models.APITurn{
		SessionID:       "sess-openai-1",
		Model:           "gpt-5",
		RequestID:       "req-1",
		InputTokens:     2048,
		OutputTokens:    100,
		CacheReadTokens: 0,
	})
	// Turn 2 — continuation HIT. cached_tokens > 0, prior prefix
	// 2048 → KindImplicitHit + CauseImplicitHit.
	feed(models.APITurn{
		SessionID:       "sess-openai-1",
		Model:           "gpt-5",
		RequestID:       "req-2",
		InputTokens:     3072,
		OutputTokens:    100,
		CacheReadTokens: 2048,
	})
	// Turn 3 — SEEDED PREFIX CHURN. cached_tokens dropped to zero on
	// a turn whose prior prefix was 2048 (above the 1024 min). Engine
	// must emit KindImplicitMiss + CausePrefixChurn — this is the
	// §15.3 P3 acceptance criterion.
	feed(models.APITurn{
		SessionID:       "sess-openai-1",
		Model:           "gpt-5",
		RequestID:       "req-3",
		InputTokens:     3072,
		OutputTokens:    100,
		CacheReadTokens: 0,
	})

	if got := len(sink.calls); got != 3 {
		t.Fatalf("sink.calls = %d, want 3 (one per turn)", got)
	}
	want := []struct {
		kind  cachetrack.Kind
		cause cachetrack.Cause
	}{
		{cachetrack.KindImplicitWrite, cachetrack.CauseReanchor},
		{cachetrack.KindImplicitHit, cachetrack.CauseImplicitHit},
		{cachetrack.KindImplicitMiss, cachetrack.CausePrefixChurn},
	}
	for i, w := range want {
		evs := sink.calls[i].Result.Events
		if len(evs) != 1 {
			t.Fatalf("turn %d: events=%d, want 1", i+1, len(evs))
		}
		if evs[0].Outcome.Kind != w.kind || evs[0].Outcome.Cause != w.cause {
			t.Errorf("turn %d: kind=%q cause=%q, want kind=%q cause=%q",
				i+1, evs[0].Outcome.Kind, evs[0].Outcome.Cause, w.kind, w.cause)
		}
		// Implicit path emits no Segments and no Entries.
		if len(sink.calls[i].Result.Segments) != 0 || len(sink.calls[i].Result.Entries) != 0 {
			t.Errorf("turn %d: implicit path leaked segments=%d entries=%d (must be 0)",
				i+1, len(sink.calls[i].Result.Segments), len(sink.calls[i].Result.Entries))
		}
	}
	// §5 G2/G3 counters MUST stay at zero across an OpenAI-only run.
	counters := eng.Counters()
	if counters.BucketMispredictTotal != 0 || counters.TriggerMispredictCalls != 0 {
		t.Errorf("after OpenAI-only run: G2/G3 counters = (%d, %d), want (0, 0) — §5 guardrail breached",
			counters.BucketMispredictTotal, counters.TriggerMispredictCalls)
	}
}

// newTestProxyMinimal builds a Proxy with just the fields
// buildCacheObserveInput needs (the clock). Avoids pulling in the
// full proxy construction path so the unit stays focused.
func newTestProxyMinimal() *Proxy {
	return &Proxy{
		now: func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) },
	}
}
