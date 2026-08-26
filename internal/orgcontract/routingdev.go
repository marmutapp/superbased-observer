package orgcontract

// RoutingDevRow is the W2.3 per-developer routing detail wire row: one row
// per (org_id, user_email, day, original_model, selected_model, turn_kind,
// mode), mirroring internal/store/routingdetail.go's SelectRoutingDetail
// aggregation exactly (same GROUP BY dimensions, same COUNT/SUM math over
// router_decisions) but composed under the shipsRawContent() gate
// (enterprise-raw wire) instead of the share.RoutingDetail /
// share.RoutingSummary opt-in tiers. This is DELIBERATELY a new
// table/row, not a retrofit of RoutingDetailRow/routing_details — the
// existing teams-tier surfaces (routing_summaries, routing_details) stay
// byte-identical and admin-only/identity-blind (see
// docs/plans/org-parity-full-depth-plan-2026-08-24.md §0, §4 "W2.3").
//
// Unlike RoutingSummaryRow (deliberately model-id-free per §R19.4 — it
// carries only a tier/model-class dimension), this row DOES carry the raw
// original/selected model ids, same as RoutingDetailRow. That's fine here:
// under the enterprise posture the admin already sees everything for the
// developer this row is attributed to (§0.1 "Enterprise = zero dev
// privacy"), so there is no additional disclosure by naming the model.
//
// NEVER carries: session_id, api_turn_id, policy body text, reason codes,
// or any prompt/response content — router_decisions doesn't feed those
// into this aggregate either (PolicyName/PolicyHash/ReasonCodes exist on
// the source table but are intentionally left off this row, matching
// RoutingDetailRow's own omission).
type RoutingDevRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same stamping
	// rule as every other wire row — see ingest.go forcePusherOrgID /
	// forcePusherEmail).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`

	// Day is the router_decisions.ts date, truncated to YYYY-MM-DD
	// (substr(ts,1,10), same convention as RoutingDetailRow.Day).
	Day string `json:"day"`
	// OriginalModel / SelectedModel are the raw model ids the router
	// considered/chose — see the type doc comment for why this is safe
	// to carry on the enterprise per-dev row unlike the summary tier.
	OriginalModel string `json:"original_model"`
	SelectedModel string `json:"selected_model"`
	// TurnKind / Mode mirror router_decisions.turn_kind / .mode
	// (RoutingDetailRow's own dimensions) — e.g. turn_kind "edit"/"plan",
	// mode "advise"/"enforce".
	TurnKind string `json:"turn_kind"`
	Mode     string `json:"mode"`

	// Decisions is COUNT(*) of router_decisions rows in this bucket.
	Decisions int64 `json:"decisions"`
	// Switched is COUNT of rows where the router actually rewrote the
	// model (router_decisions.applied != 0) — the per-dev vocabulary
	// asked for "switched count"; RoutingDetailRow calls the identical
	// aggregate "Applied".
	Switched int64 `json:"switched"`
	// EstSavingsUSD / CacheForfeitUSD are SUM(est_savings_usd) /
	// SUM(cache_forfeit_usd) over the bucket — the same two dollar
	// columns RoutingDetailRow carries.
	EstSavingsUSD   float64 `json:"est_savings_usd"`
	CacheForfeitUSD float64 `json:"cache_forfeit_usd"`
}
