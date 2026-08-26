package orgcontract

// obsegress.go carries the W5.3 wire rows for Plane-A egress routing
// visibility (docs/plans/org-parity-full-depth-plan-2026-08-24.md §4
// "W5.3") — the org admin's view of the node's outbound-call routing
// decisions (allowed/blocked, target upstream). This is the T8 obs tier;
// see internal/store/orgpush.go's composeObsTiers for T1-T7.
//
// Ownership resolution (recorded here since the plan's §3 capability matrix
// informally calls this a "Plane-B guard" gap): obs_egress_decisions is a
// Plane-A subsystem. internal/obs/store/migrations/0007_egress.sql's own
// doc comment says so directly ("v1 ships NO egress org tier ... no org
// wire, no server pair" — this wire is what closes that gap), and the row
// records a model/provider ROUTING decision (route_upstream | route_model |
// set_effort | deny | none), not a literal network/process egress fact.
// Literal network/process egress guardrails on the developer's own tool
// calls are a distinct Plane-B subsystem (internal/guard) already on the
// wire via GuardEvents — unrelated to this type.
//
// Field shapes mirror internal/obs/store/egress.go's EgressDecisionRow /
// EgressDecisionView exactly — no columns are invented. Per the honesty
// rule (CLAUDE.md's integration-registry convention: "zero value = no
// grounded capability, never fabricated"), there is deliberately NO raw
// destination host/URL field and NO byte-count field: the node table never
// captures either, so this wire type doesn't pretend to. UpstreamID (a
// symbolic operator-config key) and TargetShape are the closest grounded
// "where did this call go" substitutes.
//
// Gating: like every other obs_* tier (T1-T7), Egress requires its OWN
// [org_client.share.obs].egress opt-in (ShareOptions.ObsEgress), default
// false, node-side only, never server-forced — independent of AdminManaged.
// AdminManaged/shipsRawContent() only additionally governs the two
// content-bearing columns within an already-opted-in tier (Tenant/User,
// mirroring ObsAdmissionRow's Tenant/EndUser/ReasonExcerpt posture).

// ObsEgressRow is one row of the node's obs_egress_decisions table (an
// egress-boundary routing decision: allow-through with an optional
// upstream/model/effort override, or a deny). Immutable event-log shape
// (hash-chained on the node), not current state — ingested via
// INSERT OR IGNORE keyed on RowHash, same posture as ObsAdmissionRow.
type ObsEgressRow struct {
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`

	TS string `json:"ts"`

	Mode     string `json:"mode"`      // advise | enforce
	RuleName string `json:"rule_name"` // the egress rule that fired

	PolicyHash string `json:"policy_hash"` // compiled egress policy hash; no policy-snapshot table exists to soft-join against (v1)

	// Action is the verbatim decision vocabulary the node's egress engine
	// emits — none | route_upstream | route_model | set_effort | deny — never
	// translated/relabeled at this boundary.
	Action string `json:"action"`

	// UpstreamID is a symbolic config key naming a configured upstream (e.g.
	// "openrouter") — not a raw host/URL, because the node itself never
	// records one for this decision.
	UpstreamID  string `json:"upstream_id,omitempty"`
	TargetShape string `json:"target_shape,omitempty"` // anthropic | openai
	ModelFrom   string `json:"model_from,omitempty"`
	ModelTo     string `json:"model_to,omitempty"`
	Effort      string `json:"effort,omitempty"`
	ReasonCode  string `json:"reason_code"`

	MustUseTarget bool `json:"must_use_target"`
	Applied       bool `json:"applied"`
	FailClosed    bool `json:"fail_closed"`
	SwitchHeld    bool `json:"switch_held"`

	EstCacheForfeitClass string `json:"est_cache_forfeit_class,omitempty"`
	Degraded             string `json:"degraded,omitempty"`

	// RealizedOutcome mirrors the node's post-hoc realized-outcome
	// annotation (migration 0008) at whatever value it held when pushed —
	// deliberately excluded from the node's own hash-chain preimage there,
	// and carried here as a plain descriptive field for the same reason.
	RealizedOutcome string `json:"realized_outcome,omitempty"`

	VerdictDecision string `json:"verdict_decision,omitempty"` // the admission verdict (T6) that fed this call, if any
	CriterionID     string `json:"criterion_id,omitempty"`

	MessageHash string `json:"message_hash,omitempty"` // hash of the raw request; raw text never ships
	RequestID   string `json:"request_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`

	PrevHash string `json:"prev_hash,omitempty"` // node chain link
	RowHash  string `json:"row_hash"`            // node chain head; server dedup key

	// Tenant / User are content-bearing and ship ONLY under
	// ShareOptions.shipsRawContent() (FullContent || AdminManaged) — stripped
	// to "" (stored NULL server-side) otherwise, mirroring
	// ObsAdmissionRow.Tenant/EndUser.
	Tenant string `json:"tenant,omitempty"`
	User   string `json:"user,omitempty"`
}

// ObsEgressBatch is the T8 push unit: the window's egress routing
// decisions plus the cursor driving the next window. Unlike
// ObsAdmissionBatch there is no companion Policies list — the node carries
// no separate egress policy-snapshot table (only a bare PolicyHash column
// on each decision), so there is nothing else to batch.
type ObsEgressBatch struct {
	Events []ObsEgressRow `json:"events"`
	Cursor ObsCursor      `json:"cursor"`
}
