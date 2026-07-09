package cachewarm

import (
	"sort"
	"time"
)

// Severity is the closed-set urgency of a cache window, stable across
// releases (the dashboard / VS Code / MCP surfaces map these to colors
// and labels).
type Severity string

const (
	// SeverityOK — the cache is warm with comfortable headroom (more
	// than WarnAt left). Returned so a status surface can render a warm
	// countdown; not itself an actionable "warning".
	SeverityOK Severity = "ok"
	// SeveritySoon — expiry is within the WarnAt window. Nudge-worthy.
	SeveritySoon Severity = "soon"
	// SeverityCritical — expiry is within the CriticalAt window. Act now
	// or pay a cold re-write next turn.
	SeverityCritical Severity = "critical"
	// SeverityCold — the cache has already passed expires_at. The next
	// matching turn pays the full write tier again.
	SeverityCold Severity = "cold"
)

// CacheWindow is the normalized, provider-agnostic view of one cache the
// classifier reasons over. It is assembled at the store/boundary seam
// from a cache_entries row plus per-token rates from cost.Table; the
// classifier never sees raw provider names — capability differences are
// pre-resolved into the flag fields below.
type CacheWindow struct {
	// Model is the cache's model id (for display/grouping only — the
	// classifier does not branch on it).
	Model string `json:"model"`
	// Scope is the cache_scope hash (workspace/org key). Display/grouping.
	Scope string `json:"scope"`
	// SessionID is the creating session (informational; may be empty).
	SessionID string `json:"session_id"`
	// PrefixTokens is the cached prefix size (cache_entries.token_count).
	PrefixTokens int64 `json:"prefix_tokens"`
	// TTLTier is "5m" or "1h" — the provider TTL tier this cache holds.
	TTLTier string `json:"ttl_tier"`
	// ExpiresAt is the modelled expiry instant (cache_entries.expires_at).
	ExpiresAt time.Time `json:"expires_at"`
	// LastRefresh is the last read/write that reset the TTL
	// (cache_entries.last_refresh_at).
	LastRefresh time.Time `json:"last_refresh"`

	// ExpiryAuthoritative is true when the expiry instant is derived from
	// a byte-exact proxy observation (Anthropic), false when it is an
	// estimate (OpenAI implicit caching, which exposes no TTL). Drives
	// the Warning.Estimated flag so a surface can hedge the countdown.
	ExpiryAuthoritative bool `json:"expiry_authoritative"`
	// RefreshableByPatch is true only for providers whose cache is
	// addressable and whose TTL can be extended without resending content
	// (Gemini explicit CachedContent). Consumed by Recommend (Part B).
	RefreshableByPatch bool `json:"refreshable_by_patch"`
	// Supports1hTier is true for Anthropic, where the cheapest content-free
	// keep-warm lever is switching the cache_control breakpoint to the 1h
	// TTL tier. Consumed by Recommend (Part B).
	Supports1hTier bool `json:"supports_1h_tier"`
	// ValueAtRiskUSD is the dollar delta of letting this cache go cold vs
	// keeping it warm for one resumed turn: PrefixTokens × (r_write − r_read).
	// Resolved at the boundary from cost.Table for the window's TTL tier.
	ValueAtRiskUSD float64 `json:"value_at_risk_usd"`

	// WarnAtOverride / CriticalAtOverride optionally replace the
	// ClassifyInput thresholds FOR THIS WINDOW. Zero means "use the
	// ClassifyInput defaults". This exists because providers expire on
	// different scales: an Anthropic explicit cache has a hard TTL instant
	// (seconds-scale warn/critical), whereas an implicit (OpenAI/Codex)
	// cache has no fixed lease — its eviction risk grows over hours, so the
	// boundary maps a 24h hard-max expiry with hour-scale warn/critical
	// bands ("at risk" / "significantly increased risk"). Mixing both kinds
	// in one Classify call requires per-window thresholds. It is a
	// capability of the window, not a branch on source identity.
	WarnAtOverride     time.Duration `json:"-"`
	CriticalAtOverride time.Duration `json:"-"`
}

// ClassifyInput bundles the live windows, the evaluation clock, and the
// severity/value thresholds (sourced from [cachewarm] config at the
// boundary). All time.Now reads happen in the caller — this package takes
// Now as a parameter for testability + purity.
type ClassifyInput struct {
	// Windows is the set of live (and, when IncludeCold, recently-expired)
	// caches to classify.
	Windows []CacheWindow
	// Now is the evaluation instant.
	Now time.Time
	// WarnAt is the time-to-expiry boundary for SeveritySoon. Zero falls
	// back to DefaultWarnAt.
	WarnAt time.Duration
	// CriticalAt is the time-to-expiry boundary for SeverityCritical.
	// Zero falls back to DefaultCriticalAt.
	CriticalAt time.Duration
	// MinValueUSD suppresses windows whose ValueAtRiskUSD is below this
	// floor (caches not worth warning about). Zero includes everything.
	MinValueUSD float64
	// IncludeCold controls whether already-expired (SeverityCold) windows
	// are returned. A status surface wants them ("cache cold"); a pure
	// "expiring soon" warning feed may not.
	IncludeCold bool
}

