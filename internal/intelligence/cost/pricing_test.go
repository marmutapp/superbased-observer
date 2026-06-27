package cost

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func TestTable_LookupExact(t *testing.T) {
	tb := NewTable()
	p, ok := tb.Lookup("claude-sonnet-4-20250514")
	if !ok {
		t.Fatalf("expected sonnet-4 pricing to exist")
	}
	if p.Input != 3 || p.Output != 15 {
		t.Errorf("sonnet-4 input/output wrong: %+v", p)
	}
	if p.CacheRead != 0.30 {
		t.Errorf("sonnet-4 cache_read wrong: %+v", p)
	}
	if p.CacheCreation != 3.75 {
		t.Errorf("sonnet-4 cache_creation wrong: %+v", p)
	}
}

// TestTable_Anthropic2026Q2Pricing pins the post-2026-04-29 pricing
// snapshot (docs/pricing-reference.md). When Anthropic changes
// rates, update both this test and the reference doc.
//
// The interesting cases are the SKUs where pricing diverged WITHIN a
// model family — Opus 4.5+ dropped to 1/3 of Opus 4 / 4.1 rates. Family
// fallback for new SKUs (claude-opus-4-7) must hit the latest tier, not
// the legacy.
func TestTable_Anthropic2026Q2Pricing(t *testing.T) {
	tb := NewTable()

	for _, tc := range []struct {
		name, model    string
		in, out        float64
		cacheR, cacheW float64
		cacheW1h       float64
	}{
		// Current-gen Opus (4.5 / 4.6 / 4.7): 1/3 of legacy Opus pricing.
		{"opus-4-7 explicit", "claude-opus-4-7", 5, 25, 0.50, 6.25, 10},
		{"opus-4-6 explicit", "claude-opus-4-6", 5, 25, 0.50, 6.25, 10},
		{"opus-4-5 explicit", "claude-opus-4-5", 5, 25, 0.50, 6.25, 10},
		// Legacy Opus 4 / 4.1 — full $15/$75 rates.
		{"opus-4-1 explicit", "claude-opus-4-1", 15, 75, 1.5, 18.75, 30},
		{"opus-4-1 dated", "claude-opus-4-1-20250805", 15, 75, 1.5, 18.75, 30},
		// Sonnet 4 family — single rate across versions.
		{"sonnet-4-6", "claude-sonnet-4-6", 3, 15, 0.30, 3.75, 6},
		{"sonnet-3-7 deprecated", "claude-sonnet-3-7", 3, 15, 0.30, 3.75, 6},
		// Haiku 4.5.
		{"haiku-4-5", "claude-haiku-4-5", 1, 5, 0.10, 1.25, 2},
		{"haiku-4-5 dated", "claude-haiku-4-5-20251001", 1, 5, 0.10, 1.25, 2},
		// Opus 4.8 — pinned explicit row (was family fallback prior to
		// 2026-06-07). Same rates as 4.7. The explicit row holds the
		// FastMultiplier premium tier (speed=fast → $10/$50 — 2× across
		// the board).
		{"opus-4-8 explicit", "claude-opus-4-8", 5, 25, 0.50, 6.25, 10},
		// Hypothetical future SKU — must inherit the LATEST family rates,
		// not the legacy. claude-opus-4-9 should be priced like Opus 4.7/4.8,
		// not Opus 4. (Family prefix "claude-opus-4" is set to current
		// rates so this works out of the box.)
		{"future opus-4-9 inherits current", "claude-opus-4-9", 5, 25, 0.50, 6.25, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := tb.Lookup(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) returned ok=false", tc.model)
			}
			if p.Input != tc.in {
				t.Errorf("input: got %v want %v", p.Input, tc.in)
			}
			if p.Output != tc.out {
				t.Errorf("output: got %v want %v", p.Output, tc.out)
			}
			if p.CacheRead != tc.cacheR {
				t.Errorf("cache_read: got %v want %v", p.CacheRead, tc.cacheR)
			}
			if p.CacheCreation != tc.cacheW {
				t.Errorf("cache_creation: got %v want %v", p.CacheCreation, tc.cacheW)
			}
			if p.CacheCreation1h != tc.cacheW1h {
				t.Errorf("cache_creation_1h: got %v want %v", p.CacheCreation1h, tc.cacheW1h)
			}
		})
	}
}

// TestTable_OpenAI2026Q2Pricing pins the post-2026-04-29 OpenAI snapshot.
// Cached input maps to CacheRead. For models that publish a cache rate
// (gpt-5, gpt-4.1, gpt-4o, o-series with cache support, etc.) the
// explicit rate must round-trip exactly. For models without a published
// cache tier (gpt-5-pro, o1-pro, gpt-3.5*, base models) the explicit
// rate is 0; fillDefaults assigns 0.10 × Input as a defensive fallback,
// but that rate never binds in practice because the proxy never records
// cache_read_tokens for OpenAI cache-less models. The test asserts the
// `Input` and `Output` rates always; `cacheR < 0` is a sentinel meaning
// "skip the assertion (model has no real cache tier)".
func TestTable_OpenAI2026Q2Pricing(t *testing.T) {
	tb := NewTable()

	const skipCache = -1.0
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
	}{
		// GPT-5 family — current frontier (rates per
		// docs/pricing-reference.md, OpenAI snapshot 2026-04-29).
		{"gpt-5", "gpt-5", 1.07, 0.107, 8.50},
		{"gpt-5.5", "gpt-5.5", 5, 0.50, 30},
		{"gpt-5.4", "gpt-5.4", 2.50, 0.25, 15},
		{"gpt-5.4-mini", "gpt-5.4-mini", 0.75, 0.075, 4.50},
		{"gpt-5.4-nano", "gpt-5.4-nano", 0.20, 0.02, 1.25},
		{"gpt-5.3-codex", "gpt-5.3-codex", 1.75, 0.175, 14},
		{"gpt-5.2", "gpt-5.2", 1.75, 0.175, 14},
		{"gpt-5.1", "gpt-5.1", 1.07, 0.107, 8.50},
		{"gpt-5.1-codex-max", "gpt-5.1-codex-max", 1.25, 0.125, 10},
		{"gpt-5-mini", "gpt-5-mini", 0.25, 0.025, 2},
		{"gpt-5-nano free", "gpt-5-nano", 0, 0, 0},
		{"gpt-5-pro legacy", "gpt-5-pro", 15, skipCache, 120},
		{"gpt-5.5-pro cached=input", "gpt-5.5-pro", 30, 30, 180},
		// GPT-4.1.
		{"gpt-4.1", "gpt-4.1", 2, 0.50, 8},
		{"gpt-4.1-nano", "gpt-4.1-nano", 0.10, 0.025, 0.40},
		// o-series.
		{"o3", "o3", 2, 0.50, 8},
		{"o4-mini", "o4-mini", 1.10, 0.275, 4.40},
		{"o1", "o1", 15, 7.50, 60},
		{"o1-pro no cache", "o1-pro", 150, skipCache, 600},
		// Legacy.
		{"gpt-3.5-turbo no cache", "gpt-3.5-turbo", 0.50, skipCache, 1.50},
		{"davinci-002 no cache", "davinci-002", 2, skipCache, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := tb.Lookup(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) returned ok=false", tc.model)
			}
			if p.Input != tc.in {
				t.Errorf("input: got %v want %v", p.Input, tc.in)
			}
			if p.Output != tc.out {
				t.Errorf("output: got %v want %v", p.Output, tc.out)
			}
			if tc.cacheR != skipCache && p.CacheRead != tc.cacheR {
				t.Errorf("cache_read: got %v want %v", p.CacheRead, tc.cacheR)
			}
		})
	}
}

