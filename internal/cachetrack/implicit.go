package cachetrack

// §15.3 implicit-cache attribution path. KEPT SEPARATE from
// attribute.go (the Anthropic §7 decision tree) per the plan-of-
// record: provider differences resolve at the BOUNDARY via
// Capabilities.ImplicitCache, then the engine routes to one of two
// disjoint attribution paths. Folding implicit-cache rules into
// attribute.go would re-introduce the very provider-coupling the
// capability model is designed to prevent.
//
// Implicit caching (OpenAI / OpenAI-compatible providers,
// deepseek-flash via OpenRouter, codex on responses API) gives us:
//   - One scalar per turn: cached_tokens (count of input tokens
//     served from cache; a subset of total prompt tokens).
//   - Zero markers (no cache_control), zero TTL signal, no separate
//     write-vs-read count.
//
// So the reduced attribution treats every turn as "scalar
// observation against a per-session estimated stable prefix
// length." The state machine has 4 outputs:
//
//   1. KindImplicitWrite + CauseReanchor — first turn for the
//      (session, model, scope) tuple. Bootstrap; nothing to compare
//      against.
//   2. KindImplicitWrite + CauseSuffixGrowth — normal continuation
//      turn whose prefix grew past a new 128-token granule. The
//      unflagged baseline.
//   3. KindImplicitHit + CauseImplicitHit / CausePrefixShrink —
//      observed cached_tokens > 0; CauseImplicitHit when within the
//      expected band, CausePrefixShrink when the cached fraction
//      is below the tracked prefix (partial-invalidation signal).
//   4. KindImplicitMiss + CausePrefixChurn — observed cached_tokens
//      = 0 on a turn whose prior state established a stable prefix
//      above min-cacheable. The OpenAI analog of invalidation.
//   5. KindBelowMin + CauseBelowMinCacheable — tracked prefix
//      below model min. Reuses Anthropic's row 10 since the math
//      is identical.
//
// Plus one wired-but-inert row for the future:
//
//   6. KindImplicitWrite + CausePromptCacheKeyOverflow — when the
//      caller passes an explicit overflow signal (today no caller
//      does; placeholder for the §15.3 prompt_cache_key follow-up).
//
// All three implicit-cache Kinds fold into bucketSkipped (reconcile.go
// bucketOf) and isRateSkipped (reconcile.go) so the Anthropic §10
// MispredictRateGraded gate is PROVABLY UNMOVED by implicit-cache
// events — the load-bearing §5 guardrail. The separate
// ImplicitCacheConsistency metric grades these directly.
//
// The decision set is intentionally a small table-driven function
// (per CLAUDE.md §24.5) — adding a new implicit cause means one
// row insertion + one test row.

// OpenAIPrefixGranule is the documented 128-token granule at which
// OpenAI's implicit prompt cache matches. The proxy LCP tracker
// (Phase 2) quantizes the tracked prefix length to this granule;
// observed cached_tokens deviations of less than one granule are
// noise, not signal. Constant per OpenAI's public documentation.
const OpenAIPrefixGranule = 128

// implicitDefaultMinCacheable is the documented OpenAI minimum
// prompt size for the implicit cache to engage (gpt-4o, gpt-4o-mini,
// gpt-5* families all share this floor). When the tracked prefix
// is below this, [attributeImplicit] returns KindBelowMin with
// CauseBelowMinCacheable, matching the Anthropic row-10 shape.
//
// MUST hold ≤ defaultMinCacheable (1024); a wider implicit floor
// would silently demote OpenAI events that Anthropic would have
// graded — by-design conservatism here means the implicit-cache
// gate is honest about what the provider DOES cache, not what we
// HOPE the provider caches.
const implicitDefaultMinCacheable = 1024

// ImplicitObservation is the per-turn signal the implicit
// attribution path consumes. Pure-data; built at the engine
// boundary from in.Usage.CacheReadTokens + a per-session estimate
// of the tracked prefix length.
type ImplicitObservation struct {
	// Model is the request model string (drives MinCacheableTokens
	// in the rare case where an OpenAI-family override is added
	// — today every OpenAI family falls back to the default).
	Model string
	// PriorObserved indicates whether this is the bootstrap turn
	// (false) or a continuation turn (true). Set from the engine's
	// in.Prior != nil signal.
	PriorObserved bool
	// PriorPrefixTokens is the session's pre-turn estimate of the
	// stable cached prefix length, quantized to 128-token granules
	// upstream. Zero on bootstrap turns.
	PriorPrefixTokens int64
	// ObservedCachedTokens is the count of input tokens reported
	// by the provider as served from cache (cached_tokens /
	// cached_input_tokens).
	ObservedCachedTokens int64
	// PromptCacheKeyOverflow is the optional explicit signal the
	// proxy raises when it sees a prompt_cache_key roll on the
	// request body. Today no caller sets it; wired for the
	// follow-up.
	PromptCacheKeyOverflow bool
}

