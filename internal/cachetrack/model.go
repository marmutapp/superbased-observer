package cachetrack

import "time"

// EntryState is the lifecycle state of a modelled cache entry
// (spec §6 state machine). Persisted as the cache_entries.state
// column — labels are stable across releases.
type EntryState uint8

// EntryState values. The String form is persisted to
// cache_entries.state.
const (
	StateUnknown EntryState = iota
	// StateLive — we believe the provider holds this prefix.
	StateLive
	// StateExpired — clock passed expires_at; kept for diagnosis
	// (Tier-1 forecaster + Tier-2 stale-read split need history).
	StateExpired
	// StateInvalidated — chain evidence shows prefix changed at
	// or before the entry's block (compaction, model switch,
	// tools/system invalidation).
	StateInvalidated
	// StateUnverified — reconciliation contradicted the belief
	// for this entry; re-resolved on the next observed turn.
	StateUnverified
)

// String returns the canonical state label persisted to
// cache_entries.state. Stable across releases.
func (s EntryState) String() string {
	switch s {
	case StateLive:
		return "live"
	case StateExpired:
		return "expired"
	case StateInvalidated:
		return "invalidated"
	case StateUnverified:
		return "unverified"
	default:
		return "unknown"
	}
}

// Trigger labels the kind of event that can cause an entry to
// transition between states. Keys into [stateTransitions] (§6,
// §24.5 table-driven). Trigger strings are NOT persisted — only
// state labels are; trigger is an in-memory engine vocabulary.
type Trigger uint8

// Trigger values.
const (
	TriggerUnknown Trigger = iota
	// TriggerRead — provider reported a cache_read on this prefix.
	TriggerRead
	// TriggerWrite — provider reported a cache_creation on this
	// prefix.
	TriggerWrite
	// TriggerClockExpiry — wall-clock time advanced past
	// expires_at without a touching read/write.
	TriggerClockExpiry
	// TriggerPrefixDiverged — a later turn's chain shows the
	// prefix changed at or before this entry's block.
	TriggerPrefixDiverged
	// TriggerMispredict — reconciliation contradicted the
	// belief (predicted hit, got write; or vice versa).
	TriggerMispredict
	// TriggerCompactionReset — Tier-2 compact_boundary or
	// Tier-1 wholesale chain replacement invalidates the entry.
	TriggerCompactionReset
)

// EntryKey is the natural identity of a cache entry per spec §6:
// `(model, cache_scope, prefix_hash)`. Multi-agent sharing inside
// the same org/workspace through a single proxy is a feature
// (matches provider behavior), not a bug.
type EntryKey struct {
	Model      string // e.g. claude-opus-4-7
	Scope      string // cache_scope hash (R7 derivation)
	PrefixHash string // hex chain hash
}

// Entry is one modelled provider-cache entry. The state machine
// in [Entry.Apply] walks the §6 transitions; the per-row table
// in [stateTransitions] is the single source of truth.
//
// Level records the chain level at which the entry was created —
// needed for compaction (only message-level entries invalidate)
// and for §7 row 4–5 attribution downstream.
//
// BlockSeqOnCreate records the chain block-sequence index at
// entry-creation time. The lookback check
// (§7 row 8 / [WithinLookback]) compares this against the current
// breakpoint position.
//
// BlockCount records len(in.Blocks) at the turn that created the
// entry. For Tier-1 cumulative callers this is the count of blocks
// in the conversation prefix at entry creation; a subsequent turn's
// in.Blocks at the same scope grows monotonically, so the next
// turn's new-tail is in.Blocks[BlockCount:]. This is the slice
// anchor that closes §15.3 (c)-phase-2's predictedNewTokens delta
// computation. Crucially: BlockCount is in in.Blocks units, NOT
// chain units — m.Chain.Count() accumulates across every push
// regardless of cumulative vs delta, so it diverges from
// len(in.Blocks) on every Tier-1 turn past T2 and produces a
// wrong-anchor sum-all no-op. For Tier-2 delta callers BlockCount
// is recorded but never consulted (caps.BlocksAreCumulative=false
// at the predict site skips the slicing entirely).
type Entry struct {
	Key              EntryKey
	Level            BlockLevel
	SessionID        string // creating session (informational)
	TokenCount       int64  // reconciled when usage available
	TTL              BlockTTL
	Tier             Tier
	CreatedAt        time.Time
	LastRefreshAt    time.Time
	ExpiresAt        time.Time
	State            EntryState
	BlockSeqOnCreate int
	BlockCount       int
}

// stateTransition is one row of the §6 transition table. From +
// Trigger select the row; To is the destination state;
// RefreshTTL says whether expires_at should advance.
type stateTransition struct {
	From       EntryState
	Trigger    Trigger
	To         EntryState
	RefreshTTL bool
}