// TestTable_GeminiPricing pins the Google Gemini text-model rates.
// Tiered pricing (Gemini 2.5 Pro / 3.1 Pro Preview ≤200k vs >200k):
// we pin the lower tier — see docs/pricing-reference.md.
func TestTable_GeminiPricing(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
	}{
		{"gemini-3.1-pro-preview", "gemini-3.1-pro-preview", 2, 0.20, 12},
		{"gemini-3-flash-preview", "gemini-3-flash-preview", 0.50, 0.05, 3},
		// Gemini 3.5 Flash — Standard tier per Google's Developer API
		// pricing card (2026-05-19). No 3.5 Pro on the official page.
		{"gemini-3.5-flash", "gemini-3.5-flash", 1.50, 0.15, 9},
		// 3.5 Flash family fallback: gemini-3.5-flash is a longest-prefix
		// match for any future flash-suffix SKU (e.g. -experimental),
		// since familyKeys promotes every exact entry without a date
		// suffix into a family candidate.
		{"gemini-3.5-flash-experimental fallback", "gemini-3.5-flash-experimental", 1.50, 0.15, 9},
		{"gemini-2.5-pro", "gemini-2.5-pro", 1.25, 0.125, 10},
		{"gemini-2.5-flash", "gemini-2.5-flash", 0.30, 0.03, 2.50},
		{"gemini-2.5-flash-lite", "gemini-2.5-flash-lite", 0.10, 0.01, 0.40},
		{"gemini-2.0-flash deprecated", "gemini-2.0-flash", 0.10, 0.025, 0.40},
		// Family fallback: future SKU "gemini-3.1-something" inherits
		// gemini-3.1 family prefix (same as 3.1-pro-preview rates).
		{"gemini-3.1 family fallback", "gemini-3.1-something-new", 2, 0.20, 12},
		// Unknown 3.5 SKU (e.g. hypothetical "gemini-3.5-pro" not on
		// the official pricing page as of 2026-05-19) falls through
		// gemini-3.5-flash (doesn't match) to gemini-3 family → Pro
		// rates ($2/$12/$0.20). Conservative default.
		{"unknown 3.5 SKU falls to gemini-3 family (Pro rates)", "gemini-3.5-pro-hypothetical", 2, 0.20, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := tb.Lookup(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) returned ok=false", tc.model)
			}
			if p.Input != tc.in || p.Output != tc.out || p.CacheRead != tc.cacheR {
				t.Errorf("got %+v want input=%v cache_r=%v out=%v", p, tc.in, tc.cacheR, tc.out)
			}
		})
	}
}

// TestTable_AntigravityInternalSKUs pins the model identifiers the
// Antigravity IDE writes for each model + effort selector. Pre-2026-05-13
// these matched via family-prefix fallback (or missed entirely for
// `gemini-pro-agent`); the bug surfaced as silent $0 on Pro-high and
// silent 4× overpricing on Flash (which inherited the gemini-3 family's
// Pro-rate fallback). Pin each SKU to PricingSourceExact so future
// pricing-table churn can't silently re-break the alignment.
func TestTable_AntigravityInternalSKUs(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
		wantLC          bool
	}{
		// Pro variants — all map to Gemini 3.1 Pro rates incl. 200K LC tier.
		{"pro-agent (high)", "gemini-pro-agent", 2, 0.20, 12, true},
		{"pro-high explicit", "gemini-3.1-pro-high", 2, 0.20, 12, true},
		{"pro-medium explicit", "gemini-3.1-pro-medium", 2, 0.20, 12, true},
		{"pro-low explicit", "gemini-3.1-pro-low", 2, 0.20, 12, true},
		// Gemini 3 Pro effort SKUs — pinned in v1.6.19 paralleling
		// gemini-3.1-pro-*. Pre-v1.6.19 resolved via the gemini-3
		// family-prefix fallback (Pro rates, correct by happy accident).
		{"gemini-3 pro-high explicit", "gemini-3-pro-high", 2, 0.20, 12, true},
		{"gemini-3 pro-medium explicit", "gemini-3-pro-medium", 2, 0.20, 12, true},
		{"gemini-3 pro-low explicit", "gemini-3-pro-low", 2, 0.20, 12, true},
		// Flash variants — must map to Flash rates ($0.50/$3), NOT Pro
		// rates via family fallback. The pre-fix bug.
		{"flash-agent (default)", "gemini-3-flash-agent", 0.50, 0.05, 3, false},
		{"flash-high explicit", "gemini-3-flash-high", 0.50, 0.05, 3, false},
		{"flash-medium explicit", "gemini-3-flash-medium", 0.50, 0.05, 3, false},
		{"flash-low explicit", "gemini-3-flash-low", 0.50, 0.05, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q): ok=false", tc.model)
			}
			if src != PricingSourceExact {
				t.Errorf("Lookup(%q) source=%q want exact (alias must pin, not fall through)",
					tc.model, src)
			}
			if p.Input != tc.in || p.CacheRead != tc.cacheR || p.Output != tc.out {
				t.Errorf("Lookup(%q): got input=%v cache_r=%v out=%v; want %v / %v / %v",
					tc.model, p.Input, p.CacheRead, p.Output, tc.in, tc.cacheR, tc.out)
			}
			hasLC := p.LongContextThreshold > 0
			if hasLC != tc.wantLC {
				t.Errorf("Lookup(%q): LC threshold present=%v want %v",
					tc.model, hasLC, tc.wantLC)
			}
		})
	}
}

