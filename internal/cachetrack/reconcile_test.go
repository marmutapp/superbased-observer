package cachetrack

import (
	"math"
	"testing"
)

// TestReconcilePrediction covers the predict-vs-observe bucket
// table: hit-vs-write at the bucket level, every rewrite subtype
// folded into the write bucket, every skip kind excluded.
// Table-driven per §24.5.
func TestReconcilePrediction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		predicted   Kind
		observed    Kind
		writeTok    int64
		estTok      int64
		wantMisp    bool
		wantScale   float64
		wantTrigger Trigger
	}{
		// Exact match (hit).
		{"hit predicted, hit observed", KindHit, KindHit, 0, 0, false, 0, TriggerRead},
		// Exact match (write subtypes collapse into one bucket).
		{"write predicted, write observed", KindWrite, KindWrite, 100, 100, false, 1.0, TriggerWrite},
		{
			"write predicted, invalidation_rewrite observed (same bucket)",
			KindWrite, KindInvalidationRewrite, 100, 50, false, 2.0, TriggerWrite,
		},
		{
			"invalidation_rewrite predicted, write observed",
			KindInvalidationRewrite, KindWrite, 0, 0, false, 0, TriggerWrite,
		},
		// Cross-bucket = mispredict.
		{
			"hit predicted, write observed → mispredict",
			KindHit, KindWrite, 50000, 100, true, 500.0, TriggerMispredict,
		},
		{
			"write predicted, hit observed → mispredict",
			KindWrite, KindHit, 0, 100, true, 0, TriggerMispredict,
		},
		// Skip bucket — predicted reanchor, observed write: skipped
		// vs write are different buckets, so it does mispredict.
		// The §10 denominator filter applies separately.
		{"reanchor predicted, write observed", KindReanchor, KindWrite, 1, 1, true, 1.0, TriggerMispredict},
		// Below_min predicted, no provider engagement observed
		// (both fall in skipped bucket).
		{
			"below_min predicted, below_min observed",
			KindBelowMin, KindBelowMin, 0, 0, false, 0, TriggerUnknown,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ReconcilePrediction(
				PredictedShape{Kind: tt.predicted, EstimatedWrittenTokens: tt.estTok},
				ObservedShape{Kind: tt.observed, ObservedWriteTokens: tt.writeTok},
			)
			if got.Mispredicted != tt.wantMisp {
				t.Errorf("Mispredicted = %v, want %v", got.Mispredicted, tt.wantMisp)
			}
			if !floatNear(got.EstimateScale, tt.wantScale, 1e-9) {
				t.Errorf("EstimateScale = %v, want %v", got.EstimateScale, tt.wantScale)
			}
			if got.EntryTrigger != tt.wantTrigger {
				t.Errorf("EntryTrigger = %v, want %v", got.EntryTrigger, tt.wantTrigger)
			}
		})
	}
}

