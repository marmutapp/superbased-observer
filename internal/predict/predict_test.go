package predict

import (
	"math"
	"testing"
)

// opusRates mirrors the per-token Opus-class rates the store seam hands
// in (cost.Pricing $/M ÷ 1e6). Cache-read is the dominant term for a
// large cached prefix.
var opusRates = RatePair{
	Input:          15e-6,
	Output:         75e-6,
	CacheRead:      1.5e-6,
	CacheCreation:  18.75e-6,
	FastMultiplier: 2,
}

func hasWarn(ws []Warning, w Warning) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
}

func TestEstimate_EmptyInput(t *testing.T) {
	got := Estimate(EstimateInput{Model: "claude-opus-4-8", Rates: opusRates})
	if got.HasEstimate {
		t.Fatalf("expected no estimate on empty input")
	}
	if !hasWarn(got.Warnings, WarnNoSessionHistory) {
		t.Errorf("want no_session_history, got %v", got.Warnings)
	}
}

func TestEstimate_ObservedTier_CachedClaudeCode(t *testing.T) {
	// Cached CC shape: P large (200k), fresh≈0, output varies. Cost is
	// dominated by P·CacheRead × T.
	in := EstimateInput{
		Model:        "claude-opus-4-8",
		Rates:        opusRates,
		PrefixTokens: 200_000,
		TurnSamples: []TurnSample{
			{FreshInput: 0, Output: 100},
			{FreshInput: 1, Output: 400},
			{FreshInput: 0, Output: 800},
			{FreshInput: 2, Output: 1600},
		},
		TurnsPerMessage:  []int{8, 12, 16, 24},
		ObservedMessages: 4,
		YoungThreshold:   3,
	}
	got := Estimate(in)
	if !got.HasEstimate {
		t.Fatal("expected an estimate")
	}
	if got.TurnsTier != TurnsObserved {
		t.Errorf("want observed tier, got %q", got.TurnsTier)
	}
	if hasWarn(got.Warnings, WarnTurnsInferredPrior) || hasWarn(got.Warnings, WarnTurnsInferredDefault) {
		t.Errorf("observed tier must not warn inferred: %v", got.Warnings)
	}
	// Band must be monotonic non-decreasing in message cost.
	if !(got.Low.MessageUSD <= got.Mid.MessageUSD && got.Mid.MessageUSD <= got.High.MessageUSD) {
		t.Errorf("band not monotonic: low=%.6f mid=%.6f high=%.6f",
			got.Low.MessageUSD, got.Mid.MessageUSD, got.High.MessageUSD)
	}
	// Sanity on the dominant term: mid per-turn ≈ P·CacheRead = 200000×1.5e-6 = 0.30 + output.
	wantFloor := 200_000 * opusRates.CacheRead
	if got.Mid.PerTurnUSD < wantFloor {
		t.Errorf("mid per-turn %.6f below cache-read floor %.6f", got.Mid.PerTurnUSD, wantFloor)
	}
	// Mid message ≈ T_mid(=14) × per-turn.
	if got.Mid.Turns != 14 {
		t.Errorf("want mid turns 14 (median of 8,12,16,24), got %.1f", got.Mid.Turns)
	}
}

func TestEstimate_PriorTier(t *testing.T) {
	in := EstimateInput{
		Model:                "claude-opus-4-8",
		Rates:                opusRates,
		PrefixTokens:         50_000,
		TurnSamples:          []TurnSample{{FreshInput: 0, Output: 500}},
		TurnsPerMessage:      nil, // no boundaries this session
		ObservedMessages:     0,
		YoungThreshold:       3,
		PriorTurnsPerMessage: []int{10, 15, 20},
		DefaultTurns:         12,
	}
	got := Estimate(in)
	if got.TurnsTier != TurnsPrior {
		t.Errorf("want prior tier, got %q", got.TurnsTier)
	}
	if !hasWarn(got.Warnings, WarnTurnsInferredPrior) {
		t.Errorf("want turns_inferred_prior, got %v", got.Warnings)
	}
	if got.Mid.Turns != 15 {
		t.Errorf("want mid turns 15 (median of prior), got %.1f", got.Mid.Turns)
	}
}