// TestTable_Gemini3FlashFamilyResilience locks in the gemini-3-flash
// family-prefix entry added in v1.6.19. The antigravity audit
// (docs/antigravity-audit-2026-05-19.md §B1) identified that hypothetical
// future gemini-3-flash-* SKUs would otherwise fall through to the
// gemini-3 family and bill at Pro rates ($2/$12) instead of Flash
// ($0.50/$3) — ~4× over-bill. The family entry catches the gap.
func TestTable_Gemini3FlashFamilyResilience(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model string
		wantInput   float64
		wantOutput  float64
		wantSource  PricingSource
	}{
		{"flash family exact", "gemini-3-flash", 0.50, 3, PricingSourceExact},
		{"hypothetical experimental flash", "gemini-3-flash-experimental", 0.50, 3, PricingSourceFamily},
		{"unknown flash suffix routes to flash family, not pro family", "gemini-3-flash-foo-bar", 0.50, 3, PricingSourceFamily},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) returned ok=false", tc.model)
			}
			if p.Input != tc.wantInput || p.Output != tc.wantOutput {
				t.Errorf("Lookup(%q): got input=%v output=%v; want %v / %v (Flash, not Pro)",
					tc.model, p.Input, p.Output, tc.wantInput, tc.wantOutput)
			}
			if src != tc.wantSource {
				t.Errorf("Lookup(%q): source=%q want %q", tc.model, src, tc.wantSource)
			}
		})
	}
}

// TestTable_OtherProviderPricing covers xAI / Moonshot / Cursor own.
func TestTable_OtherProviderPricing(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
	}{
		// xAI moved to a unified $1.25/$2.50 rate card for the
		// grok-4.x line on 2026-05-15 (concurrent with grok-code-fast-1
		// retirement). CacheRead falls back to 10% × Input via
		// fillDefaults = $0.125.
		{"grok-4.3", "grok-4.3", 1.25, 0.125, 2.50},
		{"grok-4-20 historical alias", "grok-4-20", 1.25, 0.125, 2.50},
		{"grok-code-fast-1 redirects to grok-4.3", "grok-code-fast-1", 1.25, 0.125, 2.50},
		{"grok family fallback", "grok-5", 1.25, 0.125, 2.50},
		{"kimi-k2-5", "kimi-k2-5", 0.60, 0.10, 3},
		// Kimi K2.6 — added 2026-06-07; family prefix bumped to K2.6 rates.
		{"kimi-k2-6", "kimi-k2-6", 0.684, 0.144, 3.42},
		// Family fallback: future K3 SKU inherits current K2.6 rates (the
		// bumped family prefix), not legacy K2.5 — same pattern as
		// `claude-opus-4` family → 4.5+ rates rather than legacy 4.x.
		{"hypothetical kimi-k3 → K2.6 family", "kimi-k3-1", 0.684, 0.144, 3.42},
		{"composer-1", "composer-1", 1.25, 0.125, 10},
		{"composer-1.5", "composer-1.5", 3.50, 0.35, 17.50},
		{"composer-2", "composer-2", 0.50, 0.20, 2.50},
		{"composer-2.5", "composer-2.5", 0.50, 0.20, 2.50},
		{"composer-2.5-fast", "composer-2.5-fast", 3, 0.30, 15},
		// Cursor sends model="default" when the user picks Auto in
		// the model picker; Composer 2.5 Fast is the underlying
		// default backbone per cursor.com/blog/composer-2-5.
		{"default (cursor Auto)", "default", 3, 0.30, 15},
		// Family-prefix wins over miss: "composer-future-x" → composer
		// family rates ($0.50/$2.50).
		{"composer family fallback", "composer-future-x", 0.50, 0.20, 2.50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := tb.Lookup(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) returned ok=false", tc.model)
			}
			if p.Input != tc.in || p.Output != tc.out || p.CacheRead != tc.cacheR {
				t.Errorf("got %+v want input=%v cache_r=%v out=%v", p, tc.in, tc.cacheR, tc.out)
			}
		})
	}
}

// TestTable_Opus48ExplicitSourceExact pins that the v1.8.2 explicit
// claude-opus-4-8 row resolves as PricingSourceExact rather than the
// pre-pin family-fallback path (PricingSourceFamily via "claude-opus-4").
// Same rates either way ($5/$25), but the upgrade keeps the dashboard's
// "~" fallback badge off the flagship row AND gives a stable home for
// the FastMultiplier field (set on this entry only; not on the family
// prefix — fast mode is 4.8+ only).
func TestTable_Opus48ExplicitSourceExact(t *testing.T) {
	tb := NewTable()
	p, src, ok := tb.LookupWithSource("claude-opus-4-8")
	if !ok {
		t.Fatalf("Lookup claude-opus-4-8: ok=false")
	}
	if src != PricingSourceExact {
		t.Errorf("source=%q want exact (explicit row, not family fallback)", src)
	}
	if p.Input != 5 || p.Output != 25 || p.CacheRead != 0.50 ||
		p.CacheCreation != 6.25 || p.CacheCreation1h != 10 || p.WebSearchPerRequest != 0.01 {
		t.Errorf("rates: %+v", p)
	}
}

