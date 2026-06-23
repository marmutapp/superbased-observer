package cachetrack

import "time"

// Kind is the §6 kind label persisted to cache_events.kind.
// Closed vocabulary — stable across releases (schema-load-bearing).
type Kind string

// Kind values (the cache_events.kind closed set).
const (
	KindHit                 Kind = "hit"
	KindWrite               Kind = "write"
	KindExpiryRewrite       Kind = "expiry_rewrite"
	KindInvalidationRewrite Kind = "invalidation_rewrite"
	KindModelSwitchRewrite  Kind = "model_switch_rewrite"
	KindCompactionReset     Kind = "compaction_reset"
	KindReanchor            Kind = "reanchor"
	KindMispredict          Kind = "mispredict"
	// KindBelowMin — prefix below the model's min-cacheable
	// threshold; no entry will be created.
	//
	// Reconciliation note (C9 / §10): the mispredict-rate
	// denominator MUST EXCLUDE below_min events. They are not
	// predictions — the engine knew up-front the cache would
	// not engage, so the observation is uninformative for
	// §10's "did we predict hit-vs-write correctly?" health
	// metric. C9's SELECT filters `WHERE kind != 'below_min'`.
	KindBelowMin Kind = "below_min"

	// --- §15.3 implicit-cache kinds (OpenAI / OpenAI-compatible
	// providers with no cache_control markers and only a scalar
	// cached-input count). Emitted only when caps.ImplicitCache
	// is true at engine boundary. All three are EXCLUDED from the
	// §10 Anthropic MispredictRateGraded gate (numerator AND
	// denominator) via [bucketOf]'s bucketSkipped + [isRateSkipped].
	// The separate implicit-cache consistency metric
	// [ImplicitCacheConsistency] grades these directly. ---

	// KindImplicitHit — observed cached_tokens > 0 on a turn
	// whose prior turn established a stable prefix. The
	// implicit-cache analog of [KindHit]; distinct value so
	// the §10 Anthropic gate filters cleanly and a downstream
	// dashboard can pick the right "implicit cache, lower
	// fidelity" framing. Causes: [CauseImplicitHit] (within
	// expected band) or [CausePrefixShrink] (cached < tracked
	// prefix — the prefix shrank).
	KindImplicitHit Kind = "implicit_hit"
	// KindImplicitMiss — observed cached_tokens = 0 on a turn
	// whose prior turn HAD a stable prefix ≥ min-cacheable. The
	// OpenAI analog of an invalidation. Cause: [CausePrefixChurn]
	// (prefix changed OR provider-side eviction; implicit cache
	// gives us no way to tell them apart). Not a §10 mispredict
	// (no Anthropic-grade attribution to compare against).
	KindImplicitMiss Kind = "implicit_miss"
	// KindImplicitWrite — first turn for a (session, model, scope)
	// under implicit cache, or a turn whose prefix grew past a
	// new 128-token granule on the proxy LCP path. Cause:
	// [CausePromptCacheKeyOverflow] when the proxy sees a
	// prompt_cache_key roll; otherwise the unflagged baseline
	// [CauseSuffixGrowth]. Skipped from §10 by design.
	KindImplicitWrite Kind = "implicit_write"
)

// Cause is the §7 cause label persisted to cache_events.cause.
// Closed vocabulary, operator-facing — drives the dashboard
// pill colors and tooltips. Stable across releases.
type Cause string