// TestBucketOf pins the §10 denominator definition: which Kinds
// participate in the hit-vs-write health metric vs which are
// excluded.
func TestBucketOf(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind Kind
		want kindBucket
	}{
		{KindHit, bucketHit},
		{KindWrite, bucketWrite},
		{KindExpiryRewrite, bucketWrite},
		{KindInvalidationRewrite, bucketWrite},
		{KindModelSwitchRewrite, bucketWrite},
		{KindBelowMin, bucketSkipped},
		{KindReanchor, bucketSkipped},
		{KindMispredict, bucketSkipped},
		{KindCompactionReset, bucketSkipped},
		{Kind("never-defined"), bucketUnknown},
	}
	for _, tt := range tests {
		if got := bucketOf(tt.kind); got != tt.want {
			t.Errorf("bucketOf(%q) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// TestUpdateEMA covers the R6 calibration smoothing:
// - First sample seeds directly (no prior to smooth against).
// - Subsequent samples apply α=0.3.
// - Zero/negative ratios are ignored (don't pollute the EMA).
func TestUpdateEMA(t *testing.T) {
	t.Parallel()
	// First sample seeds.
	first := UpdateEMA(SessionEMA{}, 2.0)
	if first.Scale != 2.0 {
		t.Errorf("first seed: Scale = %v, want 2.0", first.Scale)
	}
	if first.Samples != 1 {
		t.Errorf("first seed: Samples = %d, want 1", first.Samples)
	}
	// Second sample smooths: 0.3 × 3.0 + 0.7 × 2.0 = 2.3.
	second := UpdateEMA(first, 3.0)
	wantSecond := EMAAlpha*3.0 + (1-EMAAlpha)*2.0
	if !floatNear(second.Scale, wantSecond, 1e-9) {
		t.Errorf("second sample: Scale = %v, want %v", second.Scale, wantSecond)
	}
	if second.Samples != 2 {
		t.Errorf("second sample: Samples = %d, want 2", second.Samples)
	}
	// Zero ratio = ignored.
	third := UpdateEMA(second, 0)
	if third != second {
		t.Errorf("zero ratio polluted EMA: %+v vs %+v", third, second)
	}
	// Negative ratio = ignored.
	fourth := UpdateEMA(second, -1.0)
	if fourth != second {
		t.Errorf("negative ratio polluted EMA: %+v vs %+v", fourth, second)
	}
}

// TestMispredictRate computes the §10 health metric and verifies
// the denominator-exclusion rule (KindBelowMin / KindReanchor /
// KindCompactionReset are excluded; KindMispredict IS counted —
// it's the very thing the rate measures).
func TestMispredictRate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		events    []Kind
		wantRate  float64
		wantDenom int
	}{
		{
			name:      "empty → 0 / 0 (insufficient data)",
			events:    nil,
			wantRate:  0,
			wantDenom: 0,
		},
		{
			name:      "all skipped → 0 / 0",
			events:    []Kind{KindBelowMin, KindReanchor, KindCompactionReset},
			wantRate:  0,
			wantDenom: 0,
		},
		{
			name:      "1 hit 0 misp → 0% / 1",
			events:    []Kind{KindHit},
			wantRate:  0,
			wantDenom: 1,
		},
		{
			name:      "1 hit 1 misp → 50% / 2",
			events:    []Kind{KindHit, KindMispredict},
			wantRate:  0.5,
			wantDenom: 2,
		},
		{
			name:      "smoke shape — 4 misp of 15 graded → 26.7% / 15",
			events:    appendMany(KindWrite, 11, []Kind{KindMispredict, KindMispredict, KindMispredict, KindMispredict}),
			wantRate:  4.0 / 15.0,
			wantDenom: 15,
		},
		{
			name: "200-turn fixture — 4 misp of 200 graded → 2% (P0 GO)",
			events: appendMany(KindHit, 100,
				appendMany(KindWrite, 96,
					[]Kind{KindMispredict, KindMispredict, KindMispredict, KindMispredict})),
			wantRate:  4.0 / 200.0,
			wantDenom: 200,
		},
		{
			name: "denominator excludes BelowMin/Reanchor/CompactionReset but counts mispredicts",
			events: []Kind{
				KindHit, KindHit, KindWrite,
				KindBelowMin, KindReanchor, KindCompactionReset, // all 3 → excluded
				KindMispredict,
			},
			wantRate:  1.0 / 4.0, // 1 misp out of (2 hit + 1 write + 1 misp) = 4 graded
			wantDenom: 4,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rate, denom := MispredictRate(tt.events)
			if denom != tt.wantDenom {
				t.Errorf("denom = %d, want %d", denom, tt.wantDenom)
			}
			if !floatNear(rate, tt.wantRate, 1e-9) {
				t.Errorf("rate = %v, want %v", rate, tt.wantRate)
			}
		})
	}
}