// TestTable_LastResortNormalization pins the §11.A central last-resort
// reducer in LookupWithSource: router-prefixed (capi:/sweagent-capi:) and
// provider-path-qualified (openrouter/anthropic/…) model strings that the
// exact + family ladders can't see past now resolve via one normalized retry
// (PricingSourceFamily) instead of silently billing $0. The "auto" router
// sentinel and the empty string still MISS (they name no real model — the
// cure is adapter-side, e.g. Copilot CLI Patch A). A curated host-rate
// provider-qualified key (deepseek/deepseek-v4-flash) still resolves EXACT,
// proving the last-resort step runs AFTER the curated lookups and never
// shadows them.
func TestTable_LastResortNormalization(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model string
		wantOK      bool
		wantSource  PricingSource
		wantInput   float64 // checked only when wantOK
		wantOutput  float64 // checked only when wantOK
	}{
		// capi: router prefix → strip → claude-sonnet-4 (was a MISS).
		{"capi prefix", "capi:claude-sonnet-4", true, PricingSourceFamily, 3, 15},
		{"sweagent-capi prefix", "sweagent-capi:claude-sonnet-4", true, PricingSourceFamily, 3, 15},
		// Provider path segments → drop all → claude-sonnet-4.6 (dot) routes
		// to the claude-sonnet-4 family (was a MISS via the openrouter/ head).
		{"provider path", "openrouter/anthropic/claude-sonnet-4.6", true, PricingSourceFamily, 3, 15},
		// Curated provider-qualified host rate still wins EXACT, not shadowed.
		{"curated host-rate key stays exact", "deepseek/deepseek-v4-flash", true, PricingSourceExact, 0.098, 0.197},
		// Router sentinels / empty name no model — still MISS.
		{"auto sentinel still miss", "auto", false, PricingSourceMiss, 0, 0},
		{"empty still miss", "", false, PricingSourceMiss, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(tc.model)
			if ok != tc.wantOK {
				t.Fatalf("LookupWithSource(%q): ok=%v want %v", tc.model, ok, tc.wantOK)
			}
			if src != tc.wantSource {
				t.Errorf("LookupWithSource(%q): source=%q want %q", tc.model, src, tc.wantSource)
			}
			if tc.wantOK && (p.Input != tc.wantInput || p.Output != tc.wantOutput) {
				t.Errorf("LookupWithSource(%q): got input=%v output=%v; want %v / %v",
					tc.model, p.Input, p.Output, tc.wantInput, tc.wantOutput)
			}
		})
	}
}

// TestTable_CodexFastModeMultiplier pins the Codex Fast mode
// (service_tier:"priority") per-SKU FastMultiplier added 2026-06-08:
// gpt-5.5 = 2.5×, gpt-5.4 = 2× (developers.openai.com/codex/speed + the
// Codex rate card, operator-confirmed). The multiplier is set ONLY on
// these two explicit SKUs — the family prefix + mini/nano/codex variants
// have no documented Fast tier and stay at 0 (standard pricing always
// applies). Also exercises ComputeBreakdown end-to-end so a fast turn
// bills the per-SKU premium. Inputs are kept below the 272K long-context
// threshold so the standard (not LC) rate is the baseline under test.
func TestTable_CodexFastModeMultiplier(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		model    string
		wantMult float64
	}{
		{"gpt-5.5", 2.5},
		{"gpt-5.4", 2},
		{"gpt-5.4-mini", 0}, // no documented Fast tier
		{"gpt-5.3-codex", 0},
		{"gpt-5", 0},
	} {
		p, ok := tb.Lookup(tc.model)
		if !ok {
			t.Fatalf("Lookup %q: ok=false", tc.model)
		}
		if p.FastMultiplier != tc.wantMult {
			t.Errorf("%s FastMultiplier = %v, want %v", tc.model, p.FastMultiplier, tc.wantMult)
		}
	}

	// End-to-end: a sub-threshold (100K, < 272K LC) input-only turn bills
	// the standard input rate at standard speed and the per-SKU premium
	// when Fast is set.
	for _, tc := range []struct {
		model    string
		wantStd  float64
		wantFast float64
	}{
		{"gpt-5.5", 0.50, 1.25}, // 100K × $5/1M = $0.50 → ×2.5 = $1.25
		{"gpt-5.4", 0.25, 0.50}, // 100K × $2.50/1M = $0.25 → ×2 = $0.50
	} {
		p, _ := tb.Lookup(tc.model)
		std := ComputeBreakdown(p, TokenBundle{Input: 100_000})
		if std.Total != tc.wantStd {
			t.Errorf("%s standard total = %v, want %v", tc.model, std.Total, tc.wantStd)
		}
		fast := ComputeBreakdown(p, TokenBundle{Input: 100_000, Fast: true})
		if fast.Total != tc.wantFast {
			t.Errorf("%s fast total = %v, want %v", tc.model, fast.Total, tc.wantFast)
		}
	}
}