// Cause values (the cache_events.cause closed set).
const (
	// CauseReanchor — first turn for (session, model, scope)
	// after daemon start (row 1).
	CauseReanchor Cause = "reanchor"
	// CauseModelChanged — model differs from prior turn (row 2).
	// Per §0 R4 refinement, the cause is set BEFORE the kind
	// falls out of the new model's entry lookup: a switch CAN
	// land on a warm cache and emit kind=hit + cause=model_changed.
	CauseModelChanged Cause = "model_changed"
	// CauseFastToggle — speed:"fast" flipped vs prior (row 3).
	CauseFastToggle Cause = "fast_toggle"
	// CauseToolsChanged — Tier-1 tools-level boundary hash
	// differs (row 4). Gated on caps.ToolsVisible.
	CauseToolsChanged Cause = "tools_changed"
	// CauseSystemChanged — Tier-1 system-level boundary hash
	// differs (row 5). Gated on caps.SystemVisible.
	CauseSystemChanged Cause = "system_changed"
	// CauseToolsOrSystemChanged — Tier-2 RESIDUAL: when neither
	// tools nor system is visible AND a rewrite happened AND no
	// other rule fired, attribute to the residual cause.
	// Specifically called out by R1 transcript-half finding.
	CauseToolsOrSystemChanged Cause = "tools_or_system_changed"
	// CauseContextCompacted — Tier-2 compact_boundary OR Tier-1
	// wholesale chain replacement (row 6).
	CauseContextCompacted Cause = "context_compacted"
	// CauseTTLExpired — now > matched entry's expires_at (row 7).
	CauseTTLExpired Cause = "ttl_expired"
	// CauseLookbackMissed — a live entry exists but is more
	// than LookbackWindow blocks behind the current breakpoint
	// (row 8); provider's backward walk won't find it.
	CauseLookbackMissed Cause = "lookback_window_missed"
	// CauseBlockDiverged — first divergent message block at
	// seq=DivergedSeq (row 9).
	CauseBlockDiverged Cause = "block_diverged"
	// CauseBelowMinCacheable — prefix size below model's min-
	// cacheable threshold (row 10); no entry will be created.
	CauseBelowMinCacheable Cause = "below_min_cacheable"
	// CauseParallelColdStart — Tier-1: a request raced against
	// an inflight prior request that hadn't finished writing
	// the prefix yet (row 11).
	CauseParallelColdStart Cause = "parallel_cold_start"
	// CauseSuffixGrowth — the unflagged baseline: a rewrite/
	// hit that matches the expected incremental shape. Per §7
	// preamble: "every turn in a growing conversation writes
	// some new suffix — the normal case is kind='write',
	// cause='suffix_growth', and it is NOT flagged."
	CauseSuffixGrowth Cause = "suffix_growth"
	// CauseUnknown — row 12 fallthrough: nothing matched and
	// no usage signal was observed. Reconciliation (§10)
	// scores these as mispredicts.
	CauseUnknown Cause = "unknown"

	// --- §15.3 implicit-cache causes. Pair with [KindImplicitHit] /
	// [KindImplicitMiss] / [KindImplicitWrite]. Closed vocabulary,
	// operator-facing. ---

	// CauseImplicitHit — observed cached_tokens > 0 within the
	// expected band of the per-session tracked prefix length.
	// The implicit-cache normal-operating-point cause; pairs with
	// [KindImplicitHit].
	CauseImplicitHit Cause = "implicit_hit"
	// CausePrefixChurn — observed cached_tokens = 0 on a turn
	// whose prior turn established a stable prefix. The OpenAI
	// analog of invalidation: either the prompt prefix changed
	// (system message edit, tools array change) OR the provider
	// evicted the cache (TTL expiry, capacity pressure). Implicit
	// cache gives us no way to distinguish the two causes —
	// dashboard copy must reflect that uncertainty. Pairs with
	// [KindImplicitMiss].
	CausePrefixChurn Cause = "prefix_churn"
	// CausePrefixShrink — observed cached_tokens > 0 but
	// materially BELOW the tracked prefix length (prefix
	// shortened by ≥ one 128-token granule from prior). A
	// partial-invalidation signal: the cache hit, but on a
	// shorter prefix than expected. Pairs with [KindImplicitHit].
	CausePrefixShrink Cause = "prefix_shrink"
	// CausePromptCacheKeyOverflow — optional per §15.3: when the
	// proxy detects a `prompt_cache_key` roll on the OpenAI
	// request body (≥ 15 rpm per cache_key forces a roll), the
	// causal label captures that. Set only when the proxy passes
	// an explicit overflow signal in the ObserveInput; today no
	// caller does, so this cause is inert — wired for the
	// follow-up. Pairs with [KindImplicitWrite].
	CausePromptCacheKeyOverflow Cause = "prompt_cache_key_overflow"
)

