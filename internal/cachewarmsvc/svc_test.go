package cachewarmsvc

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/cachewarm"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func cfgDefault() config.CacheWarmConfig {
	return config.Default().CacheWarm
}

// stubLookup returns Anthropic-ish per-million rates for any model.
func stubLookup(_ string) (cost.Pricing, bool) {
	return cost.Pricing{
		Input:           5.0,
		Output:          25.0,
		CacheRead:       0.5,  // ~0.1× input
		CacheCreation:   6.25, // ~1.25× input
		CacheCreation1h: 10.0, // ~2× input
	}, true
}

func TestAssemble_ClassifiesAndRecommends(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	rows := []store.CacheEntryRow{
		{
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "sA",
			PrefixHash: "h", TokenCount: 100000, TTLTier: "5m", Tier: "proxy",
			CreatedAt: now.Add(-time.Minute), LastRefreshAt: now.Add(-30 * time.Second),
			ExpiresAt: now.Add(20 * time.Second), State: "live",
		},
	}
	cfg := cfgDefault()
	cfg.Keepwarm.Mode = "advise" // turn on advise so a recommendation appears

	got := assemble(rows, nil, stubLookup, now, cfg, LoadOpts{IncludeCold: true}, 5*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d statuses, want 1", len(got))
	}
	st := got[0]
	if st.Severity != cachewarm.SeverityCritical {
		t.Errorf("severity = %q, want critical (20s left)", st.Severity)
	}
	if !st.Window.ExpiryAuthoritative {
		t.Errorf("proxy tier should be authoritative")
	}
	if !st.Window.Supports1hTier {
		t.Errorf("claude model should support 1h tier")
	}
	// value = 100000 × (6.25 − 0.5) / 1e6 = 0.575
	if st.ValueAtRiskUSD < 0.57 || st.ValueAtRiskUSD > 0.58 {
		t.Errorf("value-at-risk = %v, want ~0.575", st.ValueAtRiskUSD)
	}
	// 5m Anthropic, advise, fresh (high resume confidence) → use_1h_tier
	if st.Recommendation.Action != cachewarm.ActionUse1hTier {
		t.Errorf("recommendation = %q, want use_1h_tier (%s)", st.Recommendation.Action, st.Recommendation.Rationale)
	}
}

// TestAssemble_MinValueGatesGlobalNotSession pins the option-A behavior:
// a low-value warm cache is suppressed on the global view (MinValueUSD
// floor) but ALWAYS shown on the session-scoped detail view, so opening a
// session surfaces its live countdown regardless of dollar value.
func TestAssemble_MinValueGatesGlobalNotSession(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	// 1000 tokens × (6.25 − 0.5)/1e6 = $0.00575 — well under the 0.05 floor.
	rows := []store.CacheEntryRow{
		{
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "sA",
			PrefixHash: "h", TokenCount: 1000, TTLTier: "5m", Tier: "proxy",
			CreatedAt: now.Add(-time.Minute), LastRefreshAt: now.Add(-10 * time.Second),
			ExpiresAt: now.Add(10 * time.Minute), State: "live", // warm → OK severity
		},
	}
	cfg := cfgDefault() // MinValueUSD = 0.05

	// Global view (no SessionID): floor applies → suppressed.
	global := assemble(rows, nil, stubLookup, now, cfg, LoadOpts{IncludeCold: true}, 5*time.Minute)
	if len(global) != 0 {
		t.Fatalf("global: got %d, want 0 (low-value cache below MinValueUSD floor)", len(global))
	}

	// Session-scoped view: floor waived → shown with its countdown.
	scoped := assemble(rows, nil, stubLookup, now, cfg, LoadOpts{SessionID: "sA", IncludeCold: true}, 5*time.Minute)
	if len(scoped) != 1 {
		t.Fatalf("session-scoped: got %d, want 1 (floor must not gate the detail view)", len(scoped))
	}
	if scoped[0].Severity != cachewarm.SeverityOK {
		t.Errorf("severity = %q, want ok (warm, 10m left)", scoped[0].Severity)
	}
}

