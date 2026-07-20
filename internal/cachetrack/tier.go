package cachetrack

import (
	"strings"
	"time"
)

// Tier labels the provenance of a cache observation. The tier
// string survives only as a DATA LABEL on persisted rows
// (cache_segments.tier, cache_entries.tier, cache_events.tier);
// engine logic NEVER branches on a Tier value. Differences in
// what a source can see are encoded into [Capabilities] at the
// boundary; the core state machine + attribution tree read
// capability flags, not tier strings (spec §24.3).
type Tier uint8

// Tier values. The String form is persisted to the *.tier
// columns — stable across releases.
const (
	TierUnknown    Tier = iota
	TierProxy           // byte-exact: sees tools + system + markers
	TierTranscript      // claudecode JSONL reconstruction
	TierCounts          // adapters with only token counts + cadence
)

// String returns the canonical tier label persisted to the
// *.tier columns.
func (t Tier) String() string {
	switch t {
	case TierProxy:
		return "proxy"
	case TierTranscript:
		return "transcript"
	case TierCounts:
		return "counts"
	default:
		return "unknown"
	}
}

// Capabilities is the load-bearing structure of spec §24.3: the
// state machine + attribution tree read capability flags, never
// tier strings. Adding a new adapter, a new tier, or a new
// provider means setting capability flags at the boundary; the
// core logic in this package never changes.
//
// The four flags are independent — a future Tier could carry e.g.
// ToolsVisible without SystemVisible if it ever made sense. Don't
// add convenience methods that bundle them (it would re-introduce
// tier-coupling under a different name).
type Capabilities struct {
	// ToolsVisible is true when the source can observe the tools
	// array bytes (Tier 1 only). Gates §7 row 4
	// (tools_changed cause). When false, attribution falls back
	// to the residual `tools_or_system_changed`.
	ToolsVisible bool
	// SystemVisible is true when the source can observe the
	// system prompt bytes (Tier 1 only). Gates §7 row 5
	// (system_changed cause).
	SystemVisible bool
	// MarkersVisible is true when the source can observe real
	// cache_control marker positions (Tier 1 only). When false,
	// the engine assumes breakpoints via [assumedBreakpoints].
	MarkersVisible bool
	// UsageObserved is true when the source can observe per-turn
	// usage envelopes (Tier 1 + Tier 2; false for Tier 3 streaming
	// pre-final). Required for any read/write classification —
	// without it the engine cannot reconcile predictions.
	UsageObserved bool
	// BlocksAreCumulative reports whether ObserveInput.Blocks
	// carries the full prior conversation prefix in addition to the
	// new turn (Tier-1 proxy: API request body always includes the
	// entire conversation) or only the per-turn new content since
	// the last observation (Tier-2 transcript adapter: accumulator
	// emit shape resets pendingBlocks after each turn).
	//
	// Drives the predictKind suffix-token estimate at engine.go:
	// when true, estimateNewSuffixTokens slices in.Blocks from the
	// matched entry's BlockCount (the cumulative-block index at the
	// turn that created it) so the estimate scopes to the NEW tail
	// only. When false, in.Blocks is already the per-turn delta and
	// the full slice IS the new tail.
	//
	// MUST be set via [CapabilitiesFor] only — see engine.go's
	// boundary backfill at the top of ObserveTurn. A direct
	// Capabilities{} literal anywhere outside this package would
	// inherit zero-value false and silently mislabel a Tier-1 path
	// as delta; the predictKind path would then sum the entire
	// cumulative conversation and the CacheableTokens gate would
	// fire TRUE on every non-cold turn — the no-op trap §15.3 (c)
	// closes. The CapabilitiesFor pinning test
	// (TestCapabilitiesFor_BlocksAreCumulative_Pinning) guards
	// the per-tier values.
	BlocksAreCumulative bool
	// ImplicitCache reports that the provider caches automatically
	// with no explicit cache_control markers and reports only a
	// scalar cached-input count (OpenAI cached_tokens /
	// cached_input_tokens). When true the engine runs the REDUCED
	// implicit-cache attribution path ([attributeImplicit] in
	// implicit.go), NOT the Anthropic marker-based decision tree,
	// and the resulting events are EXCLUDED from the §10 Anthropic
	// MispredictRateGraded gate (numerator AND denominator) via
	// [bucketOf]'s bucketSkipped + [isRateSkipped]'s implicit-kinds
	// addition. The capability is the §15.3 seam: provider differences
	// resolve at the BOUNDARY (proxy.go for Tier-1 OpenAI; codex /
	// cline-cli / opencode / kilo adapters for Tier-2 implicit
	// routing), then the engine reads a single capability flag.
	// Never branch on provider/tier identity downstream of the
	// boundary.
	//
	// MUST be set via [CapabilitiesFor] OR resolved at the boundary
	// (proxy / adapter) before handing to [Engine.ObserveTurn]. Tier
	// alone does NOT carry the provider, so Tier-1 OpenAI starts as
	// TierProxy but the proxy seam overlays ImplicitCache=true; the
	// engine's boundary backfill respects already-set flags (the
	// `in.Caps == (Capabilities{})` guard only fills when the caller
	// left it zero). [TestCapabilitiesFor_ImplicitCache_Pinning]
	// guards the per-tier default values.
	ImplicitCache bool
}

