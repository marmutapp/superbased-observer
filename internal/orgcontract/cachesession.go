package orgcontract

// SessionCacheRow is the W2.1 SESSION-SCOPED cache-event summary — the org
// wire counterpart of the node's per-session Cache tab
// (web/src/components/sessiondetail/CacheTab.tsx /
// internal/intelligence/dashboard/cache.go::SessionCacheAnnotation). The
// fleet-only `cache_detail` tier (CacheSummaryRow, day × model × kind, no
// session_id) can't slice to a session; this row adds `session_id` so the
// org can render the same events/hits/writes/rewrites/ratio/mispredicts/
// tokens figures the node shows, scoped to one session.
//
// Grain: one row per (session_id, model, tier, kind, cause, zero_usage)
// bucket for a session touched in the recompute window — counts + token
// sums only, mirroring CacheSummaryRow's shape. `tier` and `zero_usage` are
// carried as extra dimensions (beyond CacheSummaryRow's day/model/kind) so
// the rollup can reconstruct the two node-side derived figures that would
// otherwise be lost by aggregation: the tier badge (proxy/transcript/mixed/
// none, collapsed from the distinct tiers observed) and the
// "[zero-usage, excluded from rate]" mispredict marker (cachetrack.
// MispredictRateGraded / CLAUDE.md cache-tracking §known-limitations) — both
// require knowing whether a bucket's events had tokens_read=0 AND
// tokens_written=0, which a coarser (session,model,kind) grain would erase.
//
// Content-free: no cache scope hash, no prefix hash, no prompt/tool content,
// no `detail` diagnostic JSON. This is ENTERPRISE-RAW wire (§0.1 of
// docs/plans/org-parity-full-depth-plan-2026-08-24.md) — it ships under
// shipsRawContent() (FullContent || AdminManaged), deliberately NOT the
// teams-tier `cache_detail` opt-in flag, because session-scoped telemetry is
// per-developer/per-session detail that the enterprise posture treats as
// admin-visible-by-default, not a content-free fleet aggregate.
type SessionCacheRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same stamping rule
	// as every other wire row).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	// SessionID is the session these cache events belong to.
	SessionID string `json:"session_id"`
	// Model is the model the cache events are for.
	Model string `json:"model"`
	// Tier is the cache-observation tier for this bucket's events: "proxy" |
	// "transcript" | "counts" (see internal/db/migrations/036_cache_tracking.sql).
	Tier string `json:"tier"`
	// Kind is the cache-event class: hit | write | expiry_rewrite |
	// invalidation_rewrite | model_switch_rewrite | compaction_reset |
	// reanchor | mispredict | below_min.
	Kind string `json:"kind"`
	// Cause is the §7 cause-vocabulary string (may be empty).
	Cause string `json:"cause"`
	// ZeroUsage is true when every event folded into this bucket had
	// tokens_read=0 AND tokens_written=0 (the cachetrack "observationally
	// vacant" case, excluded from mispredict-rate grading).
	ZeroUsage bool `json:"zero_usage"`
	// Events is the row count for this bucket.
	Events int64 `json:"events"`
	// TokensRead / TokensWritten are the summed cache token movements.
	TokensRead    int64 `json:"tokens_read"`
	TokensWritten int64 `json:"tokens_written"`
}