// TestMispredictRate_KnownRatio is the operator's specific ask
// after the soak surfaced rate=0.0000 on a 4-of-15 mispredict
// corpus. Asserts the literal ratio computes correctly — a
// regression on the rate calculation should fail this loudly,
// not silently grade every soak as 0.0000.
func TestMispredictRate_KnownRatio(t *testing.T) {
	t.Parallel()
	cases := []struct {
		misp, graded int
	}{
		{1, 201}, // prior 201-turn "passing" soak; pre-fix produced 0.0000, real = 0.0050
		{4, 15},  // post-fix smoke shape; pre-fix produced 0.0000, real ≈ 0.2667
		{50, 100},
		{1, 1},
	}
	for _, c := range cases {
		nonMisp := c.graded - c.misp
		events := appendMany(KindWrite, nonMisp, nil)
		for i := 0; i < c.misp; i++ {
			events = append(events, KindMispredict)
		}
		rate, denom := MispredictRate(events)
		want := float64(c.misp) / float64(c.graded)
		if denom != c.graded {
			t.Errorf("(%d/%d): denom = %d, want %d", c.misp, c.graded, denom, c.graded)
		}
		if !floatNear(rate, want, 1e-9) {
			t.Errorf("(%d/%d): rate = %v, want %v", c.misp, c.graded, rate, want)
		}
	}
}

// TestMispredictRate_P0_Gate constructs a 200-event mix that
// includes real cross-bucket mispredicts (predicted hit, got
// write) and confirms the rate computation surfaces them.
// Goal: prove the metric SCORES mispredicts correctly so the
// soak can rely on the ≥95% gate.
func TestMispredictRate_P0_Gate(t *testing.T) {
	t.Parallel()
	// 200 graded events: 100 hits + 96 writes + 4 mispredicts.
	// Mispredicts ARE in the denominator AND the numerator (the
	// §10 rate is exactly "fraction of graded events that
	// mispredicted").
	events := appendMany(KindHit, 100,
		appendMany(KindWrite, 96,
			[]Kind{KindMispredict, KindMispredict, KindMispredict, KindMispredict}))
	rate, denom := MispredictRate(events)
	if denom != 200 {
		t.Fatalf("denom = %d, want 200", denom)
	}
	want := 4.0 / 200.0 // = 0.02 = 2%
	if !floatNear(rate, want, 1e-9) {
		t.Errorf("rate = %v, want %v (4 mispredicts / 200 graded)", rate, want)
	}
	// And the P0 gate (≤ 5%) PASSES.
	if rate > 0.05 {
		t.Errorf("rate %.4f should pass the ≤5%% P0 gate", rate)
	}
}

// TestMispredictRateGraded_ZeroUsageMispredictsExcluded is the
// operator-requested carve-out: a KindMispredict event whose
// observed tokens (tokens_read AND tokens_written) are BOTH zero
// is observationally vacant — the engine predicted something but
// the provider returned no token data to grade against (the
// 2026-06-09 soak surfaced 4 such on opus-4-8: input/output
// tokens 0, http_status NULL, 372-453 ms turn times). Counting
// these as mispredicts distorts the rate the same way the C9
// rate bug did. The graded variant skips both numerator AND
// denominator for them; the simple variant keeps prior behavior.
func TestMispredictRateGraded_ZeroUsageMispredictsExcluded(t *testing.T) {
	t.Parallel()
	// 11 KindWrite (real graded writes with token movement) +
	// 4 KindMispredict events with BOTH tokens at zero.
	events := appendMany(KindWrite, 11, []Kind{
		KindMispredict, KindMispredict, KindMispredict, KindMispredict,
	})
	tokensRead := make([]int64, 15)
	tokensWritten := make([]int64, 15)
	for i := 0; i < 11; i++ {
		tokensRead[i] = 5000
		tokensWritten[i] = 100
	}
	// last 4 (mispredicts) stay at zero — the zero-usage shape.

	// Simple (token-blind) form sees 4 mispredicts in 15 graded
	// → 26.7%. This is the legacy behavior pinned by the C12
	// rate-bug fix.
	{
		rate, denom := MispredictRate(events)
		if denom != 15 || !floatNear(rate, 4.0/15.0, 1e-9) {
			t.Errorf("MispredictRate baseline: rate=%v denom=%d, want 4/15", rate, denom)
		}
	}
	// Graded form excludes the 4 zero-usage events → 0 mispredicts
	// in 11 graded → 0.
	{
		rate, denom := MispredictRateGraded(events, tokensRead, tokensWritten)
		if denom != 11 {
			t.Errorf("MispredictRateGraded: denom = %d, want 11 (zero-usage misp excluded)", denom)
		}
		if rate != 0 {
			t.Errorf("MispredictRateGraded: rate = %v, want 0 (no real mispredicts after exclusion)", rate)
		}
	}
}