// attributeImplicit walks the §15.3 reduced decision set top-down
// and returns the verdict for one implicit-cache turn. Pure
// function of inputs; no I/O, no time.Now (the implicit path is
// TTL-blind by design).
//
// Decision order (first match wins):
//
//  1. PromptCacheKeyOverflow (optional, wired-but-inert today) →
//     KindImplicitWrite + CausePromptCacheKeyOverflow
//  2. !PriorObserved (bootstrap) → KindImplicitWrite + CauseReanchor
//  3. PriorPrefixTokens > 0 && PriorPrefixTokens <
//     MinCacheableImplicit(Model) → KindBelowMin +
//     CauseBelowMinCacheable
//  4. ObservedCachedTokens == 0 && PriorPrefixTokens >=
//     MinCacheableImplicit(Model) → KindImplicitMiss +
//     CausePrefixChurn
//  5. ObservedCachedTokens > 0 && ObservedCachedTokens <
//     PriorPrefixTokens - OpenAIPrefixGranule → KindImplicitHit +
//     CausePrefixShrink (cached less than tracked — prefix shrank)
//  6. ObservedCachedTokens > 0 → KindImplicitHit + CauseImplicitHit
//  7. (fallthrough) ObservedCachedTokens == 0 && PriorPrefixTokens
//     < MinCacheableImplicit(Model) → KindImplicitWrite +
//     CauseSuffixGrowth (normal pre-cacheable growth)
//
// The table-driven shape lets future cause additions land as one
// row + one test row (§24.5).
func attributeImplicit(obs ImplicitObservation) AttributeOutcome {
	for _, r := range implicitAttributionRules {
		if matched, kind, cause := r.Fn(obs); matched {
			return AttributeOutcome{
				Kind:          kind,
				Cause:         cause,
				DivergedSeq:   -1,
				DivergedLevel: LevelUnknown,
			}
		}
	}
	// Final fallthrough — cached_tokens=0 and prefix below min:
	// normal pre-cacheable growth.
	return AttributeOutcome{
		Kind:          KindImplicitWrite,
		Cause:         CauseSuffixGrowth,
		DivergedSeq:   -1,
		DivergedLevel: LevelUnknown,
	}
}

// implicitAttributionRule is one row of the §15.3 implicit-cache
// decision table. Same shape as attributionRule in attribute.go
// but tuned for the implicit-cache signal vocabulary.
type implicitAttributionRule struct {
	Cause Cause
	Fn    func(obs ImplicitObservation) (matched bool, kind Kind, cause Cause)
}

// implicitAttributionRules is the §15.3 reduced decision table.
// Walked top-down; first match wins. Keep as data (§24.5):
// adding a cause is one row + one test row.
var implicitAttributionRules = []implicitAttributionRule{
	{Cause: CausePromptCacheKeyOverflow, Fn: ruleImplicitPromptCacheKeyOverflow},
	{Cause: CauseReanchor, Fn: ruleImplicitReanchor},
	{Cause: CauseBelowMinCacheable, Fn: ruleImplicitBelowMin},
	{Cause: CausePrefixChurn, Fn: ruleImplicitPrefixChurn},
	{Cause: CausePrefixShrink, Fn: ruleImplicitPrefixShrink},
	{Cause: CauseImplicitHit, Fn: ruleImplicitHit},
}

// ruleImplicitPromptCacheKeyOverflow — Row 1: the proxy raised the
// prompt_cache_key overflow signal. Today no caller sets it (the
// proxy doesn't yet detect it); wired here so a future overflow-
// detector lands as one boundary edit, not an engine refactor.
func ruleImplicitPromptCacheKeyOverflow(obs ImplicitObservation) (bool, Kind, Cause) {
	if obs.PromptCacheKeyOverflow {
		return true, KindImplicitWrite, CausePromptCacheKeyOverflow
	}
	return false, "", ""
}

// ruleImplicitReanchor — Row 2: bootstrap turn for a (session,
// model, scope) tuple. Implicit-cache analog of the Anthropic
// reanchor: nothing to predict against, no prior prefix established.
func ruleImplicitReanchor(obs ImplicitObservation) (bool, Kind, Cause) {
	if !obs.PriorObserved {
		return true, KindImplicitWrite, CauseReanchor
	}
	return false, "", ""
}