// PriorSignals carries cross-turn state from the previous
// observation on this CacheModel. Nil when the current turn is
// the first for (session, model, scope) — that's the row 1
// reanchor condition. ToolsLevelHash and SystemLevelHash are
// empty when the prior turn did not observe them.
type PriorSignals struct {
	Model           string
	Fast            bool
	ToolsLevelHash  string
	SystemLevelHash string
	// FirstResponseMS is the prior turn's request-to-first-event
	// latency in milliseconds. Zero when not observed (Tier 2/3
	// transcripts log post-response). Used by row 11.
	FirstResponseMS int64
	// SecondsSince is the wall-clock delta between the prior
	// turn's start and the current turn's Now, in seconds. Used
	// by row 11: when the gap is smaller than the prior turn's
	// first-response latency, the current turn was inflight
	// against an uncached prefix.
	SecondsSince float64
}

// AttributeInput is the per-turn observation handed to Attribute.
// The engine pre-fills it from the observed-usage envelope plus
// the running CacheModel state (chain hashes, entry lookup,
// prior-turn signals). The rule walk consumes ONLY this struct
// — no globals, no time.Now, no map lookups inside rules.
type AttributeInput struct {
	// Caps gates Tier-1-only rules. Engine call sites read
	// these flags, never tier strings (spec §24.3).
	Caps Capabilities

	// Current-turn signals.
	Model        string
	Fast         bool
	Now          time.Time
	PrefixHash   string // current cumulative chain hash (hex)
	PrefixTokens int

	// Observed usage from the provider envelope. Zero values =
	// not observed.
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64

	// Compaction signal (Tier-2 compact_boundary OR Tier-1
	// wholesale chain replacement).
	Compaction bool

	// Level-boundary hashes — empty when the corresponding cap
	// is false. ToolsLevelHash is computed only when caps.
	// ToolsVisible; same for SystemLevelHash.
	ToolsLevelHash  string
	SystemLevelHash string

	// First differing message-level block index vs prior chain.
	// -1 means "no divergence detected" OR "unknown" — the rule
	// treats these the same (no row 9 fire).
	DivergedSeq int

	// Engine-supplied: blocks pushed since the last marker.
	// Used by row 8 (lookback miss) via WithinLookback.
	BlocksSinceBreakpoint int

	// Prior-turn signals — nil when this is the first turn for
	// (session, model, scope). Row 1 (reanchor) fires on nil.
	Prior *PriorSignals

	// Engine-resolved entry lookup for in.PrefixHash; nil when
	// no entry matches. Pre-resolved so the rule walk stays
	// pure.
	MatchedEntry *Entry
}

// AttributeOutcome is the engine's verdict for one observed
// turn. Persisted to cache_events as (kind, cause, diverged_seq,
// diverged_level). DivergedSeq is -1 when not applicable;
// DivergedLevel is LevelUnknown when not applicable.
type AttributeOutcome struct {
	Kind          Kind
	Cause         Cause
	DivergedSeq   int
	DivergedLevel BlockLevel
}

// attributionRule is one row of the §7 decision table. Walked
// top-down by [Attribute]; first match wins.
//
// Fn returns (matched, kind, divergedSeq, divergedLevel). When
// matched=false the other return values are ignored. The kind
// is row-specific (model_changed's kind depends on the matched
// entry's state per §0 R4 refinement; everything else is fixed).
type attributionRule struct {
	Cause Cause
	Fn    func(in AttributeInput) (matched bool, kind Kind, divergedSeq int, divergedLevel BlockLevel)
}