// CapabilitiesFor returns the standard capability set for a tier.
// This is the SINGLE boundary where a tier label collapses into
// flags — every call site inside the engine consumes the returned
// Capabilities, never the Tier itself.
//
// ImplicitCache defaults to FALSE on every tier — provider identity
// (OpenAI vs Anthropic) is NOT carried by Tier alone, so the
// proxy/adapter boundary MUST overlay ImplicitCache=true when the
// observed traffic is implicit-cache shape (no markers, scalar
// cached_tokens). See [Capabilities.ImplicitCache].
func CapabilitiesFor(t Tier) Capabilities {
	switch t {
	case TierProxy:
		return Capabilities{
			ToolsVisible:        true,
			SystemVisible:       true,
			MarkersVisible:      true,
			UsageObserved:       true,
			BlocksAreCumulative: true,
			ImplicitCache:       false,
		}
	case TierTranscript:
		return Capabilities{
			ToolsVisible:        false,
			SystemVisible:       false,
			MarkersVisible:      false,
			UsageObserved:       true,
			BlocksAreCumulative: false,
			ImplicitCache:       false,
		}
	case TierCounts:
		return Capabilities{
			ToolsVisible:        false,
			SystemVisible:       false,
			MarkersVisible:      false,
			UsageObserved:       true,
			BlocksAreCumulative: false,
			ImplicitCache:       false,
		}
	default:
		return Capabilities{}
	}
}

// AssumedBreakpoint describes one inferred cache_control marker
// for a Tier-2 / Tier-3 turn where the engine cannot see the real
// request body. The engine creates one cache_entries row per
// AssumedBreakpoint per emitted observation (when
// MarkersVisible=false), nested in the order returned.
//
// Level is the chain level the breakpoint anchors at (tools /
// system / message). Rolling=true means the marker re-anchors at
// the tail on every turn (typical for the last message block);
// Rolling=false means it sits at a fixed boundary (typical for
// system breakpoints set by client conventions like Claude Code's
// system[1]+system[2]).
//
// TTL is the breakpoint's tier — drives `cache_entries.ttl_tier`
// at insert time.
type AssumedBreakpoint struct {
	Level   BlockLevel
	Rolling bool
	TTL     BlockTTL
}

// assumedBreakpoints returns the inferred cache_control breakpoint
// model the engine uses when MarkersVisible=false (Tier 2 / Tier 3).
//
// **R1(a) live capture (2026-06-08 operator) — RESOLVED.** A real
// Claude Code 2.1+ opus-4-8 request carries 3 explicit `cache_control`
// markers of the 4-max budget: two in the system array
// (`system[1]`, `system[2]`) plus one rolling marker on the last
// message block. All 1h tier. Tools blocks carry zero own markers
// but are cached under the first system breakpoint via the
// provider's hierarchy (tools → system → messages).
//
// Tier-2 transcripts cannot see system bytes, so the engine cannot
// anchor a precise prefix_hash for the system entries here — they
// are MODELED as placeholders the engine knows exist by client
// convention. The rolling message-level breakpoint is observable
// in transcripts and gets a real prefix_hash. Tier-1 (proxy, C8)
// enumerates real markers and does NOT consult this function.
//
// Spec §24.4 isolation rule: this is the SINGLE adjustment site if
// a future Claude Code SDK behavior change drops a breakpoint or
// adds a fourth. Engine call sites read it through this function;
// the returned slice's order maps to chain nesting order.
func assumedBreakpoints() []AssumedBreakpoint {
	return []AssumedBreakpoint{
		// R1(a) shape: 2 system breakpoints + 1 rolling last-message
		// breakpoint. We collapse the two system entries into one
		// "system-boundary" model here — Tier-2 can't tell them apart
		// (system bytes are invisible) and a single system-level entry
		// captures the same invalidation semantics. C8 Tier-1 detection
		// emits two real entries when the request body shows them
		// both.
		{Level: LevelSystem, Rolling: false, TTL: TTL1h},
		{Level: LevelMessage, Rolling: true, TTL: TTL1h},
	}
}

