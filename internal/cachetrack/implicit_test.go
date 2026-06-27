package cachetrack

import (
	"testing"
	"time"
)

// TestAttributeImplicit_DecisionTable is the row-by-row pin of the
// §15.3 reduced attribution decision set. One row per documented
// case; future cause additions land as one row here + one in
// implicitAttributionRules.
func TestAttributeImplicit_DecisionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		obs       ImplicitObservation
		wantKind  Kind
		wantCause Cause
	}{
		{
			name: "row 1 — prompt_cache_key overflow wins over everything",
			obs: ImplicitObservation{
				Model:                  "gpt-5",
				PriorObserved:          true,
				PriorPrefixTokens:      4096,
				ObservedCachedTokens:   2048, // would otherwise be a hit
				PromptCacheKeyOverflow: true,
			},
			wantKind:  KindImplicitWrite,
			wantCause: CausePromptCacheKeyOverflow,
		},
		{
			name: "row 2 — bootstrap turn (no prior) reanchors",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        false,
				PriorPrefixTokens:    0,
				ObservedCachedTokens: 0,
			},
			wantKind:  KindImplicitWrite,
			wantCause: CauseReanchor,
		},
		{
			name: "row 2 — bootstrap with observed cached tokens still reanchors (first turn)",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        false,
				PriorPrefixTokens:    0,
				ObservedCachedTokens: 1024,
			},
			wantKind:  KindImplicitWrite,
			wantCause: CauseReanchor,
		},
		{
			name: "row 3 — tracked prefix below implicit min (1024) → below_min",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        true,
				PriorPrefixTokens:    512,
				ObservedCachedTokens: 0,
			},
			wantKind:  KindBelowMin,
			wantCause: CauseBelowMinCacheable,
		},
		{
			name: "row 4 — prior prefix above min + observed cached=0 → implicit_miss / prefix_churn",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        true,
				PriorPrefixTokens:    4096,
				ObservedCachedTokens: 0,
			},
			wantKind:  KindImplicitMiss,
			wantCause: CausePrefixChurn,
		},
		{
			name: "row 5 — observed cached well below tracked → implicit_hit / prefix_shrink",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        true,
				PriorPrefixTokens:    8192,
				ObservedCachedTokens: 4096, // 8192 - 128 = 8064; 4096 < 8064 → shrink
			},
			wantKind:  KindImplicitHit,
			wantCause: CausePrefixShrink,
		},
		{
			name: "row 6 — observed cached within band of tracked → implicit_hit / implicit_hit",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        true,
				PriorPrefixTokens:    4096,
				ObservedCachedTokens: 4096, // within band (within one granule)
			},
			wantKind:  KindImplicitHit,
			wantCause: CauseImplicitHit,
		},
		{
			name: "row 6 — sub-granule deviation still a clean hit (within 128 tokens)",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        true,
				PriorPrefixTokens:    4096,
				ObservedCachedTokens: 4032, // 4096 - 64 = within one granule
			},
			wantKind:  KindImplicitHit,
			wantCause: CauseImplicitHit,
		},
		{
			name: "fallthrough — observed cached=0 + prior prefix below min → implicit_write / suffix_growth",
			obs: ImplicitObservation{
				Model:                "gpt-5",
				PriorObserved:        true,
				PriorPrefixTokens:    0, // no prior estimate yet; growing toward cacheable
				ObservedCachedTokens: 0,
			},
			wantKind:  KindImplicitWrite,
			wantCause: CauseSuffixGrowth,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := attributeImplicit(tt.obs)
			if got.Kind != tt.wantKind || got.Cause != tt.wantCause {
				t.Errorf("attributeImplicit: kind=%q cause=%q, want kind=%q cause=%q",
					got.Kind, got.Cause, tt.wantKind, tt.wantCause)
			}
		})
	}
}

