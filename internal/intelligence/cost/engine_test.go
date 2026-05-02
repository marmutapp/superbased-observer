package cost

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

func TestNewEngine_MergesConfigOverrides(t *testing.T) {
	cfg := config.IntelligenceConfig{
		Pricing: config.PricingConfig{Models: map[string]config.ModelPricing{
			"claude-sonnet-4-20250514": {Input: 100, Output: 200, CacheRead: 10, CacheCreation: 50},
			"my-local-llm":             {Input: 0, Output: 0},
		}},
	}
	e := NewEngine(cfg)

	// Override takes effect.
	p, ok := e.Lookup("claude-sonnet-4-20250514")
	if !ok || p.Input != 100 || p.Output != 200 {
		t.Errorf("override not applied: %+v ok=%v", p, ok)
	}

	// Baked-in entries that weren't overridden remain.
	if _, ok := e.Lookup("claude-opus-4-20250514"); !ok {
		t.Errorf("baked-in opus missing after merge")
	}

	// Zero-only entry is present but Compute returns (0, true) — the model
	// was registered, just at zero cost (some users run local models free
	// of charge).
	cost, ok := e.Compute("my-local-llm", TokenBundle{Input: 1000, Output: 1000})
	if !ok {
		t.Errorf("my-local-llm lookup should succeed")
	}
	if cost != 0 {
		t.Errorf("expected zero cost, got %v", cost)
	}
}

func TestEngine_Compute_UnknownModel(t *testing.T) {
	e := NewEngine(config.IntelligenceConfig{})
	if _, ok := e.Compute("definitely-fake-model-99", TokenBundle{Input: 1000}); ok {
		t.Errorf("unknown model should return ok=false")
	}
}

func TestTokenBundle_Add(t *testing.T) {
	a := TokenBundle{Input: 1, Output: 2, CacheRead: 3, CacheCreation: 4, CacheCreation1h: 1}
	b := TokenBundle{Input: 10, Output: 20, CacheRead: 30, CacheCreation: 40, CacheCreation1h: 5}
	a.Add(b)
	if a.Input != 11 || a.Output != 22 || a.CacheRead != 33 || a.CacheCreation != 44 || a.CacheCreation1h != 6 {
		t.Errorf("Add wrong: %+v", a)
	}
}

// TestCompute_CacheCreationTierSplit verifies that the 1h ephemeral
// subset of cache_creation is billed at the 1h rate and the remainder at
// the 5m rate. Anthropic prices 1h-tier at 2× the 5m rate; the engine
// must apply both bands and never charge the 1h portion at the cheaper
// 5m rate.
func TestCompute_CacheCreationTierSplit(t *testing.T) {
	p := Pricing{
		Input:           3,
		Output:          15,
		CacheRead:       0.30,
		CacheCreation:   3.75,
		CacheCreation1h: 7.50, // 2 × 5m rate, mirroring Anthropic public pricing
	}
	// 1M total cache_creation tokens; 400k of them landed in 1h tier.
	// 5m portion = 600k × 3.75 / 1M = 2.25
	// 1h portion = 400k × 7.50 / 1M = 3.00
	// Expected total = 5.25
	got := Compute(p, TokenBundle{
		CacheCreation:   1_000_000,
		CacheCreation1h: 400_000,
	})
	want := 5.25
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Compute tier split: got %v want %v", got, want)
	}

	// Pre-tier-aware data has CacheCreation1h=0 → entirely 5m rate.
	got = Compute(p, TokenBundle{CacheCreation: 1_000_000})
	want = 3.75
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Compute pre-tier (no 1h): got %v want %v", got, want)
	}
}

// TestCompute_CacheCreation1hClamps guards against malformed input where
// the 1h portion exceeds the total — the engine clamps rather than
// double-counting.
func TestCompute_CacheCreation1hClamps(t *testing.T) {
	p := Pricing{Input: 3, Output: 15, CacheCreation: 3.75, CacheCreation1h: 7.50}
	// 1h > total: clamp 1h down to the total, 5m portion becomes 0.
	got := Compute(p, TokenBundle{CacheCreation: 100_000, CacheCreation1h: 500_000})
	// Expected: 100k × 7.50 / 1M = 0.75 (entire bundle billed at 1h rate).
	want := 0.75
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Compute clamp: got %v want %v", got, want)
	}

	// Negative 1h: treat as zero (defensive).
	got = Compute(p, TokenBundle{CacheCreation: 100_000, CacheCreation1h: -50_000})
	want = 0.375 // entire bundle at 5m rate
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Compute negative 1h: got %v want %v", got, want)
	}
}

