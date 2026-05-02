package cost

import "testing"

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
		// Hypothetical future SKUs — must inherit the LATEST family rates,
		// not the legacy. claude-opus-4-8 should be priced like Opus 4.7,
		// not Opus 4. (Family prefix "claude-opus-4" is set to current
		// rates so this works out of the box.)
		{"future opus-4-8 inherits current", "claude-opus-4-8", 5, 25, 0.50, 6.25, 10},
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
		{"gemini-2.5-pro", "gemini-2.5-pro", 1.25, 0.125, 10},
		{"gemini-2.5-flash", "gemini-2.5-flash", 0.30, 0.03, 2.50},
		{"gemini-2.5-flash-lite", "gemini-2.5-flash-lite", 0.10, 0.01, 0.40},
		{"gemini-2.0-flash deprecated", "gemini-2.0-flash", 0.10, 0.025, 0.40},
		// Family fallback: future SKU "gemini-3.1-something" inherits
		// gemini-3.1 family prefix (same as 3.1-pro-preview rates).
		{"gemini-3.1 family fallback", "gemini-3.1-something-new", 2, 0.20, 12},
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

// TestTable_OtherProviderPricing covers xAI / Moonshot / Cursor own.
func TestTable_OtherProviderPricing(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		name, model     string
		in, cacheR, out float64
	}{
		{"grok-4-20", "grok-4-20", 2, 0.20, 6},
		{"grok family fallback", "grok-5", 2, 0.20, 6},
		{"kimi-k2-5", "kimi-k2-5", 0.60, 0.10, 3},
		{"composer-1", "composer-1", 1.25, 0.125, 10},
		{"composer-1.5", "composer-1.5", 3.50, 0.35, 17.50},
		{"composer-2", "composer-2", 0.50, 0.20, 2.50},
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