// TestTable_OpenWeightFamilies2026Q2Pricing pins the v1.8.2 open-weight
// model additions. Anchored to first-party / OpenRouter representative
// rates per docs/plans/open-weight-models-pricing-review-2026-06-06.md
// Problem 1 recommendation (A). Per-family `:free` cases live in
// TestTable_FreeSuffixGuard (the suffix guard covers all of them).
//
// One row per family covering an explicit SKU + the family prefix
// (longest-prefix fallback). Cross-host variants live in
// TestTable_OpenRouterServedOpenWeightPricing once the §A6 commit lands.
func TestTable_OpenWeightFamilies2026Q2Pricing(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
		wantSource      PricingSource
	}{
		// OpenAI GPT-OSS (open weight).
		{"gpt-oss-120b", "gpt-oss-120b", 0.039, 0.0039, 0.18, PricingSourceExact},
		{"gpt-oss-20b", "gpt-oss-20b", 0.03, 0.003, 0.14, PricingSourceExact},
		{"gpt-oss family", "gpt-oss-unknown-sku", 0.039, 0.0039, 0.18, PricingSourceFamily},
		// Nvidia Nemotron 3 (canonical OpenRouter ids).
		{"nemotron-3-ultra-550b-a55b", "nemotron-3-ultra-550b-a55b", 0.50, 0.05, 2.50, PricingSourceExact},
		{"nemotron-3-super-120b-a12b", "nemotron-3-super-120b-a12b", 0.09, 0.009, 0.45, PricingSourceExact},
		{"nemotron-3-nano-30b-a3b", "nemotron-3-nano-30b-a3b", 0.04, 0.004, 0.15, PricingSourceExact},
		{"nemotron-3-ultra shorthand", "nemotron-3-ultra", 0.50, 0.05, 2.50, PricingSourceExact},
		{"nemotron family → super representative", "nemotron-unknown", 0.09, 0.009, 0.45, PricingSourceFamily},
		// Nous Hermes — flat in/out. hermes-4 rates are placeholder (= Hermes 3).
		{"hermes-3-llama-3.1-405b", "hermes-3-llama-3.1-405b", 1.00, 0.10, 1.00, PricingSourceExact},
		{"hermes-4-405b placeholder", "hermes-4-405b", 1.00, 0.10, 1.00, PricingSourceExact},
		{"hermes family", "hermes-future-sku", 1.00, 0.10, 1.00, PricingSourceFamily},
		// Alibaba Qwen — implicit cache = 20% of input on first-party.
		{"qwen3-max", "qwen3-max", 0.78, 0.156, 3.90, PricingSourceExact},
		{"qwen3-coder", "qwen3-coder", 1.50, 0.30, 7.50, PricingSourceExact},
		{"qwen family → qwen3-max", "qwen-unknown", 0.78, 0.156, 3.90, PricingSourceFamily},
		// Zhipu GLM — 5.1 latest; family prefix points at 5.1.
		{"glm-5", "glm-5", 1.00, 0.20, 3.20, PricingSourceExact},
		{"glm-5.1", "glm-5.1", 0.98, 0.182, 3.08, PricingSourceExact},
		{"glm family → 5.1", "glm-future", 0.98, 0.182, 3.08, PricingSourceFamily},
		// Mistral — batch 50% off known-unmodelled.
		{"mistral-large", "mistral-large", 2.00, 0.20, 6.00, PricingSourceExact},
		{"mistral-medium-3", "mistral-medium-3", 1.00, 0.10, 3.00, PricingSourceExact},
		{"mistral-small", "mistral-small", 0.15, 0.015, 0.60, PricingSourceExact},
		{"mistral family → medium-3", "mistral-future", 1.00, 0.10, 3.00, PricingSourceFamily},
		// MiniMax — family → m2.7 latest.
		{"minimax-m2.7", "minimax-m2.7", 0.279, 0.0279, 1.20, PricingSourceExact},
		{"minimax-m2.5", "minimax-m2.5", 0.15, 0.015, 1.15, PricingSourceExact},
		{"minimax family → m2.7", "minimax-future", 0.279, 0.0279, 1.20, PricingSourceFamily},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) ok=false", tc.model)
			}
			if src != tc.wantSource {
				t.Errorf("Lookup(%q) source=%q want %q", tc.model, src, tc.wantSource)
			}
			// Epsilon-tolerant comparisons. CacheRead defaults via
			// fillDefaults = 0.10 × Input, which produces representation
			// noise for non-binary-fraction inputs (e.g. 0.039, 0.279).
			if diff := p.Input - tc.in; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Lookup(%q) input=%v want %v", tc.model, p.Input, tc.in)
			}
			if diff := p.Output - tc.out; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Lookup(%q) output=%v want %v", tc.model, p.Output, tc.out)
			}
			if diff := p.CacheRead - tc.cacheR; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Lookup(%q) cache_read=%v want %v", tc.model, p.CacheRead, tc.cacheR)
			}
			// Auto-cache shape — no separate cache-write charge.
			if p.CacheCreation != 0 {
				t.Errorf("Lookup(%q) CacheCreation=%v want 0 (auto-cache shape)",
					tc.model, p.CacheCreation)
			}
		})
	}
}

// TestTable_OpenRouterServedOpenWeightPricing pins the v1.8.2 OpenRouter
// open-weight catalog snapshot. String keys exactly as OpenRouter emits
// them — see provider-model-price-catalog-2026-06-06.md §14 + brief A6.
// Each row resolves PricingSourceExact (verbatim match wins before any
// prefix strip can pull the lookup onto a bare-id row). The host-anchor
// policy from open-weight-models-pricing-review-2026-06-06.md is what
// makes this safe: bare ids anchor to first-party / representative rate,
// provider-qualified ids preserve the per-host delta.
func TestTable_OpenRouterServedOpenWeightPricing(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
	}{
		// OpenAI GPT-OSS via OpenRouter.
		{"openai/gpt-oss-120b", "openai/gpt-oss-120b", 0.039, 0.0039, 0.18},
		{"openai/gpt-oss-20b", "openai/gpt-oss-20b", 0.03, 0.003, 0.14},
		// Nvidia Nemotron 3 via OpenRouter.
		{"nvidia/nemotron-3-ultra-550b-a55b", "nvidia/nemotron-3-ultra-550b-a55b", 0.50, 0.05, 2.50},
		{"nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3-super-120b-a12b", 0.09, 0.009, 0.45},
		// Nous Hermes via OpenRouter.
		{"nousresearch/hermes-3-llama-3.1-405b", "nousresearch/hermes-3-llama-3.1-405b", 1.00, 0.10, 1.00},
		{"nousresearch/hermes-4-405b", "nousresearch/hermes-4-405b", 1.00, 0.10, 1.00},
		// Alibaba Qwen via OpenRouter (qwen/ prefix != bare qwen3-* first-party).
		{"qwen/qwen3.7-max", "qwen/qwen3.7-max", 1.25, 0.25, 3.75},
		{"qwen/qwen3.6-flash", "qwen/qwen3.6-flash", 0.1875, 0.01875, 1.125},
		// Zhipu GLM via OpenRouter (z-ai/ prefix).
		{"z-ai/glm-5.1", "z-ai/glm-5.1", 0.98, 0.182, 3.08},
		{"z-ai/glm-5-turbo", "z-ai/glm-5-turbo", 1.20, 0.24, 4.00},
		// Mistral via OpenRouter.
		{"mistralai/mistral-small-2603", "mistralai/mistral-small-2603", 0.15, 0.015, 0.60},
		{"mistralai/mistral-medium-3-5", "mistralai/mistral-medium-3-5", 1.50, 0.15, 7.50},
		// MiniMax via OpenRouter.
		{"minimax/minimax-m2.7", "minimax/minimax-m2.7", 0.26, 0.026, 1.20},
		// xAI via OpenRouter — multi-agent variant priced higher than 4.3 base.
		{"x-ai/grok-4.3", "x-ai/grok-4.3", 1.25, 0.20, 2.50},
		{"x-ai/grok-4.20-multi-agent", "x-ai/grok-4.20-multi-agent", 2.00, 0.20, 6.00},
		{"x-ai/grok-build-0.1", "x-ai/grok-build-0.1", 1.00, 0.20, 2.00},
		// Moonshot via OpenRouter.
		{"moonshotai/kimi-k2.6", "moonshotai/kimi-k2.6", 0.684, 0.144, 3.42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) ok=false", tc.model)
			}
			if src != PricingSourceExact {
				t.Errorf("Lookup(%q) source=%q want exact (provider-qualified, verbatim match)",
					tc.model, src)
			}
			if diff := p.Input - tc.in; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Lookup(%q) input=%v want %v", tc.model, p.Input, tc.in)
			}
			if diff := p.Output - tc.out; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Lookup(%q) output=%v want %v", tc.model, p.Output, tc.out)
			}
			if diff := p.CacheRead - tc.cacheR; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("Lookup(%q) cache_read=%v want %v", tc.model, p.CacheRead, tc.cacheR)
			}
		})
	}
}

