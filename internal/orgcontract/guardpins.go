package orgcontract

// guardpins.go carries the W5.2 wire rows for guard MCP pins and exception
// approvals (docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W5.2")
// — the per-developer visibility surface so an admin can audit what a dev's
// node has pinned (MCP servers / hook configs / native dialects it trusts)
// and what scoped exceptions have been granted against the guard policy.
//
// Guard POLICY AUTHORING stays entirely on the org policy rail (§0.3,
// internal/routingconfig-style signed distribution) — these rows are
// READ-ONLY visibility into node state, never a channel for the org to push
// pin/approval changes back down.
//
// Field shapes mirror the node's own guard_pins / guard_approvals tables
// (internal/db/migrations/040_guard_layer.sql, internal/store/guard.go)
// exactly — no columns are invented. guard_policy_state (the third
// G13/G14-deferred table) is deliberately NOT wired here: it is a
// policy-source load log, not a per-dev pin/approval; see
// internal/store/guardpinsorgrows.go's doc comment for the reasoning.

// GuardPinRow is one row of the node's guard_pins table — a pinned MCP
// server / hook config / native-dialect identity the node has trusted-on-
// first-use and continues to verify. Pins are CURRENT STATE (one row per
// (kind, name, client) on the node, upserted in place on every re-sighting),
// not an event log — so this wire row is a snapshot entry, re-shipped and
// upserted by natural key on every push.
type GuardPinRow struct {
	OrgID     string `json:"org_id"`
	UserEmail string `json:"user_email"`

	// PinKey is the server's natural per-dev key, derived from the node's
	// own UNIQUE(kind, name, client) triple — see
	// internal/store/guardpinsorgrows.go::guardPinKey.
	PinKey string `json:"pin_key"`

	Kind   string `json:"kind"` // 'mcp_server' | 'hook_config' | 'native_dialect'
	Name   string `json:"name"`
	Client string `json:"client,omitempty"` // '' when client-agnostic

	// PinHash is the SHA-256 hex over the pinned identity material — the
	// node never stores the raw identity payload either, so this is the
	// full-fidelity form that exists to ship, admin_managed or not.
	PinHash string `json:"pin_hash"`

	FirstSeen    string `json:"first_seen"`    // RFC3339, node first-sighting time
	LastVerified string `json:"last_verified"` // RFC3339, this push's observation time

	Status string `json:"status"` // 'pinned' | 'drifted' | 'approved'
}

// GuardApprovalRow is one row of the node's guard_approvals table — an
// operator-granted, scoped exception against a guard rule. Approvals are
// also current-state on the node (an approval simply stops existing once it
// expires or is revoked, and gets pruned by store.PruneGuardTables), so this
// wire row ships the node's currently-active register on every push; see
// internal/store/guardpinsorgrows.go for the staleness-handling note.
//
// The node's guard_approvals table has no "category" column (only
// guard_events does) and NEVER stores a raw project-root path — only its
// SHA-256 (internal/guard/approvals.go::HashProjectRoot) — so RuleID is the
// sole rule/category dimension and ProjectRootHash is the sole "target" for
// project-scoped grants; there is no raw path to ship even under
// admin_managed, because none exists node-side. SessionID IS carried raw
// (not hashed) because the node's own table stores it raw for
// 'once'/'session'-scoped grants.
type GuardApprovalRow struct {
	OrgID     string `json:"org_id"`
	UserEmail string `json:"user_email"`

	// ApprovalKey is the server's natural per-dev key: the node's own local
	// guard_approvals.id, stable for the row's lifetime and disambiguated
	// from other nodes/devs by the (org_id, user_email) it is scoped under.
	ApprovalKey string `json:"approval_key"`

	RuleID string `json:"rule_id"`
	Scope  string `json:"scope"` // 'once' | 'session' | 'project' | 'global'

	SessionID       string `json:"session_id,omitempty"`        // set for scope='once'/'session'
	ProjectRootHash string `json:"project_root_hash,omitempty"` // set for scope='project'; sha256 hex, never raw

	GrantedBy string `json:"granted_by,omitempty"` // approver identity (local operator / org user)
	GrantedAt string `json:"granted_at"`           // RFC3339, node's approval ts
	ExpiresAt string `json:"expires_at,omitempty"` // RFC3339; '' = does not expire

	// Active reflects the node's own read of "still valid as of this push"
	// (expires_at empty or in the future). Because the wire only ever
	// carries the node's currently-active register (see
	// SelectGuardApprovalRows), this is always true on arrival; it is kept
	// as an explicit field — rather than inferred purely from ExpiresAt on
	// the read side — so the org rollup's expiring-soon cut and any future
	// producer that ships a broader (including-expired) register have a
	// single unambiguous flag to trust.
	Active bool `json:"active"`
}