// attributionRules is the §7 decision table. Order matters:
// first match wins, so a more-specific cause (model_changed)
// must precede a less-specific cause that would also match
// (ttl_expired). See TestAttribute_Precedence_b9bd459d for the
// load-bearing example.
//
// §24.5 discipline: keep this as data, not an if/else ladder.
// A new cause means one row insertion + one test row.
var attributionRules = []attributionRule{
	{Cause: CauseReanchor, Fn: ruleReanchor},
	{Cause: CauseModelChanged, Fn: ruleModelChanged},
	{Cause: CauseFastToggle, Fn: ruleFastToggle},
	{Cause: CauseToolsChanged, Fn: ruleToolsChanged},
	{Cause: CauseSystemChanged, Fn: ruleSystemChanged},
	{Cause: CauseContextCompacted, Fn: ruleContextCompacted},
	{Cause: CauseTTLExpired, Fn: ruleTTLExpired},
	{Cause: CauseLookbackMissed, Fn: ruleLookbackMissed},
	{Cause: CauseBlockDiverged, Fn: ruleBlockDiverged},
	{Cause: CauseToolsOrSystemChanged, Fn: ruleToolsOrSystemResidual},
	{Cause: CauseBelowMinCacheable, Fn: ruleBelowMinCacheable},
	{Cause: CauseParallelColdStart, Fn: ruleParallelColdStart},
}

// Attribute walks the §7 decision table top-down and returns
// the verdict. The capability flags on in.Caps gate rows that
// depend on Tier-1-only visibility (tools_changed, system_changed);
// row 11 (parallel_cold_start) self-gates on prior FirstResponseMS
// (Tier 2/3 leave it zero, so the rule won't fire). The residual
// 'tools_or_system_changed' row sits between block_diverged and
// below_min so Tier-2 catches the rewrite case Tier-1 rows 4–5
// would have caught.
//
// When no rule matches:
//   - CacheCreationTokens > 0 → KindWrite + CauseSuffixGrowth
//     (the unflagged baseline — normal incremental write).
//   - CacheReadTokens > 0 → KindHit + CauseSuffixGrowth
//     (clean hit on the predicted entry as the conversation grows).
//   - neither observed → KindMispredict + CauseUnknown
//     (row 12 fallthrough; §10 reconciliation scores these).
func Attribute(in AttributeInput) AttributeOutcome {
	for _, r := range attributionRules {
		if matched, kind, divergedSeq, divergedLevel := r.Fn(in); matched {
			return AttributeOutcome{
				Kind:          kind,
				Cause:         r.Cause,
				DivergedSeq:   divergedSeq,
				DivergedLevel: divergedLevel,
			}
		}
	}
	if in.CacheCreationTokens > 0 {
		return AttributeOutcome{
			Kind:          KindWrite,
			Cause:         CauseSuffixGrowth,
			DivergedSeq:   -1,
			DivergedLevel: LevelUnknown,
		}
	}
	if in.CacheReadTokens > 0 {
		return AttributeOutcome{
			Kind:          KindHit,
			Cause:         CauseSuffixGrowth,
			DivergedSeq:   -1,
			DivergedLevel: LevelUnknown,
		}
	}
	return AttributeOutcome{
		Kind:          KindMispredict,
		Cause:         CauseUnknown,
		DivergedSeq:   -1,
		DivergedLevel: LevelUnknown,
	}
}

// --- Rule implementations (one per §7 row) ---

// ruleReanchor — Row 1: first turn for (session, model, scope)
// after daemon start. Engine sets in.Prior=nil when no prior
// observation exists.
func ruleReanchor(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.Prior == nil {
		return true, KindReanchor, -1, LevelUnknown
	}
	return false, "", 0, LevelUnknown
}