// TestTable_OllamaLocalFreeFamily pins the explicit `ollama` family-prefix
// row added in v1.8.2. Local inference runs on user hardware → $0
// per-token, but pre-this-pin `ollama/gemma3:1b`-shape strings hit
// PricingSourceMiss and the cost rollup tagged the row reliability as
// "unknown" — visually noisy on dashboards. With the row in place, the
// family-prefix ladder catches `ollama/<anything>`, returns Pricing{0,0}
// as PricingSourceFamily, and the rollup tags reliability as
// "approximate" (known-priced model with a zero rate).
//
// Also pins the Engine.Compute contract: ok=true on a known-$0 row
// (only PricingSourceMiss returns ok=false; explicit / date-stripped /
// family all return ok=true regardless of rate magnitude).
func TestTable_OllamaLocalFreeFamily(t *testing.T) {
	tb := NewTable()
	for _, model := range []string{
		"ollama/gemma4:e4b",
		"ollama/gemma3:1b",
		"ollama/qwen3-coder:30b",
		"ollama/some-future-model",
		"ollama", // bare family id resolves exact.
	} {
		t.Run(model, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(model)
			if !ok {
				t.Fatalf("Lookup(%q) ok=false; ollama should resolve via family",
					model)
			}
			if src == PricingSourceMiss {
				t.Errorf("Lookup(%q) source=miss; want exact or family", model)
			}
			if p.Input != 0 || p.Output != 0 {
				t.Errorf("Lookup(%q) non-zero rates: %+v", model, p)
			}
		})
	}

	// Engine-level: Compute returns ok=true on known-$0 rows.
	e := NewEngine(emptyIntelConfigForTest())
	cost, ok := e.Compute("ollama/gemma3:1b", TokenBundle{Input: 1_000_000, Output: 500_000})
	if !ok {
		t.Errorf("Compute(ollama/...) ok=false; known-$0 must return ok=true")
	}
	if cost != 0 {
		t.Errorf("Compute(ollama/...) cost=%v; want 0", cost)
	}
}

// emptyIntelConfigForTest returns a zero IntelligenceConfig — the engine
// boots with only baked-in defaults. Test-only helper so the ollama
// engine-level case doesn't import config in the file's main body.
func emptyIntelConfigForTest() config.IntelligenceConfig {
	return config.IntelligenceConfig{}
}

// TestTable_DeepSeek2026Q2Pricing pins the v1.8.2 DeepSeek V4 rates
// (api-docs.deepseek.com snapshot 2026-06-06). Cache hit → CacheRead;
// no separate cache-write charge (auto-cache, OpenAI-shape). V4-Pro's
// $0.435 input is the 75%-off rate made permanent 2026-05-22.
//
// Pins BOTH the first-party rate (bare model id) AND the OpenRouter-
// served rate (provider-qualified key) so the host-variance is
// captured cleanly. The two must NOT collapse — that would silently
// drop the 30% OpenRouter delta on v4-flash.
func TestTable_DeepSeek2026Q2Pricing(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
	}{
		// First-party (bare ids).
		{"v4-flash", "deepseek-v4-flash", 0.14, 0.0028, 0.28},
		{"v4-pro", "deepseek-v4-pro", 0.435, 0.003625, 0.87},
		{"chat alias → v4-flash", "deepseek-chat", 0.14, 0.0028, 0.28},
		{"reasoner alias → v4-flash", "deepseek-reasoner", 0.14, 0.0028, 0.28},
		{"v4 family → flash", "deepseek-v4", 0.14, 0.0028, 0.28},
		{"deepseek family → flash", "deepseek", 0.14, 0.0028, 0.28},
		// OpenRouter-served (provider-qualified) — different rates,
		// must NOT collapse to the bare rate via prefix strip.
		{"OR v4-flash 30% off", "deepseek/deepseek-v4-flash", 0.098, 0.0197, 0.197},
		{"OR v4-pro matches FP", "deepseek/deepseek-v4-pro", 0.435, 0.0036, 0.87},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) ok=false", tc.model)
			}
			if src != PricingSourceExact {
				t.Errorf("Lookup(%q) source=%q want exact", tc.model, src)
			}
			if p.Input != tc.in {
				t.Errorf("Lookup(%q) input=%v want %v", tc.model, p.Input, tc.in)
			}
			if p.Output != tc.out {
				t.Errorf("Lookup(%q) output=%v want %v", tc.model, p.Output, tc.out)
			}
			if p.CacheRead != tc.cacheR {
				t.Errorf("Lookup(%q) cache_read=%v want %v", tc.model, p.CacheRead, tc.cacheR)
			}
			// No cache_write — OpenAI-shape, auto-cache.
			if p.CacheCreation != 0 || p.CacheCreation1h != 0 {
				t.Errorf("Lookup(%q): CacheCreation should stay 0 (auto-cache shape): %+v",
					tc.model, p)
			}
		})
	}
	// Cross-host delta: provider-qualified MUST differ from bare for flash.
	pBare, _ := tb.Lookup("deepseek-v4-flash")
	pOR, _ := tb.Lookup("deepseek/deepseek-v4-flash")
	if pBare.Input == pOR.Input || pBare.Output == pOR.Output {
		t.Errorf("v4-flash bare ($%v/$%v) and OpenRouter ($%v/$%v) MUST differ",
			pBare.Input, pBare.Output, pOR.Input, pOR.Output)
	}
}