// Default severity boundaries used when ClassifyInput leaves them zero.
const (
	// DefaultWarnAt is the SeveritySoon boundary.
	DefaultWarnAt = 90 * time.Second
	// DefaultCriticalAt is the SeverityCritical boundary.
	DefaultCriticalAt = 30 * time.Second
)

// Warning is one classified cache window. Despite the name it carries the
// full status spectrum (including SeverityOK) so a single call serves both
// the status surface (all windows) and the warning feed (callers filter to
// Severity != SeverityOK).
type Warning struct {
	// Window is the classified cache.
	Window CacheWindow `json:"window"`
	// Severity is the urgency bucket.
	Severity Severity `json:"severity"`
	// SecondsToExpiry is ExpiresAt − Now in whole seconds; negative once
	// the cache is cold.
	SecondsToExpiry int64 `json:"seconds_to_expiry"`
	// ValueAtRiskUSD echoes Window.ValueAtRiskUSD for convenience.
	ValueAtRiskUSD float64 `json:"value_at_risk_usd"`
	// Estimated is true when the underlying expiry is an estimate
	// (!Window.ExpiryAuthoritative) — the surface should hedge the
	// countdown ("~").
	Estimated bool `json:"estimated"`
}

// Classify scores each window against the thresholds and returns them
// ordered most-urgent-first (severity rank desc, then soonest expiry,
// then highest value), deterministic. Pure function; safe for concurrent
// handlers.
//
// Windows below MinValueUSD are dropped entirely. Cold windows are dropped
// unless IncludeCold is set.
func Classify(in ClassifyInput) []Warning {
	warnAt := in.WarnAt
	if warnAt <= 0 {
		warnAt = DefaultWarnAt
	}
	criticalAt := in.CriticalAt
	if criticalAt <= 0 {
		criticalAt = DefaultCriticalAt
	}
	// Guard the inversion case: a critical boundary must not exceed the
	// warn boundary, else every "soon" would also read "critical".
	if criticalAt > warnAt {
		criticalAt = warnAt
	}

	out := make([]Warning, 0, len(in.Windows))
	for _, w := range in.Windows {
		if in.MinValueUSD > 0 && w.ValueAtRiskUSD < in.MinValueUSD {
			continue
		}
		// Per-window thresholds let Anthropic (seconds-scale hard TTL) and
		// implicit (hour-scale graded risk) windows be classified in one
		// pass; zero overrides fall back to the ClassifyInput defaults.
		wWarn, wCrit := warnAt, criticalAt
		if w.WarnAtOverride > 0 {
			wWarn = w.WarnAtOverride
		}
		if w.CriticalAtOverride > 0 {
			wCrit = w.CriticalAtOverride
		}
		if wCrit > wWarn {
			wCrit = wWarn
		}
		remaining := w.ExpiresAt.Sub(in.Now)
		sev := severityFor(remaining, wWarn, wCrit)
		if sev == SeverityCold && !in.IncludeCold {
			continue
		}
		out = append(out, Warning{
			Window:          w,
			Severity:        sev,
			SecondsToExpiry: int64(remaining / time.Second),
			ValueAtRiskUSD:  w.ValueAtRiskUSD,
			Estimated:       !w.ExpiryAuthoritative,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := severityRank(out[i].Severity), severityRank(out[j].Severity)
		if ri != rj {
			return ri > rj // higher urgency first
		}
		if out[i].SecondsToExpiry != out[j].SecondsToExpiry {
			return out[i].SecondsToExpiry < out[j].SecondsToExpiry // soonest first
		}
		return out[i].ValueAtRiskUSD > out[j].ValueAtRiskUSD // most valuable first
	})
	return out
}

// severityFor buckets a time-to-expiry into a Severity. remaining ≤ 0 is
// cold; within criticalAt is critical; within warnAt is soon; else ok.
func severityFor(remaining, warnAt, criticalAt time.Duration) Severity {
	switch {
	case remaining <= 0:
		return SeverityCold
	case remaining <= criticalAt:
		return SeverityCritical
	case remaining <= warnAt:
		return SeveritySoon
	default:
		return SeverityOK
	}
}

// severityRank orders severities for sorting (higher = more urgent).
// Cold ranks above Critical because a cold cache is already costing the
// next write.
func severityRank(s Severity) int {
	switch s {
	case SeverityCold:
		return 3
	case SeverityCritical:
		return 2
	case SeveritySoon:
		return 1
	default:
		return 0
	}
}