// ruleImplicitBelowMin — Row 3: tracked prefix below the model's
// implicit-cache minimum. Reuses Anthropic's row-10 shape: the
// engine knew up-front the cache wouldn't engage, so the
// observation is uninformative.
func ruleImplicitBelowMin(obs ImplicitObservation) (bool, Kind, Cause) {
	if obs.PriorPrefixTokens > 0 && obs.PriorPrefixTokens < int64(MinCacheableImplicit(obs.Model)) {
		return true, KindBelowMin, CauseBelowMinCacheable
	}
	return false, "", ""
}

// ruleImplicitPrefixChurn — Row 4: observed cached_tokens = 0 on
// a turn whose prior state established a stable prefix above the
// min-cacheable floor. The OpenAI analog of invalidation; we can't
// tell whether the cache was evicted by TTL/pressure or whether the
// prefix changed — dashboard copy must reflect that uncertainty.
func ruleImplicitPrefixChurn(obs ImplicitObservation) (bool, Kind, Cause) {
	if obs.ObservedCachedTokens == 0 && obs.PriorPrefixTokens >= int64(MinCacheableImplicit(obs.Model)) {
		return true, KindImplicitMiss, CausePrefixChurn
	}
	return false, "", ""
}

// ruleImplicitPrefixShrink — Row 5: observed cached_tokens > 0
// but materially below the tracked prefix (by more than one
// 128-token granule). A partial-invalidation signal: the cache
// hit, but on a shorter prefix than expected.
//
// Materiality threshold = one 128-token granule. Sub-granule
// deviations are noise (the proxy quantizes the tracked prefix
// upstream, but the provider's granule alignment is not
// deterministic in the documentation).
func ruleImplicitPrefixShrink(obs ImplicitObservation) (bool, Kind, Cause) {
	if obs.ObservedCachedTokens <= 0 || obs.PriorPrefixTokens <= 0 {
		return false, "", ""
	}
	if obs.ObservedCachedTokens < obs.PriorPrefixTokens-int64(OpenAIPrefixGranule) {
		return true, KindImplicitHit, CausePrefixShrink
	}
	return false, "", ""
}

// ruleImplicitHit — Row 6: observed cached_tokens > 0 within the
// expected band. The implicit-cache normal-operating-point.
func ruleImplicitHit(obs ImplicitObservation) (bool, Kind, Cause) {
	if obs.ObservedCachedTokens > 0 {
		return true, KindImplicitHit, CauseImplicitHit
	}
	return false, "", ""
}

// MinCacheableImplicit returns the per-model-family implicit-cache
// minimum for OpenAI / OpenAI-compatible providers. Today every
// family falls back to [implicitDefaultMinCacheable] (1024 tokens
// per OpenAI's public documentation). A future provider-specific
// override (e.g. a higher floor for a specific OpenAI family or
// for deepseek-flash via OpenRouter) lands as one row in a future
// table here — same §24.5 discipline as the Anthropic
// [minCacheableTable].
func MinCacheableImplicit(model string) int {
	_ = model // placeholder for future per-family overrides
	return implicitDefaultMinCacheable
}

// predictImplicitKind generates the engine's pre-observation guess
// about an implicit-cache turn's Kind, used by the consistency
// metric (PredictedKind != observed Kind = inconsistency surface,
// analogous to the §10 mispredict signal but on a separate axis).
//
// The prediction logic mirrors the observed-side rule walk but
// reads PRIOR state (the per-session prefix tracker's pre-turn
// estimate), not the observed cached_tokens scalar:
//
//   - !PriorObserved → KindImplicitWrite (bootstrap)
//   - PriorPrefixTokens < min-cacheable → KindBelowMin
//   - PriorPrefixTokens >= min-cacheable → KindImplicitHit
//     (we expect a hit; if observed cached_tokens turns out to be 0
//     the actual outcome is KindImplicitMiss and the consistency
//     metric flags the inconsistency)
//
// Pure function; no time.Now.
func predictImplicitKind(obs ImplicitObservation) Kind {
	if !obs.PriorObserved {
		return KindImplicitWrite
	}
	if obs.PriorPrefixTokens > 0 && obs.PriorPrefixTokens < int64(MinCacheableImplicit(obs.Model)) {
		return KindBelowMin
	}
	if obs.PriorPrefixTokens >= int64(MinCacheableImplicit(obs.Model)) {
		return KindImplicitHit
	}
	// Below min and not yet established — predict bootstrap-shape
	// write (the pre-cacheable growth path).
	return KindImplicitWrite
}

// QuantizeToGranule rounds n DOWN to the nearest multiple of the
// granule. Used by the proxy LCP tracker (Phase 2) so the stored
// PriorPrefixTokens is granule-aligned and sub-granule observed
// deviations don't false-flag as shrinks.
func QuantizeToGranule(n int64, granule int) int64 {
	if granule <= 0 || n <= 0 {
		return 0
	}
	return (n / int64(granule)) * int64(granule)
}