// ruleModelChanged — Row 2: model differs from prior turn.
// §0 R4 refinement: cause stays model_changed, kind falls out of
// the new model's entry lookup. A switch that lands on a warm
// prefix at the same scope reads from cache (the b9bd459d 07:32
// pattern, observed cache_read=101690).
func ruleModelChanged(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.Prior == nil || in.Prior.Model == in.Model {
		return false, "", 0, LevelUnknown
	}
	if in.MatchedEntry != nil && in.MatchedEntry.State == StateLive &&
		in.MatchedEntry.Key.Model == in.Model {
		return true, KindHit, -1, LevelUnknown
	}
	return true, KindModelSwitchRewrite, -1, LevelUnknown
}

// ruleFastToggle — Row 3: speed:"fast" flipped vs prior turn.
func ruleFastToggle(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.Prior != nil && in.Prior.Fast != in.Fast {
		return true, KindInvalidationRewrite, -1, LevelUnknown
	}
	return false, "", 0, LevelUnknown
}

// ruleToolsChanged — Row 4: tools-level boundary hash differs.
// Tier-1-only; gated by caps.ToolsVisible. Requires prior signals
// to compare against.
func ruleToolsChanged(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if !in.Caps.ToolsVisible || in.Prior == nil {
		return false, "", 0, LevelUnknown
	}
	if in.ToolsLevelHash != "" && in.ToolsLevelHash != in.Prior.ToolsLevelHash {
		return true, KindInvalidationRewrite, -1, LevelTools
	}
	return false, "", 0, LevelUnknown
}

// ruleSystemChanged — Row 5: system-level boundary hash differs.
// Tier-1-only; gated by caps.SystemVisible. Requires prior.
func ruleSystemChanged(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if !in.Caps.SystemVisible || in.Prior == nil {
		return false, "", 0, LevelUnknown
	}
	if in.SystemLevelHash != "" && in.SystemLevelHash != in.Prior.SystemLevelHash {
		return true, KindInvalidationRewrite, -1, LevelSystem
	}
	return false, "", 0, LevelUnknown
}

// ruleContextCompacted — Row 6: compaction marker since prior
// turn (Tier-2 compact_boundary OR Tier-1 wholesale chain
// replacement). Engine sets in.Compaction.
func ruleContextCompacted(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.Compaction {
		return true, KindCompactionReset, -1, LevelUnknown
	}
	return false, "", 0, LevelUnknown
}

// ruleTTLExpired — Row 7: now ≥ matched entry's expires_at.
// Uses ≥ rather than > so a turn that lands exactly at the
// expiry instant is treated as a miss (the provider's wall-clock
// is not synchronized with ours; equality is the conservative
// edge).
func ruleTTLExpired(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.MatchedEntry == nil {
		return false, "", 0, LevelUnknown
	}
	if !in.Now.Before(in.MatchedEntry.ExpiresAt) {
		return true, KindExpiryRewrite, -1, LevelUnknown
	}
	return false, "", 0, LevelUnknown
}

// ruleLookbackMissed — Row 8: a live entry exists but is more
// than LookbackWindow blocks behind the current breakpoint.
// Provider's backward walk won't find it; predicts MISS even
// though the entry still exists in our model state.
//
// Tier 2 approximates BlocksSinceBreakpoint from message count
// (less precise; spec §7 footnote flags this).
func ruleLookbackMissed(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.MatchedEntry == nil || in.MatchedEntry.State != StateLive {
		return false, "", 0, LevelUnknown
	}
	if !WithinLookback(in.BlocksSinceBreakpoint) {
		return true, KindWrite, -1, LevelUnknown
	}
	return false, "", 0, LevelUnknown
}

// ruleBlockDiverged — Row 9: first divergent message block at
// seq=DivergedSeq. Engine fills in.DivergedSeq when it detects
// a chain mismatch at the message level. -1 means "no divergence
// or unknown" — the rule does not fire.
func ruleBlockDiverged(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.DivergedSeq >= 0 {
		return true, KindInvalidationRewrite, in.DivergedSeq, LevelMessage
	}
	return false, "", 0, LevelUnknown
}