func TestEstimate_DefaultTier(t *testing.T) {
	in := EstimateInput{
		Model:        "gpt-5.4-codex",
		Rates:        opusRates,
		PrefixTokens: 0, // uncached
		TurnSamples:  []TurnSample{{FreshInput: 3000, Output: 600}},
		DefaultTurns: 12,
	}
	got := Estimate(in)
	if got.TurnsTier != TurnsDefault {
		t.Errorf("want default tier, got %q", got.TurnsTier)
	}
	if !hasWarn(got.Warnings, WarnTurnsInferredDefault) {
		t.Errorf("want turns_inferred_default, got %v", got.Warnings)
	}
	if !hasWarn(got.Warnings, WarnEmptyPrefix) {
		t.Errorf("want empty_prefix (P=0), got %v", got.Warnings)
	}
	if got.Mid.Turns != 12 {
		t.Errorf("want default 12 turns, got %.1f", got.Mid.Turns)
	}
	// Uncached: per-turn is fresh-input + output priced; cache-read term 0.
	wantPerTurn := 3000*opusRates.Input + 600*opusRates.Output
	if math.Abs(got.Mid.PerTurnUSD-wantPerTurn) > 1e-9 {
		t.Errorf("uncached per-turn %.9f want %.9f", got.Mid.PerTurnUSD, wantPerTurn)
	}
}

func TestEstimate_FastMode(t *testing.T) {
	base := EstimateInput{
		Model:            "claude-opus-4-8",
		Rates:            opusRates,
		PrefixTokens:     100_000,
		TurnSamples:      []TurnSample{{FreshInput: 0, Output: 500}},
		TurnsPerMessage:  []int{10, 10, 10},
		ObservedMessages: 3,
		YoungThreshold:   3,
	}
	slow := Estimate(base)
	base.CurrentFast = true
	fast := Estimate(base)
	if !hasWarn(fast.Warnings, WarnFastModeActive) {
		t.Errorf("want fast_mode_active, got %v", fast.Warnings)
	}
	if !(fast.Mid.PerTurnUSD > slow.Mid.PerTurnUSD) {
		t.Errorf("fast per-turn %.6f should exceed slow %.6f", fast.Mid.PerTurnUSD, slow.Mid.PerTurnUSD)
	}
}

func TestEstimate_YoungSessionBlendsPrior(t *testing.T) {
	// Young session (1 msg < threshold 3) with a prior shape blends the
	// prior samples into the (S,O) quantiles AND uses the prior T.
	in := EstimateInput{
		Model:                "claude-opus-4-8",
		Rates:                opusRates,
		PrefixTokens:         80_000,
		TurnSamples:          []TurnSample{{FreshInput: 0, Output: 100}},
		TurnsPerMessage:      []int{30}, // 1 message, below threshold
		ObservedMessages:     1,
		YoungThreshold:       3,
		PriorTurnsPerMessage: []int{8, 12, 16},
		PriorTurnSamples:     []TurnSample{{FreshInput: 0, Output: 2000}, {FreshInput: 0, Output: 3000}},
	}
	got := Estimate(in)
	if got.TurnsTier != TurnsPrior {
		t.Errorf("young session should use prior T, got %q", got.TurnsTier)
	}
	// High output quantile should reflect the blended prior (2000/3000),
	// not just the lone session sample (100).
	if got.High.Output < 1000 {
		t.Errorf("expected prior-blended high output, got %d", got.High.Output)
	}
}

func TestQuantile(t *testing.T) {
	s := []float64{10, 20, 30, 40} // sorted
	cases := []struct {
		q    float64
		want float64
	}{
		{0, 10}, {1, 40}, {0.5, 25}, {0.25, 17.5}, {0.75, 32.5},
	}
	for _, c := range cases {
		if got := quantile(s, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("quantile(%.2f)=%.4f want %.4f", c.q, got, c.want)
		}
	}
	if quantile(nil, 0.5) != 0 {
		t.Errorf("empty quantile should be 0")
	}
	if quantile([]float64{7}, 0.9) != 7 {
		t.Errorf("single-element quantile should be the element")
	}
}