// LookbackWindow is the provider's per-breakpoint backward-walk
// bound (research doc §2.1, spec §6). Entries created more than
// this many blocks behind the current breakpoint predict a MISS
// even though they still exist in the model state
// (cause='lookback_window_missed', §7 row 8). Constant per
// Anthropic's documented behavior; not configurable.
const LookbackWindow = 20

// WithinLookback returns true when an entry created
// blocksSinceBreakpoint positions ago is still reachable from
// the current breakpoint position via the provider's backward
// walk. Negative inputs are out of range (defensive — should not
// occur, but a wrong-sign bug shouldn't produce false HITs).
func WithinLookback(blocksSinceBreakpoint int) bool {
	return blocksSinceBreakpoint >= 0 && blocksSinceBreakpoint <= LookbackWindow
}

// MaxBreakpoints is the provider's hard cap on cache_control
// markers per request (research doc §2.1, spec §6). Tier-1
// requests with more are provider-invalid; the engine logs a
// warning event and proceeds (skip, don't crash). Constant per
// Anthropic's documented limit.
const MaxBreakpoints = 4

// TTL5mDuration and TTL1hDuration are the two ephemeral cache
// TTL tiers Anthropic exposes (research doc §2.1). The state
// machine refreshes expires_at by adding the chosen duration.
const (
	TTL5mDuration = 5 * time.Minute
	TTL1hDuration = time.Hour
)

// TTLDuration returns the time.Duration for a BlockTTL. Unknown
// values fall back to the 5m default per spec §6.
func TTLDuration(ttl BlockTTL) time.Duration {
	switch ttl {
	case TTL1h:
		return TTL1hDuration
	default:
		return TTL5mDuration
	}
}

// minCacheableEntry maps a model-name substring to the per-family
// minimum prefix size required for an Anthropic provider cache to
// engage (research doc §2.1). Walked in order, first match wins;
// no match → defaultMinCacheable.
type minCacheableEntry struct {
	Match string
	Min   int
}

// minCacheableTable encodes the model-family → min-cacheable
// thresholds per research doc §2.1. Walked top-down by
// [MinCacheableTokens]; one row per family.
//
// Per the §24.5 table-driven discipline: keeping these as data
// (not an if/else ladder) means a future provider update is one
// row insertion — and the matching test (TestMinCacheableTokens)
// gets a one-line row for the new family.
var minCacheableTable = []minCacheableEntry{
	// Larger min on the Opus 4.5–4.7 + Haiku 4.5 family per
	// research doc §2.1.
	{"opus-4-5", 4096},
	{"opus-4-6", 4096},
	{"opus-4-7", 4096},
	{"haiku-4-5", 4096},
	// Smaller min on older Haiku.
	{"haiku-3-5", 2048},
}

// defaultMinCacheable is the threshold for the Anthropic models
// not covered by [minCacheableTable] (Opus 4.8, Sonnet 4.5/4.6,
// Opus 4/4.1 — the v2026-Q2 standard). 1,024 tokens per research
// doc §2.1.
const defaultMinCacheable = 1024

// MinCacheableTokens returns the per-model-family minimum prefix
// size required for an Anthropic provider cache to engage. Below
// threshold → no entry, kind='below_min', cause=
// 'below_min_cacheable' (§7 row 10).
//
// Family matching is a substring scan (case-sensitive) because
// model strings carry SKU suffixes (e.g. `claude-opus-4-7`,
// `claude-haiku-3-5-20240620`) that include the family marker
// verbatim. A future provider id scheme that breaks this
// assumption is a one-line table fix.
func MinCacheableTokens(model string) int {
	for _, e := range minCacheableTable {
		if strings.Contains(model, e.Match) {
			return e.Min
		}
	}
	return defaultMinCacheable
}