// ruleToolsOrSystemResidual — Tier-2 RESIDUAL: when neither
// tools nor system is visible (Tier 2 / Tier 3) AND a rewrite
// was observed AND no other rule fired, attribute to the
// residual cause. Placed AFTER block_diverged so a real
// message-level divergence gets the precise cause first; placed
// BEFORE below_min so a Tier-2 small-prefix rewrite still gets
// the residual attribution rather than below_min_cacheable.
//
// Requires a prior turn (otherwise reanchor would have fired).
//
// NOTE: the CauseToolsOrSystemChanged label this rule emits is
// a RESIDUAL CATCH-ALL, NOT literal change detection. Tier-2
// transcripts NEVER emit LevelTools or LevelSystem blocks (R1
// finding), so ToolsLevelHash and SystemLevelHash are always
// empty on Tier-2 input — the engine has no way to know whether
// tools/system actually changed. A future debugger investigating
// `cause=tools_or_system_changed` rows in cache_events should
// NOT chase a phantom tools-or-system change: the cause label
// just means "Tier-2 write turn after the first." Confirmed by
// the §14.3 per-level hash dump tests in
// `internal/cachetrack/fixture_grading_test.go`
// (TestPerLevelHashDump_*) which assert tools+system are empty
// on every turn for opencode/kilo-cli emitters.
func ruleToolsOrSystemResidual(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.Caps.ToolsVisible || in.Caps.SystemVisible {
		return false, "", 0, LevelUnknown
	}
	if in.Prior == nil || in.CacheCreationTokens <= 0 {
		return false, "", 0, LevelUnknown
	}
	return true, KindInvalidationRewrite, -1, LevelUnknown
}

// ruleBelowMinCacheable — Row 10: prefix below model minimum.
// Below threshold → kind=below_min, no entry created downstream
// by the engine.
func ruleBelowMinCacheable(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.PrefixTokens > 0 && !CacheableTokens(in.Model, in.PrefixTokens) {
		return true, KindBelowMin, -1, LevelUnknown
	}
	return false, "", 0, LevelUnknown
}

// ruleParallelColdStart — Row 11: second request within
// ~first-response latency of prior (parallel race). T1 only —
// self-gates on prior.FirstResponseMS > 0 (Tier 2/3 leave it
// zero because transcripts log post-response).
//
// Conservative trigger (operator review post-C4): rule fires
// only when the timing evidence is unambiguous. Two guards
// keep false positives down — the rule is on the soak watch-
// list:
//
//   - parallelLatencyFloorSeconds: the prior turn's first-
//     response latency must be at least 1 second. Sub-second
//     latencies don't give a wide enough window to infer
//     "inflight against an uncached prefix"; small jitter
//     dominates the signal.
//   - parallelGapFraction: the gap from prior-turn start to now
//     must be clearly less than HALF the prior latency. A gap
//     close to (or above) the prior latency is ambiguous —
//     the prior request could have finished writing.
//
// Ambiguous cases fall through to the suffix_growth /
// unknown baselines downstream. The two thresholds are
// tunable engine logic (not schema) — they can be re-tuned
// during P0 soak without changing the cause vocabulary.
const (
	parallelLatencyFloorSeconds = 1.0
	parallelGapFraction         = 0.5
)

func ruleParallelColdStart(in AttributeInput) (bool, Kind, int, BlockLevel) {
	if in.Prior == nil || in.Prior.FirstResponseMS <= 0 {
		return false, "", 0, LevelUnknown
	}
	if in.CacheCreationTokens <= 0 {
		return false, "", 0, LevelUnknown
	}
	gapSeconds := in.Prior.SecondsSince
	if gapSeconds <= 0 {
		return false, "", 0, LevelUnknown
	}
	firstResponseSeconds := float64(in.Prior.FirstResponseMS) / 1000.0
	if firstResponseSeconds < parallelLatencyFloorSeconds {
		return false, "", 0, LevelUnknown
	}
	if gapSeconds >= parallelGapFraction*firstResponseSeconds {
		return false, "", 0, LevelUnknown
	}
	return true, KindWrite, -1, LevelUnknown
}