// TestTable_FreeSuffixGuard pins the `:free` suffix guard added to
// LookupWithSource. Open-weight free tiers on OpenRouter / Kilo Gateway
// (nemotron-3-super-120b-a12b:free, gpt-oss-120b:free,
// deepseek/deepseek-v4-flash:free, hermes-4-405b:free, etc.) genuinely
// cost $0 — but the moment any paid `<family>` row is added, a
// `<family>:free` string would fall through the date-strip and
// family-prefix ladder and inherit the paid family rate. The guard
// returns Pricing{Input:0, Output:0} as PricingSourceExact (so cost
// rollups tag the row as known-$0, not "unknown" or "approximate")
// for ANY model id ending in `:free`, BEFORE the ladder runs.
//
// The verbatim exact-match check still wins, so a user can pin a
// non-zero rate by adding an explicit `<model>:free` entry above —
// the guard is a defensive default, not a hard override.
func TestTable_FreeSuffixGuard(t *testing.T) {
	tb := NewTable()
	for _, model := range []string{
		"nemotron-3-super-120b-a12b:free",
		"gpt-oss-120b:free",
		"hermes-4-405b:free",
		"deepseek-v4-flash:free",
		// Provider-qualified shape (OpenRouter ids):
		"deepseek/deepseek-v4-flash:free",
		"nvidia/nemotron-3-super-120b-a12b:free",
		"moonshotai/kimi-k2.6:free",
		"openai/gpt-oss-120b:free",
		// Case variations: OpenRouter sometimes capitalises in user-
		// supplied catalog mirrors. The guard lowercases before suffix
		// check, so :Free / :FREE resolve identically.
		"some-model:Free",
		"another-model:FREE",
	} {
		t.Run(model, func(t *testing.T) {
			p, src, ok := tb.LookupWithSource(model)
			if !ok {
				t.Fatalf("LookupWithSource(%q) ok=false; want ok=true", model)
			}
			if src != PricingSourceExact {
				t.Errorf("LookupWithSource(%q) source=%q want exact (known-$0, not family fallback)",
					model, src)
			}
			if p.Input != 0 || p.Output != 0 || p.CacheRead != 0 {
				t.Errorf("LookupWithSource(%q) rates non-zero: %+v", model, p)
			}
		})
	}
}

// TestTable_FreeSuffixGuardOverridable verifies the guard doesn't block
// an explicit override. Users who want to pin a non-zero `:free` rate
// (e.g. a private host that's about to stop being free) add an explicit
// row; the verbatim exact-match check above the guard returns that row.
func TestTable_FreeSuffixGuardOverridable(t *testing.T) {
	tb := NewTable()
	tb.Merge(map[string]Pricing{
		"some-private-host/model:free": {Input: 0.50, Output: 1.50},
	})
	p, src, ok := tb.LookupWithSource("some-private-host/model:free")
	if !ok || src != PricingSourceExact {
		t.Fatalf("explicit override: ok=%v src=%q want ok=true exact", ok, src)
	}
	if p.Input != 0.50 || p.Output != 1.50 {
		t.Errorf("explicit override rates lost: %+v", p)
	}
}

// TestTable_ProviderQualifiedExactWinsOverBare pins the lookup-precedence
// invariant the open-weight pricing additions rely on: a verbatim
// `<provider>/<model>` row resolves as PricingSourceExact BEFORE any
// date-strip / family-prefix reduction can pull it onto the bare
// `<model>` rate. This is what lets DeepSeek's first-party
// (deepseek-v4-flash $0.14/$0.28) and OpenRouter-served
// (deepseek/deepseek-v4-flash $0.098/$0.197) rows coexist with
// different rates — same model string after stripProviderPrefix, but
// the adapter that emits the qualified id (e.g. clinecli passing
// `deepseek/deepseek-v4-flash` through verbatim) bills at the
// OpenRouter rate; an adapter that pre-strips to the bare id bills at
// the first-party rate.
//
// Uses a synthetic Table rather than the baked-in defaults so a future
// pricing-table edit can't accidentally pass this test by changing the
// rates to coincide.
func TestTable_ProviderQualifiedExactWinsOverBare(t *testing.T) {
	tb := &Table{exact: map[string]Pricing{
		"some-model":           {Input: 1.00, Output: 5.00},
		"some-host/some-model": {Input: 0.50, Output: 2.50}, // host serves it cheaper
	}}
	// Bare id: first-party rate.
	pBare, srcBare, ok := tb.LookupWithSource("some-model")
	if !ok || srcBare != PricingSourceExact {
		t.Fatalf("bare: ok=%v src=%q want ok=true exact", ok, srcBare)
	}
	if pBare.Input != 1.00 || pBare.Output != 5.00 {
		t.Errorf("bare rates: %+v want $1/$5", pBare)
	}
	// Provider-qualified: host rate, NOT bare rate via prefix-strip.
	pHost, srcHost, ok := tb.LookupWithSource("some-host/some-model")
	if !ok || srcHost != PricingSourceExact {
		t.Fatalf("host: ok=%v src=%q want ok=true exact", ok, srcHost)
	}
	if pHost.Input != 0.50 || pHost.Output != 2.50 {
		t.Errorf("host rates lost to prefix-strip: %+v want $0.50/$2.50", pHost)
	}
	// The two MUST differ — proves the lookup didn't collapse them.
	if pBare.Input == pHost.Input || pBare.Output == pHost.Output {
		t.Errorf("bare and host rates collapsed: bare=%+v host=%+v", pBare, pHost)
	}
}