// TestCompute_LCTier_BelowThreshold verifies that prompt windows under
// LongContextThreshold price at standard rates — the LC fields don't
// kick in until the threshold is exceeded.
func TestCompute_LCTier_BelowThreshold(t *testing.T) {
	p := Pricing{
		Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6,
		LongContextThreshold:     200_000,
		LongContextInput:         6,
		LongContextOutput:        22.50,
		LongContextCacheRead:     0.60,
		LongContextCacheCreation: 7.50, LongContextCacheCreation1h: 12,
	}
	// 199_999 prompt — exactly at threshold (>=, not >). LC must NOT
	// kick in: standard rate 3 × 199_999 / 1M = 0.599997.
	got := Compute(p, TokenBundle{Input: 199_999})
	want := 199_999 * 3.0 / 1_000_000
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("at-threshold-minus-1: got %v want %v", got, want)
	}
	// Threshold equality: still standard rate (the boundary is strict >).
	got = Compute(p, TokenBundle{Input: 200_000})
	want = 200_000 * 3.0 / 1_000_000
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("at-threshold-exact: got %v want %v", got, want)
	}
}

// TestCompute_LCTier_AboveThreshold pins the dispatch behavior for a
// Sonnet 4 / 4.5 turn that clears the 200K threshold. Every dimension
// reprices at the LC rate, including the 1h cache-creation tier.
func TestCompute_LCTier_AboveThreshold(t *testing.T) {
	p := Pricing{
		Input: 3, Output: 15, CacheRead: 0.30, CacheCreation: 3.75, CacheCreation1h: 6,
		LongContextThreshold:     200_000,
		LongContextInput:         6,
		LongContextOutput:        22.50,
		LongContextCacheRead:     0.60,
		LongContextCacheCreation: 7.50, LongContextCacheCreation1h: 12,
	}
	// Prompt window = 100K + 50K + 100K = 250K → > 200K threshold.
	// LC pricing applies to every dimension:
	//   input        100K × $6     / 1M = 0.60
	//   output        50K × $22.50 / 1M = 1.125
	//   cache_read   200K × $0.60  / 1M = 0.12
	//   cc_5m         50K × $7.50  / 1M = 0.375  (100K total - 50K 1h)
	//   cc_1h         50K × $12    / 1M = 0.60
	// Total = 2.82
	got := Compute(p, TokenBundle{
		Input: 100_000, Output: 50_000,
		CacheRead: 200_000, CacheCreation: 100_000, CacheCreation1h: 50_000,
	})
	want := 2.82
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("LC rates above threshold: got %v want %v", got, want)
	}
}

// TestCompute_LCTier_ZeroFieldFallsBack verifies that when an LC override
// is left at zero on one dimension, that dimension uses the standard rate
// while other dimensions still get the LC override. Lets a Pricing entry
// pin only the rates that change at the LC tier (e.g. Sonnet's output
// goes 1.5× while every other dim goes 2×).
func TestCompute_LCTier_ZeroFieldFallsBack(t *testing.T) {
	// Hypothetical: only LC input overridden; output stays at standard $15.
	p := Pricing{
		Input: 3, Output: 15,
		LongContextThreshold: 200_000,
		LongContextInput:     6, // override input only
		// LongContextOutput intentionally zero → falls back to 15.
	}
	got := Compute(p, TokenBundle{Input: 300_000, Output: 100_000})
	// 300K × $6 + 100K × $15 = 1.80 + 1.50 = 3.30
	want := 3.30
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("LC partial override: got %v want %v", got, want)
	}
}

// TestCompute_LCTier_ThresholdZeroDisables guards that an entry without
// an LC threshold (the default for non-LC providers) never reprices,
// even at huge prompt sizes.
func TestCompute_LCTier_ThresholdZeroDisables(t *testing.T) {
	p := Pricing{Input: 3, Output: 15} // no LC fields set
	got := Compute(p, TokenBundle{Input: 10_000_000, Output: 1_000_000})
	want := 10_000_000*3.0/1_000_000 + 1_000_000*15.0/1_000_000 // 30 + 15 = 45
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("no LC threshold should never reprice: got %v want %v", got, want)
	}
}

// TestCompute_LCTier_PromptIncludesCacheRead verifies the threshold is
// compared against (Input + CacheRead + CacheCreation), not just Input.
// A turn where most of the prompt is cached must still trigger LC if
// the total prompt window crosses the threshold — that's how Anthropic
// and OpenAI bill the LC tier.
func TestCompute_LCTier_PromptIncludesCacheRead(t *testing.T) {
	p := Pricing{
		Input: 3, Output: 15, CacheRead: 0.30,
		LongContextThreshold: 200_000,
		LongContextInput:     6, LongContextOutput: 22.50, LongContextCacheRead: 0.60,
	}
	// Input alone = 50K (under threshold), but prompt window =
	// 50K + 200K cached = 250K → triggers LC.
	got := Compute(p, TokenBundle{Input: 50_000, Output: 10_000, CacheRead: 200_000})
	// LC: 50K × $6 + 10K × $22.50 + 200K × $0.60 / 1M
	//   = 0.30 + 0.225 + 0.12 = 0.645
	want := 0.645
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("LC by cached prompt window: got %v want %v", got, want)
	}
}