// TestPredictImplicitKind_DecisionTable pins the prediction-side
// logic that the consistency metric grades against.
func TestPredictImplicitKind_DecisionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		obs  ImplicitObservation
		want Kind
	}{
		{
			name: "bootstrap turn predicts write",
			obs:  ImplicitObservation{PriorObserved: false},
			want: KindImplicitWrite,
		},
		{
			name: "prior below min predicts below_min",
			obs:  ImplicitObservation{Model: "gpt-5", PriorObserved: true, PriorPrefixTokens: 512},
			want: KindBelowMin,
		},
		{
			name: "prior above min predicts implicit_hit",
			obs:  ImplicitObservation{Model: "gpt-5", PriorObserved: true, PriorPrefixTokens: 4096},
			want: KindImplicitHit,
		},
		{
			name: "prior at min boundary predicts implicit_hit",
			obs:  ImplicitObservation{Model: "gpt-5", PriorObserved: true, PriorPrefixTokens: 1024},
			want: KindImplicitHit,
		},
		{
			name: "prior zero on continuation predicts write (pre-cacheable growth)",
			obs:  ImplicitObservation{Model: "gpt-5", PriorObserved: true, PriorPrefixTokens: 0},
			want: KindImplicitWrite,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := predictImplicitKind(tt.obs); got != tt.want {
				t.Errorf("predictImplicitKind = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestMinCacheableImplicit_Default pins the OpenAI 1024 default.
// A future per-family override lands as a row in MinCacheableImplicit.
func TestMinCacheableImplicit_Default(t *testing.T) {
	t.Parallel()
	for _, m := range []string{"gpt-5", "gpt-4o", "gpt-4o-mini", "gpt-5-codex", ""} {
		if got := MinCacheableImplicit(m); got != implicitDefaultMinCacheable {
			t.Errorf("MinCacheableImplicit(%q) = %d, want %d", m, got, implicitDefaultMinCacheable)
		}
	}
}

// TestQuantizeToGranule pins the proxy-side LCP rounding helper.
func TestQuantizeToGranule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n, granule, want int64
	}{
		{0, 128, 0},
		{127, 128, 0},
		{128, 128, 128},
		{200, 128, 128},
		{255, 128, 128},
		{256, 128, 256},
		{4096, 128, 4096},
		{4097, 128, 4096},
		{-100, 128, 0}, // defensive: negative inputs clamp to 0
		{1024, 0, 0},   // defensive: zero granule returns 0
		{1024, -1, 0},  // defensive: negative granule returns 0
	}
	for _, tt := range tests {
		if got := QuantizeToGranule(tt.n, int(tt.granule)); got != tt.want {
			t.Errorf("QuantizeToGranule(%d, %d) = %d, want %d", tt.n, tt.granule, got, tt.want)
		}
	}
}

// TestBucketOf_ImplicitKindsAreSkipped is the §5 guardrail at the
// bucket level: every implicit-cache kind folds into bucketSkipped
// so ReconcilePrediction's bucket-diff check never fires for them
// (no entry demotion, no G2/G3 mispredict accounting).
func TestBucketOf_ImplicitKindsAreSkipped(t *testing.T) {
	t.Parallel()
	for _, k := range []Kind{KindImplicitHit, KindImplicitMiss, KindImplicitWrite} {
		if got := bucketOf(k); got != bucketSkipped {
			t.Errorf("bucketOf(%q) = %d, want bucketSkipped (%d) — §5 guardrail: implicit kinds must NOT enter the Anthropic predict-vs-observe bucket-diff",
				k, got, bucketSkipped)
		}
	}
}

// TestIsRateSkipped_ImplicitKindsAreSkipped is the §5 guardrail at
// the rate level: every implicit-cache kind is excluded from the §10
// Anthropic MispredictRateGraded gate (numerator AND denominator).
func TestIsRateSkipped_ImplicitKindsAreSkipped(t *testing.T) {
	t.Parallel()
	for _, k := range []Kind{KindImplicitHit, KindImplicitMiss, KindImplicitWrite} {
		if !isRateSkipped(k) {
			t.Errorf("isRateSkipped(%q) = false, want true — §10 gate must exclude implicit kinds (load-bearing §5 guardrail)", k)
		}
	}
}

// TestIsImplicitCacheKind covers the closed-set helper.
func TestIsImplicitCacheKind(t *testing.T) {
	t.Parallel()
	implicit := []Kind{KindImplicitHit, KindImplicitMiss, KindImplicitWrite}
	anthropic := []Kind{
		KindHit, KindWrite, KindReanchor, KindBelowMin, KindMispredict,
		KindExpiryRewrite, KindInvalidationRewrite, KindModelSwitchRewrite, KindCompactionReset,
	}
	for _, k := range implicit {
		if !IsImplicitCacheKind(k) {
			t.Errorf("IsImplicitCacheKind(%q) = false, want true", k)
		}
	}
	for _, k := range anthropic {
		if IsImplicitCacheKind(k) {
			t.Errorf("IsImplicitCacheKind(%q) = true, want false (Anthropic kind must not be classified as implicit)", k)
		}
	}
}

// TestMispredictRateGraded_AnthropicRateUnmovedByImplicit IS THE
// LOAD-BEARING §5 / §13 HARD GATE: an interleaved corpus of Anthropic
// + implicit-cache events run through the REAL MispredictRateGraded
// assembly path must produce the SAME rate as the Anthropic-only
// subset. The implicit-cache events must vanish from both numerator
// and denominator.
//
// This catches the §11.2 trap: a predicate-only unit test cannot
// see how the rate function actually consumes the kinds slice. Run
// the real function with the real parallel slices.
func TestMispredictRateGraded_AnthropicRateUnmovedByImplicit(t *testing.T) {
	t.Parallel()
	// Anthropic-only corpus: a deliberate mix of hits + writes + 2
	// mispredicts + skip kinds. Rate denominator = 7 (3 hit + 3 write
	// + 2 mispredict, with the 2 reanchor + 2 below_min skipped).
	// Numerator = 2 (mispredicts). Expected rate = 2/9 ≈ 0.2222.
	anthropicKinds := []Kind{
		KindReanchor,              // skipped (rate)
		KindHit,                   // graded
		KindWrite,                 // graded
		KindHit,                   // graded
		KindMispredict, KindWrite, // graded mispredict + graded write
		KindBelowMin,               // skipped
		KindHit,                    // graded
		KindWrite,                  // graded
		KindMispredict,             // graded mispredict
		KindBelowMin, KindReanchor, // skipped + skipped
	}
	anthropicReads := make([]int64, len(anthropicKinds))
	anthropicWrites := make([]int64, len(anthropicKinds))
	// Give mispredicts non-zero tokens so they aren't excluded by the
	// zero-usage filter — we want them in the rate numerator.
	for i, k := range anthropicKinds {
		if k == KindMispredict {
			anthropicReads[i] = 100
			anthropicWrites[i] = 50
		}
	}

	wantRate, wantDenom := MispredictRateGraded(anthropicKinds, anthropicReads, anthropicWrites)
	if wantDenom == 0 {
		t.Fatalf("baseline Anthropic-only corpus produced denom=0; fixture is broken")
	}

	// Now build the interleaved corpus: scatter every implicit-cache
	// Kind (hit/miss/write) between the Anthropic events. The §5
	// guardrail says the rate MUST be byte-identical with these
	// extras present.
	implicitKinds := []Kind{KindImplicitHit, KindImplicitMiss, KindImplicitWrite}
	// Build by interleaving: A, I, A, I, A, ...
	var interleavedKinds []Kind
	var interleavedReads, interleavedWrites []int64
	implicitIdx := 0
	for i, k := range anthropicKinds {
		interleavedKinds = append(interleavedKinds, k)
		interleavedReads = append(interleavedReads, anthropicReads[i])
		interleavedWrites = append(interleavedWrites, anthropicWrites[i])
		// Inject every implicit-cache kind multiple times around each
		// Anthropic event so the test exercises real interleaving.
		for j := 0; j < 3; j++ {
			ik := implicitKinds[implicitIdx%len(implicitKinds)]
			implicitIdx++
			interleavedKinds = append(interleavedKinds, ik)
			// Give implicit-cache events realistic non-zero token
			// counts (the real proxy will emit them this way). If the
			// gate accidentally let them through, the rate WOULD move.
			switch ik {
			case KindImplicitHit:
				interleavedReads = append(interleavedReads, 4096)
				interleavedWrites = append(interleavedWrites, 0)
			case KindImplicitMiss:
				interleavedReads = append(interleavedReads, 0)
				interleavedWrites = append(interleavedWrites, 0)
			case KindImplicitWrite:
				interleavedReads = append(interleavedReads, 0)
				interleavedWrites = append(interleavedWrites, 200)
			}
		}
	}

	gotRate, gotDenom := MispredictRateGraded(interleavedKinds, interleavedReads, interleavedWrites)
	if gotRate != wantRate {
		t.Errorf("MispredictRateGraded interleaved rate = %v, want %v (Anthropic-only baseline) — §5 GUARDRAIL FAILED: implicit-cache events moved the Anthropic §10 gate",
			gotRate, wantRate)
	}
	if gotDenom != wantDenom {
		t.Errorf("MispredictRateGraded interleaved denom = %d, want %d (Anthropic-only baseline) — §5 GUARDRAIL FAILED: implicit-cache events entered the denominator",
			gotDenom, wantDenom)
	}
	// Diagnostics for human review of the fixture itself:
	t.Logf("Anthropic-only:  rate=%v denom=%d", wantRate, wantDenom)
	t.Logf("Interleaved A+I: rate=%v denom=%d (corpus size = %d events with %d implicit + %d anthropic)",
		gotRate, gotDenom, len(interleavedKinds),
		len(interleavedKinds)-len(anthropicKinds), len(anthropicKinds))
}

// TestImplicitCacheConsistency_OnlyGradesImplicitEvents pins the
// separate metric (§15.3 / §13 acceptance criterion). Mixed corpus
// in; only the implicit-cache subset comes out.
func TestImplicitCacheConsistency_OnlyGradesImplicitEvents(t *testing.T) {
	t.Parallel()
	// 5 implicit events: 3 hit (predicted hit ✓), 1 miss
	// (predicted hit ✗), 1 write (bootstrap — excluded from graded).
	// 4 Anthropic events: ignored entirely.
	predicted := []Kind{
		KindImplicitHit,   // implicit, p=hit
		KindHit,           // ignored
		KindImplicitHit,   // implicit, p=hit
		KindImplicitHit,   // implicit, p=hit (predicted Hit, observed Miss → inconsistency)
		KindWrite,         // ignored
		KindImplicitWrite, // implicit BOOTSTRAP excluded
		KindMispredict,    // ignored
		KindImplicitHit,   // implicit, p=hit ✓
		KindHit,           // ignored
	}
	observed := []Kind{
		KindImplicitHit,   // ✓
		KindHit,           // ignored
		KindImplicitHit,   // ✓
		KindImplicitMiss,  // ✗ — inconsistency
		KindWrite,         // ignored
		KindImplicitWrite, // bootstrap
		KindMispredict,    // ignored
		KindImplicitHit,   // ✓
		KindHit,           // ignored
	}
	report := ImplicitCacheConsistency(predicted, observed)
	if report.Total != 5 {
		t.Errorf("Total = %d, want 5 (every implicit-cache event)", report.Total)
	}
	if report.Graded != 4 {
		t.Errorf("Graded = %d, want 4 (bootstrap implicit_write excluded)", report.Graded)
	}
	if report.Consistent != 3 {
		t.Errorf("Consistent = %d, want 3 (one Hit→Miss inconsistency)", report.Consistent)
	}
	if report.Rate != 0.75 {
		t.Errorf("Rate = %v, want 0.75 (3/4)", report.Rate)
	}
}

// TestImplicitCacheConsistency_EmptyInput returns the zero report.
func TestImplicitCacheConsistency_EmptyInput(t *testing.T) {
	t.Parallel()
	report := ImplicitCacheConsistency(nil, nil)
	if report.Total != 0 || report.Graded != 0 || report.Consistent != 0 || report.Rate != 0 {
		t.Errorf("ImplicitCacheConsistency on empty input: %+v, want zero report", report)
	}
}

// TestImplicitCacheConsistency_AnthropicOnlyReturnsZero confirms a
// pure-Anthropic corpus yields the zero report — implicit metric is
// not retro-fitting Anthropic data.
func TestImplicitCacheConsistency_AnthropicOnlyReturnsZero(t *testing.T) {
	t.Parallel()
	predicted := []Kind{KindHit, KindWrite, KindMispredict, KindReanchor}
	observed := []Kind{KindHit, KindHit, KindMispredict, KindReanchor}
	report := ImplicitCacheConsistency(predicted, observed)
	if report.Total != 0 || report.Graded != 0 || report.Rate != 0 {
		t.Errorf("Anthropic-only corpus produced non-zero implicit report %+v", report)
	}
}

// TestObserveTurn_ImplicitCache_DispatchesToImplicitPath covers the
// engine seam: when caps.ImplicitCache is true, the engine routes
// to the reduced attribution + emits an implicit kind, no Segments,
// no Entries.
func TestObserveTurn_ImplicitCache_DispatchesToImplicitPath(t *testing.T) {
	t.Parallel()
	eng := NewEngine(0)
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// Bootstrap turn — no prior state, should reanchor (KindImplicitWrite).
	res := eng.ObserveTurn(ObserveInput{
		SessionID: "sess-implicit",
		Model:     "gpt-5",
		Scope:     "default",
		Tier:      TierProxy,
		Caps: Capabilities{
			ToolsVisible:        true,
			SystemVisible:       true,
			MarkersVisible:      true,
			UsageObserved:       true,
			BlocksAreCumulative: true,
			ImplicitCache:       true, // overlay
		},
		MessageID: "req-1",
		Now:       now,
		Usage: CacheUsageObserved{
			NetInputTokens:  2048, // > implicit min
			OutputTokens:    100,
			CacheReadTokens: 0, // bootstrap: cache cold
		},
		APITurnID: 1,
	})
	if len(res.Events) != 1 {
		t.Fatalf("bootstrap: events=%d, want 1", len(res.Events))
	}
	if res.Events[0].Outcome.Kind != KindImplicitWrite || res.Events[0].Outcome.Cause != CauseReanchor {
		t.Errorf("bootstrap event = %q/%q, want implicit_write/reanchor", res.Events[0].Outcome.Kind, res.Events[0].Outcome.Cause)
	}
	if len(res.Segments) != 0 {
		t.Errorf("implicit path leaked %d segments — must be 0", len(res.Segments))
	}
	if len(res.Entries) != 0 {
		t.Errorf("implicit path leaked %d entries — must be 0", len(res.Entries))
	}

	// Continuation turn — prior prefix established + observed
	// cached_tokens > 0 → implicit_hit.
	now2 := now.Add(time.Second)
	res2 := eng.ObserveTurn(ObserveInput{
		SessionID: "sess-implicit",
		Model:     "gpt-5",
		Scope:     "default",
		Tier:      TierProxy,
		Caps: Capabilities{
			ToolsVisible:        true,
			SystemVisible:       true,
			MarkersVisible:      true,
			UsageObserved:       true,
			BlocksAreCumulative: true,
			ImplicitCache:       true,
		},
		MessageID: "req-2",
		Now:       now2,
		Usage: CacheUsageObserved{
			NetInputTokens:  3072,
			OutputTokens:    100,
			CacheReadTokens: 2048, // matches the seeded prefix
		},
		APITurnID: 2,
	})
	if len(res2.Events) != 1 {
		t.Fatalf("continuation: events=%d, want 1", len(res2.Events))
	}
	if res2.Events[0].Outcome.Kind != KindImplicitHit {
		t.Errorf("continuation event kind = %q, want implicit_hit", res2.Events[0].Outcome.Kind)
	}
	// G2/G3 counters MUST stay at zero — implicit path never increments.
	if got := eng.Counters().BucketMispredictTotal; got != 0 {
		t.Errorf("BucketMispredictTotal after implicit-only turns = %d, want 0 — implicit path is leaking into the Anthropic G2 counter",
			got)
	}
	if got := eng.Counters().TriggerMispredictCalls; got != 0 {
		t.Errorf("TriggerMispredictCalls after implicit-only turns = %d, want 0 — implicit path is leaking into the Anthropic G3 counter",
			got)
	}

	// Continuation turn 3 — observed cached_tokens=0 on a warm
	// session → implicit_miss / prefix_churn.
	now3 := now2.Add(time.Second)
	res3 := eng.ObserveTurn(ObserveInput{
		SessionID: "sess-implicit",
		Model:     "gpt-5",
		Scope:     "default",
		Tier:      TierProxy,
		Caps: Capabilities{
			ToolsVisible:        true,
			SystemVisible:       true,
			MarkersVisible:      true,
			UsageObserved:       true,
			BlocksAreCumulative: true,
			ImplicitCache:       true,
		},
		MessageID: "req-3",
		Now:       now3,
		Usage: CacheUsageObserved{
			NetInputTokens:  3072,
			OutputTokens:    100,
			CacheReadTokens: 0, // cache evicted or prefix changed
		},
		APITurnID: 3,
	})
	if len(res3.Events) != 1 {
		t.Fatalf("churn turn: events=%d, want 1", len(res3.Events))
	}
	if res3.Events[0].Outcome.Kind != KindImplicitMiss || res3.Events[0].Outcome.Cause != CausePrefixChurn {
		t.Errorf("churn event = %q/%q, want implicit_miss/prefix_churn", res3.Events[0].Outcome.Kind, res3.Events[0].Outcome.Cause)
	}
}

// TestObserveTurn_ImplicitCache_DoesNotMoveAnthropicGate is the
// engine-side end-to-end variant of the §5 guardrail: run an
// Anthropic session and an implicit-cache session through the SAME
// Engine instance, then prove the Anthropic G2 counter is byte-
// identical to what an Anthropic-only run would produce.
func TestObserveTurn_ImplicitCache_DoesNotMoveAnthropicGate(t *testing.T) {
	t.Parallel()

	// Baseline: pure Anthropic run.
	engBaseline := NewEngine(0)
	runAnthropicTurns(t, engBaseline)
	baselineCounters := engBaseline.Counters()

	// Interleaved: same Anthropic run + a handful of implicit-cache
	// turns scattered between them on a separate session_id.
	engMixed := NewEngine(0)
	runAnthropicTurns(t, engMixed)
	runImplicitTurns(t, engMixed)
	// Re-run an Anthropic turn after the implicit traffic to catch
	// any state leak through the engine.
	runAnthropicTurns(t, engMixed)
	mixedCounters := engMixed.Counters()

	// The baseline counter snapshot is the lower bound — the mixed
	// run has TWO Anthropic runs, so its counters can be up to 2×
	// baseline (if Anthropic had mispredicts) or exactly 2× zero
	// (if it didn't). What matters: NO implicit-cache turn is in
	// the counters at all. If the implicit path leaked into the
	// G2 counter, mixedCounters would carry the implicit-cache
	// kind's mispredicts on top of 2× baseline.
	// We assert against the Anthropic-only DOUBLE run:
	engDoubleBaseline := NewEngine(0)
	runAnthropicTurns(t, engDoubleBaseline)
	runAnthropicTurns(t, engDoubleBaseline)
	wantCounters := engDoubleBaseline.Counters()

	if mixedCounters.BucketMispredictTotal != wantCounters.BucketMispredictTotal {
		t.Errorf("BucketMispredictTotal: mixed=%d, want=%d (2× Anthropic baseline=%d) — §5 GUARDRAIL FAILED: implicit-cache turns moved the Anthropic G2 counter",
			mixedCounters.BucketMispredictTotal, wantCounters.BucketMispredictTotal, baselineCounters.BucketMispredictTotal)
	}
	if mixedCounters.TriggerMispredictCalls != wantCounters.TriggerMispredictCalls {
		t.Errorf("TriggerMispredictCalls: mixed=%d, want=%d — §5 GUARDRAIL FAILED: implicit-cache turns moved the Anthropic G3 counter",
			mixedCounters.TriggerMispredictCalls, wantCounters.TriggerMispredictCalls)
	}
}

// runAnthropicTurns drives a tiny Anthropic-shape sequence through
// the engine (proxy tier, real blocks).
func runAnthropicTurns(t *testing.T, eng *Engine) {
	t.Helper()
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	// 3 turns: bootstrap (reanchor), continuation (hit), continuation (hit).
	for i := 0; i < 3; i++ {
		eng.ObserveTurn(ObserveInput{
			SessionID: "sess-anthropic",
			Model:     "claude-opus-4-8",
			Scope:     "default",
			Tier:      TierProxy,
			MessageID: "anthropic-msg-" + string(rune('a'+i)),
			Now:       base.Add(time.Duration(i) * time.Second),
			Blocks: []ObserveBlock{
				{Level: LevelTools, Kind: "tools", CanonicalBytes: []byte(`{"name":"shell"}`)},
				{Level: LevelSystem, Kind: "system", CanonicalBytes: []byte("you are claude")},
				{Level: LevelMessage, Kind: "user", CanonicalBytes: []byte("hello world")},
			},
			Usage: CacheUsageObserved{
				NetInputTokens:        100,
				OutputTokens:          50,
				CacheReadTokens:       int64(i * 10),
				CacheCreationTokens:   int64(50 + i*10),
				CacheCreation1hTokens: int64(50 + i*10),
			},
			APITurnID: int64(100 + i),
		})
	}
}

// runImplicitTurns drives a small OpenAI-implicit-cache sequence
// through the engine. Distinct session_id so the Anthropic session
// state isn't touched.
func runImplicitTurns(t *testing.T, eng *Engine) {
	t.Helper()
	base := time.Date(2026, 6, 10, 13, 0, 0, 0, time.UTC)
	caps := Capabilities{
		ToolsVisible:        true,
		SystemVisible:       true,
		MarkersVisible:      true,
		UsageObserved:       true,
		BlocksAreCumulative: true,
		ImplicitCache:       true,
	}
	// 5 turns to exercise bootstrap, hit, miss, hit-shrink, hit.
	scenarios := []struct {
		readTokens, inputTokens int64
	}{
		{0, 2048},    // bootstrap → implicit_write
		{2048, 3072}, // hit → implicit_hit
		{0, 4096},    // miss → implicit_miss
		{1024, 4096}, // hit_shrink → implicit_hit / prefix_shrink (if prior > 1024 + granule)
		{4096, 5120}, // hit → implicit_hit
	}
	for i, s := range scenarios {
		eng.ObserveTurn(ObserveInput{
			SessionID: "sess-openai",
			Model:     "gpt-5",
			Scope:     "default",
			Tier:      TierProxy,
			Caps:      caps,
			MessageID: "openai-msg-" + string(rune('A'+i)),
			Now:       base.Add(time.Duration(i) * time.Second),
			// No Blocks — implicit path skips the chain entirely.
			Usage: CacheUsageObserved{
				NetInputTokens:  s.inputTokens,
				OutputTokens:    100,
				CacheReadTokens: s.readTokens,
			},
			APITurnID: int64(200 + i),
		})
	}
}