func TestTable_LookupDateStripped(t *testing.T) {
	tb := &Table{exact: map[string]Pricing{
		"claude-test-4": {Input: 2, Output: 8},
	}}
	// Date-suffixed variants fall back to the bare family entry.
	p, ok := tb.Lookup("claude-test-4-20250101")
	if !ok {
		t.Fatalf("expected date-stripped fallback")
	}
	if p.Input != 2 {
		t.Errorf("got %+v", p)
	}
}

func TestTable_LookupPrefix(t *testing.T) {
	tb := &Table{exact: map[string]Pricing{
		"custom-family-v2":    {Input: 1, Output: 2},
		"custom-family-v2-sm": {Input: 0.5, Output: 1},
	}}
	// Longer prefix wins when both match.
	p, ok := tb.Lookup("custom-family-v2-sm-20260101")
	if !ok {
		t.Fatalf("expected match")
	}
	if p.Input != 0.5 {
		t.Errorf("expected longer-prefix pricing, got %+v", p)
	}
}

func TestTable_LookupUnknown(t *testing.T) {
	tb := NewTable()
	if _, ok := tb.Lookup("some-bogus-model-xyz"); ok {
		t.Errorf("unknown model should not match")
	}
	if _, ok := tb.Lookup(""); ok {
		t.Errorf("empty model should not match")
	}
}

func TestFillDefaults_CacheFromInput(t *testing.T) {
	// Only Input set — CacheRead defaults to 10% (universal across
	// providers). CacheCreation stays 0 (Anthropic-only concept; we
	// don't speculatively assign cache_write rates to non-Anthropic
	// entries — that would over-bill if a stray cache_creation_tokens
	// ever appeared on an OpenAI/Gemini row). See pricing.go
	// fillDefaults doc comment.
	p := fillDefaults(Pricing{Input: 10, Output: 50})
	if p.CacheRead != 1 {
		t.Errorf("CacheRead default wrong: %+v", p)
	}
	if p.CacheCreation != 0 {
		t.Errorf("CacheCreation should NOT be defaulted (non-Anthropic shape): %+v", p)
	}
	if p.CacheCreation1h != 0 {
		t.Errorf("CacheCreation1h should NOT be defaulted without explicit CacheCreation: %+v", p)
	}
}

func TestFillDefaults_RespectsExplicit(t *testing.T) {
	p := fillDefaults(Pricing{Input: 10, Output: 50, CacheRead: 0.05, CacheCreation: 9})
	if p.CacheRead != 0.05 || p.CacheCreation != 9 {
		t.Errorf("explicit cache rates overwritten: %+v", p)
	}
}

func TestTable_Merge(t *testing.T) {
	tb := NewTable()
	tb.Merge(map[string]Pricing{
		"claude-sonnet-4-20250514": {Input: 99, Output: 999},
	})
	p, _ := tb.Lookup("claude-sonnet-4-20250514")
	if p.Input != 99 || p.Output != 999 {
		t.Errorf("override not applied: %+v", p)
	}
}

func TestCompute_Basic(t *testing.T) {
	p := Pricing{Input: 3, Output: 15, CacheRead: 0.3, CacheCreation: 3.75}
	got := Compute(p, TokenBundle{
		Input:         1_000_000,
		Output:        500_000,
		CacheRead:     200_000,
		CacheCreation: 100_000,
	})
	// 3 + 7.5 + 0.06 + 0.375 = 10.935
	want := 10.935
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Compute: got %v want %v", got, want)
	}
}

func TestCompute_ZeroTokens(t *testing.T) {
	if got := Compute(Pricing{Input: 3, Output: 15}, TokenBundle{}); got != 0 {
		t.Errorf("zero tokens: got %v", got)
	}
}

func TestStripDateSuffix(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"claude-sonnet-4-20250514", "claude-sonnet-4"},
		{"claude-sonnet-4", "claude-sonnet-4"},
		{"gpt-4o", "gpt-4o"},
		{"x-2025", "x-2025"},
		{"x-99990101", "x"},
	}
	for _, tc := range cases {
		if got := stripDateSuffix(tc.in); got != tc.out {
			t.Errorf("stripDateSuffix(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

// TestTable_OpusUniformPricingNoLongContext pins audit F2 (2026-06-08):
// Opus has UNIFORM pricing with NO context-size tier, even on the 1M-context
// beta (platform.claude.com/docs/en/about-claude/pricing, operator-confirmed).
// The `[1m]` suffix that claude-code / cowork append for 1M-context turns
// therefore resolves — via the family-prefix fallback — to the SAME flat
// rates as the base SKU, never a long-context premium. This guards against
// (a) someone adding a phantom Opus LC tier, and (b) a future auditor
// re-opening the "is Opus [1m] under-billed above 200K?" question.
func TestTable_OpusUniformPricingNoLongContext(t *testing.T) {
	tbl := NewTable()
	// A 300K-prompt turn (well past the 200K Sonnet threshold) must cost the
	// same on the [1m] variant as on the base SKU — proving no LC dispatch.
	bundle := TokenBundle{Input: 300_000, Output: 5_000}
	for _, base := range []string{"claude-opus-4-7", "claude-opus-4-8", "claude-opus-4-6"} {
		bp, _, ok := tbl.LookupWithSource(base)
		if !ok {
			t.Fatalf("%s: not priced", base)
		}
		if bp.LongContextThreshold != 0 {
			t.Errorf("%s: LongContextThreshold=%d, want 0 (Opus is uniform by context)", base, bp.LongContextThreshold)
		}
		lp, _, ok := tbl.LookupWithSource(base + "[1m]")
		if !ok {
			t.Fatalf("%s[1m]: not priced", base)
		}
		if lp.Input != bp.Input || lp.Output != bp.Output || lp.CacheRead != bp.CacheRead {
			t.Errorf("%s[1m] rates (%v/%v/%v) != base (%v/%v/%v) — Opus 1M-context is flat-priced",
				base, lp.Input, lp.Output, lp.CacheRead, bp.Input, bp.Output, bp.CacheRead)
		}
		if got, want := Compute(lp, bundle), Compute(bp, bundle); got != want {
			t.Errorf("%s[1m] 300K-prompt cost %.6f != base %.6f", base, got, want)
		}
	}
}