// TestTable_LongContextDefaults pins the LC entries baked into
// defaultPricing. When Anthropic / OpenAI / Google publish new LC
// rates or thresholds, update both this test and the matching
// docs/pricing-reference.md section.
func TestTable_LongContextDefaults(t *testing.T) {
	tb := NewTable()
	for _, tc := range []struct {
		model             string
		threshold         int64
		lcIn, lcOut, lcCR float64
		lcCC, lcCC1h      float64 // 0 if not applicable (non-Anthropic)
	}{
		{"claude-sonnet-4-5", 200_000, 6, 22.50, 0.60, 7.50, 12},
		{"claude-sonnet-4-20250514", 200_000, 6, 22.50, 0.60, 7.50, 12},
		{"claude-sonnet-4", 200_000, 6, 22.50, 0.60, 7.50, 12},
		{"gpt-5.4", 272_000, 5, 30, 0.50, 0, 0},
		{"gpt-5.5", 272_000, 10, 60, 1, 0, 0},
		{"gemini-2.5-pro", 200_000, 2.50, 20, 0.25, 0, 0},
		{"gemini-3.1-pro-preview", 200_000, 4, 24, 0.40, 0, 0},
	} {
		t.Run(tc.model, func(t *testing.T) {
			p, ok := tb.Lookup(tc.model)
			if !ok {
				t.Fatalf("Lookup(%q) returned ok=false", tc.model)
			}
			if p.LongContextThreshold != tc.threshold {
				t.Errorf("threshold: got %v want %v", p.LongContextThreshold, tc.threshold)
			}
			if p.LongContextInput != tc.lcIn {
				t.Errorf("lc_input: got %v want %v", p.LongContextInput, tc.lcIn)
			}
			if p.LongContextOutput != tc.lcOut {
				t.Errorf("lc_output: got %v want %v", p.LongContextOutput, tc.lcOut)
			}
			if p.LongContextCacheRead != tc.lcCR {
				t.Errorf("lc_cache_read: got %v want %v", p.LongContextCacheRead, tc.lcCR)
			}
			if p.LongContextCacheCreation != tc.lcCC {
				t.Errorf("lc_cache_creation: got %v want %v", p.LongContextCacheCreation, tc.lcCC)
			}
			if p.LongContextCacheCreation1h != tc.lcCC1h {
				t.Errorf("lc_cache_creation_1h: got %v want %v", p.LongContextCacheCreation1h, tc.lcCC1h)
			}
		})
	}

	// Sonnet 4.6 is flat-rate per the 2026-04-29 snapshot — no LC tier.
	p, _ := tb.Lookup("claude-sonnet-4-6")
	if p.LongContextThreshold != 0 {
		t.Errorf("sonnet-4-6 should be flat-rate, got threshold %v", p.LongContextThreshold)
	}
	// gpt-5.4-mini has no public LC numbers — flat-rate for now.
	p, _ = tb.Lookup("gpt-5.4-mini")
	if p.LongContextThreshold != 0 {
		t.Errorf("gpt-5.4-mini should be flat-rate, got threshold %v", p.LongContextThreshold)
	}
}

// TestEngine_ReloadSwapsTable verifies the Settings-page hot-reload
// path: building a new IntelligenceConfig and calling Reload swaps the
// active pricing in place, in-flight callers see consistent rates, and
// previously-cached *Table snapshots stay safe to read.
func TestEngine_ReloadSwapsTable(t *testing.T) {
	e := NewEngine(config.IntelligenceConfig{})
	// Default pricing for sonnet 4.6 is $3 input.
	pBefore, ok := e.Lookup("claude-sonnet-4-6")
	if !ok || pBefore.Input != 3 {
		t.Fatalf("baseline lookup: got %+v ok=%v want input=3", pBefore, ok)
	}
	// Snapshot the pre-reload table — must remain valid for reads after
	// Reload because we use atomic.Pointer (immutable Tables).
	snap := e.Table()

	// Reload with an override.
	e.Reload(config.IntelligenceConfig{
		Pricing: config.PricingConfig{Models: map[string]config.ModelPricing{
			"claude-sonnet-4-6": {Input: 99, Output: 999},
		}},
	})
	pAfter, ok := e.Lookup("claude-sonnet-4-6")
	if !ok || pAfter.Input != 99 || pAfter.Output != 999 {
		t.Errorf("post-reload lookup: got %+v want input=99 output=999", pAfter)
	}
	// Pre-reload snapshot still works — atomic.Pointer guarantees reads
	// against the old *Table never tear, so a goroutine that grabbed
	// the table before Reload finishes its work without panicking.
	pSnap, ok := snap.Lookup("claude-sonnet-4-6")
	if !ok || pSnap.Input != 3 {
		t.Errorf("snapshot stale-read: got %+v ok=%v want input=3", pSnap, ok)
	}
}