// stateTransitions is the §6 state-machine table. Rows are
// walked top-down in [Entry.Apply]; first (From, Trigger) match
// wins. A trigger with no matching row is a no-op (the entry
// stays in its current state) — this is the correct shape for
// triggers fired on already-terminal states (e.g. a delayed
// TriggerRead arriving on a StateExpired entry).
//
// §24.5 discipline: keep this as data, not an if/else ladder.
// One row per documented transition; the matching test has one
// case per row.
var stateTransitions = []stateTransition{
	// live + observed read or write → refresh TTL, stay live.
	{From: StateLive, Trigger: TriggerRead, To: StateLive, RefreshTTL: true},
	{From: StateLive, Trigger: TriggerWrite, To: StateLive, RefreshTTL: true},
	// live + clock past expires_at → expired (kept for diagnosis).
	{From: StateLive, Trigger: TriggerClockExpiry, To: StateExpired, RefreshTTL: false},
	// live + prefix divergence → invalidated.
	{From: StateLive, Trigger: TriggerPrefixDiverged, To: StateInvalidated, RefreshTTL: false},
	// live + reconciliation contradiction → unverified (re-resolves next turn).
	{From: StateLive, Trigger: TriggerMispredict, To: StateUnverified, RefreshTTL: false},
	// live + compaction → invalidated.
	{From: StateLive, Trigger: TriggerCompactionReset, To: StateInvalidated, RefreshTTL: false},
	// unverified + confirming observation → back to live (refresh).
	{From: StateUnverified, Trigger: TriggerRead, To: StateLive, RefreshTTL: true},
	{From: StateUnverified, Trigger: TriggerWrite, To: StateLive, RefreshTTL: true},
	// unverified + another mispredict → stays unverified.
	{From: StateUnverified, Trigger: TriggerMispredict, To: StateUnverified, RefreshTTL: false},
	// unverified + clock expiry → expired (don't keep an unverified
	// entry around past its TTL window).
	{From: StateUnverified, Trigger: TriggerClockExpiry, To: StateExpired, RefreshTTL: false},
}

// Apply walks [stateTransitions] and returns whether a refresh
// fired. The entry's State + LastRefreshAt + ExpiresAt are
// updated in place. now is supplied by the caller (no time.Now
// in this package — testability + spec §24.1 purity).
//
// Returns refreshed=true when the TTL advanced. Engine callers
// emit cache_events from the return value (hit vs write
// classification lives in attribute.go / C4, not here).
func (e *Entry) Apply(trigger Trigger, now time.Time) (refreshed bool) {
	for _, row := range stateTransitions {
		if row.From == e.State && row.Trigger == trigger {
			e.State = row.To
			if row.RefreshTTL {
				e.LastRefreshAt = now
				e.ExpiresAt = now.Add(TTLDuration(e.TTL))
			}
			return row.RefreshTTL
		}
	}
	return false
}

// ObservedTTL chooses a BlockTTL from observed-usage signals per
// spec §6 + R1 emphasis: ephemeral_1h_input_tokens > 0 → TTL1h;
// otherwise → TTL5m. Tier-1 callers may pass a request-side
// marker TTL (from cache_control ttl:"1h"); when set, it takes
// precedence — the marker tells us what we ASKED for, the usage
// tells us what landed, and we believe the request shape.
//
// Per R1 finding: "1h-tier dominates for Claude Code (observed
// 1h=32393, 5m=0 on the captured turn). §6 TTL detection MUST
// key on ephemeral_1h_input_tokens > 0, not assume 5m."
func ObservedTTL(creation1hTokens int64, markerTTL BlockTTL) BlockTTL {
	if markerTTL == TTL1h || markerTTL == TTL5m {
		return markerTTL
	}
	if creation1hTokens > 0 {
		return TTL1h
	}
	return TTL5m
}