// TestMispredictRateGraded_RealMispredictsStillCounted pins that
// the carve-out doesn't accidentally exclude REAL mispredicts —
// a KindMispredict event with non-zero tokens stays graded.
// Distinguishes "engine predicted wrong but observation has
// signal" from "observation is vacant."
func TestMispredictRateGraded_RealMispredictsStillCounted(t *testing.T) {
	t.Parallel()
	events := appendMany(KindWrite, 11, []Kind{
		KindMispredict, // tokens unknown via empty slices → counts
		KindMispredict, // explicit non-zero → counts
		KindMispredict, // zero usage → excluded
	})
	tokensRead := []int64{
		5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, // 11 writes
		0, // mispredict #1 — zero-but-via-bounds? no, this slot HAS value
		// hmm — let me explicit-set per slot
	}
	tokensWritten := []int64{
		100, 100, 100, 100, 100, 100, 100, 100, 100, 100, 100, // 11 writes
		// mispredicts below
	}
	// Slot 11 mispredict: read=0 write=0 → zero-usage → excluded
	// Slot 12 mispredict: read=500 write=0 → counted
	// Slot 13 mispredict: read=0 write=0 → excluded
	tokensRead = append(tokensRead, 0, 500, 0)
	tokensWritten = append(tokensWritten, 0, 0, 0)

	rate, denom := MispredictRateGraded(events, tokensRead, tokensWritten)
	// Denominator: 11 writes + 1 counted mispredict = 12
	// Numerator: 1 counted mispredict
	if denom != 12 {
		t.Errorf("denom = %d, want 12 (11 writes + 1 graded mispredict)", denom)
	}
	if !floatNear(rate, 1.0/12.0, 1e-9) {
		t.Errorf("rate = %v, want %v", rate, 1.0/12.0)
	}
}

// TestMispredictRateGraded_NilTokensMatchesSimpleVariant pins
// the contract that passing nil token slices makes the graded
// variant behave identically to MispredictRate.
func TestMispredictRateGraded_NilTokensMatchesSimpleVariant(t *testing.T) {
	t.Parallel()
	events := appendMany(KindWrite, 11, []Kind{
		KindMispredict, KindMispredict, KindMispredict, KindMispredict,
	})
	rA, dA := MispredictRate(events)
	rB, dB := MispredictRateGraded(events, nil, nil)
	if rA != rB || dA != dB {
		t.Errorf("nil tokens broke parity: simple=(%v,%d) graded=(%v,%d)", rA, dA, rB, dB)
	}
}

// appendMany is a small helper that returns n copies of v
// prepended to tail. Reads cleanly at the test-table site.
func appendMany(v Kind, n int, tail []Kind) []Kind {
	out := make([]Kind, n+len(tail))
	for i := 0; i < n; i++ {
		out[i] = v
	}
	copy(out[n:], tail)
	return out
}

// floatNear returns true when |a−b| ≤ eps. Avoids the
// math.Abs+subtract pattern at the call sites.
func floatNear(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