// TestAssemble_CollapsesChainToOneWarmCache pins the sliding-window model:
// a session's many per-turn cache write-points collapse into ONE logical
// warm cache whose expiry is the warm-LONGEST entry (most-recent activity +
// TTL), with cumulative token-count — not one noisy row per turn each with
// its own staggered countdown.
func TestAssemble_CollapsesChainToOneWarmCache(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	// Three write-points of the SAME (session, model, scope) chain, written
	// at +0 / +1 / +2 min, each 1h TTL. The latest keeps the chain warm.
	rows := []store.CacheEntryRow{
		{
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "sA",
			PrefixHash: "h1", TokenCount: 4000, TTLTier: "1h", Tier: "transcript",
			LastRefreshAt: now, ExpiresAt: now.Add(60 * time.Minute), State: "live",
		},
		{
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "sA",
			PrefixHash: "h2", TokenCount: 600, TTLTier: "1h", Tier: "transcript",
			LastRefreshAt: now.Add(time.Minute), ExpiresAt: now.Add(61 * time.Minute), State: "live",
		},
		{
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "sA",
			PrefixHash: "h3", TokenCount: 900, TTLTier: "1h", Tier: "transcript",
			LastRefreshAt: now.Add(2 * time.Minute), ExpiresAt: now.Add(62 * time.Minute), State: "live",
		},
	}

	got := assemble(rows, nil, stubLookup, now, cfgDefault(), LoadOpts{SessionID: "sA", IncludeCold: true}, 5*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d windows, want 1 (chain collapses to one logical cache)", len(got))
	}
	w := got[0].Window
	// Expiry = warm-longest entry (latest write keeps the whole chain warm).
	if !w.ExpiresAt.Equal(now.Add(62 * time.Minute)) {
		t.Errorf("expiry = %v, want %v (most-recent activity + TTL)", w.ExpiresAt, now.Add(62*time.Minute))
	}
	// LastRefresh = latest activity across the chain.
	if !w.LastRefresh.Equal(now.Add(2 * time.Minute)) {
		t.Errorf("last_refresh = %v, want %v", w.LastRefresh, now.Add(2*time.Minute))
	}
	// Tokens = cumulative prefix estimate (sum of the chain's writes).
	if w.PrefixTokens != 4000+600+900 {
		t.Errorf("prefix_tokens = %d, want 5500 (cumulative chain)", w.PrefixTokens)
	}

	// Two models in one session stay separate rows.
	rows = append(rows, store.CacheEntryRow{
		Model: "claude-haiku-4-5", CacheScope: "s", SessionID: "sA",
		PrefixHash: "k1", TokenCount: 2000, TTLTier: "1h", Tier: "transcript",
		LastRefreshAt: now, ExpiresAt: now.Add(60 * time.Minute), State: "live",
	})
	got = assemble(rows, nil, stubLookup, now, cfgDefault(), LoadOpts{SessionID: "sA", IncludeCold: true}, 5*time.Minute)
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2 (distinct models = distinct caches)", len(got))
	}
}

// TestImplicitWindows_GradedIdleRiskBands pins the OpenAI/Codex model:
// implicit activity (no cache_entries / no TTL) becomes an ESTIMATED window
// whose severity is graded by idle age — ok <1h, soon ("at risk") 1–2h,
// critical ("significantly increased risk") 2–24h — with a provider-correct
// value-at-risk (full input − cached read, no write tier).
func TestImplicitWindows_GradedIdleRiskBands(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	cfg := cfgDefault() // 1h / 2h / 24h

	cases := []struct {
		name    string
		idle    time.Duration
		wantSev cachewarm.Severity
	}{
		{"fresh", 5 * time.Minute, cachewarm.SeverityOK},
		{"at_risk", 90 * time.Minute, cachewarm.SeveritySoon},
		{"increased_risk", 3 * time.Hour, cachewarm.SeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acts := []store.ImplicitCacheActivity{{
				SessionID: "codexA", Model: "gpt-5.5", Tier: "transcript",
				PrefixTokens: 50000, LastActivity: now.Add(-tc.idle),
			}}
			got := assemble(nil, acts, stubLookup, now, cfg, LoadOpts{SessionID: "codexA", IncludeCold: true}, 5*time.Minute)
			if len(got) != 1 {
				t.Fatalf("got %d windows, want 1", len(got))
			}
			w := got[0]
			if w.Severity != tc.wantSev {
				t.Errorf("idle %v → severity %q, want %q", tc.idle, w.Severity, tc.wantSev)
			}
			if !w.Estimated {
				t.Errorf("implicit window must be flagged estimated")
			}
			if w.Window.Supports1hTier {
				t.Errorf("gpt-5.5 must not advertise the Anthropic 1h tier")
			}
			// value = 50000 × (Input 5.0 − CacheRead 0.5)/1e6 = 0.225.
			if w.ValueAtRiskUSD < 0.224 || w.ValueAtRiskUSD > 0.226 {
				t.Errorf("value = %v, want ~0.225 (full input − cached read)", w.ValueAtRiskUSD)
			}
		})
	}
}