// CacheModel is the per-(session, model, scope) engine state.
// Holds the current chain plus the entry set keyed by prefix
// hash. Owned by the engine; callers serialize access through
// the engine's mutex (R-engine = Option A, spec §0 / §24.4).
//
// Cross-turn signals (LastModel, LastFast, LastMessageID,
// LastTurnAt) are consumed by §7 attribution (C4) and reset on
// compaction.
type CacheModel struct {
	// Chain is the rolling-hash chain for the current turn-set.
	Chain *Chain

	// Entries holds live + diagnosed entries keyed by
	// prefix_hash hex. Lookups are O(1).
	Entries map[string]*Entry

	// Cross-turn signals — read by §7 attribution (C4).
	LastModel     string
	LastFast      bool
	LastMessageID string
	LastTurnAt    time.Time

	// LastToolsLevelHash is the cumulative chain hash at the
	// END of the tools section of the prior turn's request.
	// Drives §7 row 4 (tools_changed): when the current turn's
	// tools-end hash differs from this, tools changed.
	// Empty for the first turn (no prior signal); also empty
	// when the prior turn had no tools (Capabilities.ToolsVisible
	// must additionally be true for row 4 to fire).
	LastToolsLevelHash string
	// LastSystemLevelHash is the cumulative chain hash at the
	// END of the system section of the prior turn. Drives §7
	// row 5 (system_changed) the same way LastToolsLevelHash
	// drives row 4. Empty when no prior or no system content.
	LastSystemLevelHash string

	// BlocksSinceLastBreakpoint counts blocks pushed since the
	// last marker. Read by §7 row 8 / [WithinLookback].
	BlocksSinceLastBreakpoint int

	// EstimateEMA carries the per-session R6 token-estimate
	// calibration. Updated by ReconcilePrediction's
	// (observed_written / estimated_written) ratio every
	// confirmed-write turn (UpdateEMA in reconcile.go). The
	// forecaster (§14.2) multiplies its per-byte estimates by
	// EstimateEMA.Scale to get session-calibrated predictions.
	// Zero / uninitialized falls through to the v1 constant.
	EstimateEMA SessionEMA

	// ImplicitPrefixTokens is the §15.3 per-session estimate of
	// the stable cached prefix length, quantized to 128-token
	// granules ([OpenAIPrefixGranule]). Set ONLY on the
	// implicit-cache attribution path (caps.ImplicitCache); zero
	// on Anthropic sessions. The proxy LCP tracker (Phase 2)
	// updates this from the observed cached_tokens scalar +
	// the request-body prefix length; the codex Tier-2 emitter
	// (Phase 3) updates this from the scalar alone (lower
	// fidelity — no prefix-byte access in transcripts). Implicit
	// attribution's [ImplicitObservation.PriorPrefixTokens] reads
	// from here at the engine boundary.
	ImplicitPrefixTokens int64

	// ImplicitObserved is true once at least one implicit-cache
	// turn has been observed for this (session, model, scope).
	// Drives [attributeImplicit]'s reanchor row (the bootstrap
	// turn cannot have a prior estimate).
	ImplicitObserved bool
}

// NewCacheModel returns a fresh per-session CacheModel with an
// empty chain and no entries.
func NewCacheModel() *CacheModel {
	return &CacheModel{
		Chain:   NewChain(),
		Entries: make(map[string]*Entry),
	}
}

// FindByPrefix returns the entry whose Key.PrefixHash matches, or
// nil. O(1) map lookup.
func (m *CacheModel) FindByPrefix(prefixHex string) *Entry {
	return m.Entries[prefixHex]
}

// AddEntry inserts an entry into the model. Caller is
// responsible for setting CreatedAt / LastRefreshAt / ExpiresAt
// per its source clock. Existing entries with the same prefix
// hash are replaced.
func (m *CacheModel) AddEntry(e *Entry) {
	m.Entries[e.Key.PrefixHash] = e
}

// SweepExpired walks live and unverified entries and flips any
// whose ExpiresAt is at or before now to StateExpired. Returns
// the slice of flipped entries so the engine can emit
// cache_events for §7 row 7 (ttl_expired cause).
func (m *CacheModel) SweepExpired(now time.Time) []*Entry {
	var out []*Entry
	for _, e := range m.Entries {
		if (e.State == StateLive || e.State == StateUnverified) && !now.Before(e.ExpiresAt) {
			e.State = StateExpired
			out = append(out, e)
		}
	}
	return out
}

// CompactionReset invalidates every live message-level entry for
// the session and resets the chain. Tools- and system-level
// entries are NOT touched — compaction only clears the messages
// array. Returns the slice of invalidated entries so the engine
// can emit one cache_event per (kind='compaction_reset', cause=
// 'context_compacted', §7 row 6). The next observed turn lands
// as `kind='reanchor'` per the §6 compaction note.
func (m *CacheModel) CompactionReset() []*Entry {
	var out []*Entry
	for _, e := range m.Entries {
		if e.State == StateLive && e.Level == LevelMessage {
			e.State = StateInvalidated
			out = append(out, e)
		}
	}
	m.Chain.Reset()
	m.BlocksSinceLastBreakpoint = 0
	return out
}

// CacheableTokens returns true when the prefix size meets the
// model's minimum-cacheable threshold (§6 row 10 / §7 row 10).
// Below threshold → no entry, no event, `kind='below_min'`. The
// model parameter feeds [MinCacheableTokens].
func CacheableTokens(model string, tokens int) bool {
	return tokens >= MinCacheableTokens(model)
}