// TestEngine_ReloadConcurrentSafe runs many concurrent Lookups against
// a Reload loop. Pure smoke test for the atomic.Pointer wiring; the -race
// flag (make test) is the actual guarantor.
func TestEngine_ReloadConcurrentSafe(t *testing.T) {
	e := NewEngine(config.IntelligenceConfig{})
	done := make(chan struct{})
	// Reader goroutines.
	for i := 0; i < 4; i++ {
		go func() {
			for {
				select {
				case <-done:
					return
				default:
					_, _ = e.Lookup("claude-sonnet-4-6")
					_, _ = e.Compute("claude-sonnet-4-6", TokenBundle{Input: 1000})
				}
			}
		}()
	}
	// Reload loop.
	for i := 0; i < 50; i++ {
		e.Reload(config.IntelligenceConfig{
			Pricing: config.PricingConfig{Models: map[string]config.ModelPricing{
				"claude-sonnet-4-6": {Input: float64(i % 5), Output: float64(i)},
			}},
		})
	}
	close(done)
}

// TestNewEngine_LCFieldsRoundTripFromConfig verifies that user overrides
// via config.toml carry the LC fields through to the engine — so an
// install can pin LC rates for SKUs we haven't baked in yet (or correct
// a baked-in rate that's gone stale).
func TestNewEngine_LCFieldsRoundTripFromConfig(t *testing.T) {
	cfg := config.IntelligenceConfig{
		Pricing: config.PricingConfig{Models: map[string]config.ModelPricing{
			"my-custom-llm": {
				Input: 1, Output: 5,
				LongContextThreshold:       300_000,
				LongContextInput:           2,
				LongContextOutput:          10,
				LongContextCacheRead:       0.20,
				LongContextCacheCreation:   2.50,
				LongContextCacheCreation1h: 4,
			},
		}},
	}
	e := NewEngine(cfg)
	p, ok := e.Lookup("my-custom-llm")
	if !ok {
		t.Fatalf("lookup failed")
	}
	if p.LongContextThreshold != 300_000 {
		t.Errorf("threshold not threaded through: %v", p.LongContextThreshold)
	}
	if p.LongContextInput != 2 || p.LongContextOutput != 10 ||
		p.LongContextCacheRead != 0.20 || p.LongContextCacheCreation != 2.50 ||
		p.LongContextCacheCreation1h != 4 {
		t.Errorf("LC overrides not threaded through: %+v", p)
	}
}

// TestFillDefaults_CacheCreation1h verifies the 2 × Input default for the
// 1h tier when the config leaves it blank — but ONLY when CacheCreation
// is explicit non-zero (signal: this entry is Anthropic-shape). For
// non-Anthropic entries (CacheCreation = 0) we don't speculatively
// derive a 1h rate. Pre-2026-04-29 the default was 2 × CacheCreation
// (= 2.5 × Input) and applied unconditionally to anything with Input >
// 0; that over-billed Anthropic 1h cache writes by 25% AND would have
// inflated non-Anthropic rows if a stray cache_creation_tokens leaked
// in. The fix: gate on CacheCreation > 0, use 2 × Input as the rate.
func TestFillDefaults_CacheCreation1h(t *testing.T) {
	// Anthropic-shape entry: explicit CacheCreation triggers the 1h default.
	p := fillDefaults(Pricing{Input: 3, Output: 15, CacheCreation: 3.75})
	if p.CacheCreation1h != 6 {
		t.Errorf("CacheCreation1h default wrong: got %v want 6 (2 × Input)", p.CacheCreation1h)
	}

	// Explicit override stays.
	p = fillDefaults(Pricing{Input: 3, Output: 15, CacheCreation: 3.75, CacheCreation1h: 1})
	if p.CacheCreation1h != 1 {
		t.Errorf("explicit CacheCreation1h overwritten: got %v", p.CacheCreation1h)
	}

	// Non-Anthropic shape (CacheCreation == 0): no default.
	p = fillDefaults(Pricing{Input: 3, Output: 15})
	if p.CacheCreation1h != 0 {
		t.Errorf("CacheCreation1h should stay 0 for non-Anthropic shape: got %v", p.CacheCreation1h)
	}

	// Without Input set, no default applies (we can't infer from CacheCreation
	// alone — the 1h:input ratio is the invariant, not 1h:cache_creation).
	p = fillDefaults(Pricing{CacheCreation: 3.75})
	if p.CacheCreation1h != 0 {
		t.Errorf("CacheCreation1h should be 0 without Input: got %v", p.CacheCreation1h)
	}
}