func TestAssemble_KeepwarmOffSuppressesRecommendation(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	rows := []store.CacheEntryRow{{
		Model: "claude-opus-4-8", CacheScope: "s", SessionID: "sA",
		PrefixHash: "h", TokenCount: 100000, TTLTier: "5m", Tier: "proxy",
		LastRefreshAt: now.Add(-30 * time.Second),
		ExpiresAt:     now.Add(20 * time.Second), State: "live",
	}}
	cfg := cfgDefault() // Keepwarm.Mode == "off" by default
	got := assemble(rows, nil, stubLookup, now, cfg, LoadOpts{IncludeCold: true}, 5*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Recommendation.Action != cachewarm.ActionNone {
		t.Errorf("keep-warm off → action none, got %q", got[0].Recommendation.Action)
	}
}

func TestAssemble_DropsStaleColdKeepsRecentCold(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	rows := []store.CacheEntryRow{
		{ // recently cold: expired 1m ago → kept (within 5m grace)
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "recent",
			PrefixHash: "h1", TokenCount: 100000, TTLTier: "5m", Tier: "proxy",
			LastRefreshAt: now.Add(-6 * time.Minute),
			ExpiresAt:     now.Add(-1 * time.Minute), State: "live",
		},
		{ // long-dead cold: expired 3 DAYS ago (idle 'live' entry) → dropped
			Model: "claude-opus-4-8", CacheScope: "s", SessionID: "stale",
			PrefixHash: "h2", TokenCount: 100000, TTLTier: "5m", Tier: "proxy",
			LastRefreshAt: now.Add(-72 * time.Hour),
			ExpiresAt:     now.Add(-72 * time.Hour), State: "live",
		},
	}
	got := assemble(rows, nil, stubLookup, now, cfgDefault(), LoadOpts{IncludeCold: true}, 5*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1 (stale cold dropped, recent cold kept)", len(got))
	}
	if got[0].Window.SessionID != "recent" {
		t.Errorf("kept the wrong window: %s", got[0].Window.SessionID)
	}
}

func TestResumeConfidence_DecaysWithIdle(t *testing.T) {
	now := time.Unix(1_000_000, 0).UTC()
	cases := []struct {
		idle time.Duration
		want float64
	}{
		{30 * time.Second, 0.9},
		{5 * time.Minute, 0.6},
		{20 * time.Minute, 0.4},
		{2 * time.Hour, 0.2},
	}
	for _, tc := range cases {
		if got := resumeConfidence(now, now.Add(-tc.idle)); got != tc.want {
			t.Errorf("idle %s: confidence = %v, want %v", tc.idle, got, tc.want)
		}
	}
	if got := resumeConfidence(now, time.Time{}); got != 0.3 {
		t.Errorf("zero lastRefresh → 0.3, got %v", got)
	}
}

func TestLooksAnthropicAndGemini(t *testing.T) {
	if !looksAnthropic("claude-opus-4-8") || !looksAnthropic("anthropic/sonnet") {
		t.Errorf("anthropic detection failed")
	}
	if looksAnthropic("gpt-5.5") || looksAnthropic("gemini-2.5-pro") {
		t.Errorf("false anthropic positive")
	}
	if !looksGemini("gemini-2.5-pro") || looksGemini("claude-opus-4-8") {
		t.Errorf("gemini detection failed")
	}
}
