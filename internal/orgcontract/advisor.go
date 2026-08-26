package orgcontract

// AdvisorSuggestionRow is the W3.2 Suggestions/Advisor wire row: one row per
// active suggestion in the node's advisor digest (see
// docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W3.2"), mirroring
// the node dashboard's Suggestions page (web/src/pages/Suggestions.tsx) and
// the underlying internal/intelligence/advisor engine
// (Suggestion / Report / advisor_digest, internal/db/migrations/039_advisor_state.sql).
//
// This is per-DEVELOPER, not per-session: there is no SessionID field —
// identity rides OrgID/UserEmail stamping like every other wire row (see
// ingest.go forcePusherOrgID / forcePusherEmail), and the server table keys
// on (org_id, user_email, suggestion_key).
//
// Unlike SessionVerbosityRow (byte counts only), the advisor engine's own
// node-local Evidence doc comment says "never raw content" — but that is a
// node-local UI/detector design choice, not a privacy boundary: detectors do
// embed truncated command text (e.g. C4 "failed N times", detect_phase2.go)
// and Evidence.Items[].Label routinely carries session ids and project paths
// (ScopeID e.g. an absolute path for project-scoped suggestions). Per the
// org-parity plan §0 enterprise posture, this row ships every advisor field
// VERBATIM/RAW — paths and command summaries included — gated the same as
// every other enterprise-raw wire (share.shipsRawContent(), composed in
// store/orgpush.go, not here). There is no partial/redacted tier: it either
// ships whole or not at all.
type AdvisorSuggestionRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same stamping
	// rule as every other wire row — see ingest.go forcePusherOrgID /
	// forcePusherEmail).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`

	// SuggestionKey is the node's own advisor.Suggestion.DedupKey — the
	// stable identity dismissals/snoozes/cooldowns attach to on the node.
	// Natural key on the server is (org_id, user_email, suggestion_key),
	// upserted on every push (idempotent, snapshot-recompute — the digest
	// is regenerated and re-pushed whole each time, like every other
	// windowed-recompute aggregate in this arc).
	SuggestionKey string `json:"suggestion_key"`

	// Detector is the internal detector key that produced this suggestion
	// (advisor.Detector.Key) — e.g. "context_balloon", "trivial_routing".
	Detector string `json:"detector"`
	// Category is one of advisor.Category{Cost,Latency,Quality,Hygiene}.
	Category string `json:"category"`
	// Scope is one of advisor.Scope{Session,Project,Global}; ScopeID is the
	// RAW scope identifier for Session/Project scope — a session id or an
	// absolute project path. Carried verbatim per the enterprise-raw
	// posture above.
	Scope   string `json:"scope"`
	ScopeID string `json:"scope_id,omitempty"`

	// Severity is one of advisor.Severity{Info,Advice,Warning}.
	Severity string `json:"severity"`
	// Title / Nudge are the human-facing suggestion text exactly as the
	// node's Suggestions page renders it. Title may embed a truncated
	// command or file reference (e.g. the C4 "never-passed command"
	// detector) — carried verbatim, not redacted.
	Title string `json:"title"`
	Nudge string `json:"nudge"`

	// SavingsUSD / SavingsMin / Confidence are the node's own computed
	// estimates, carried exactly as produced — never recomputed or
	// invented on the wire.
	SavingsUSD float64 `json:"savings_usd,omitempty"`
	SavingsMin float64 `json:"savings_min,omitempty"`
	Confidence float64 `json:"confidence"`

	// EvidenceJSON is the JSON-encoded advisor.Evidence{Numbers, Items,
	// Math} backing this suggestion — counts, arithmetic, and (per the
	// enterprise-raw posture) whatever labels/paths/command summaries the
	// detector attached to Items[].Label. JSON-encoded, not exploded into
	// columns, because Evidence's shape varies per detector.
	EvidenceJSON string `json:"evidence_json"`

	// ActionKind / ActionTarget / ActionLabel flatten advisor.Action (a
	// one-click remediation pointer — never a write itself, see
	// advisor.Action's doc comment) when the suggestion carries one; all
	// three are empty when Action is nil.
	ActionKind   string `json:"action_kind,omitempty"`
	ActionTarget string `json:"action_target,omitempty"`
	ActionLabel  string `json:"action_label,omitempty"`

	// Status is the node's advisor_state.status for this DedupKey when one
	// exists (advisor.StatusDismissed / StatusSnoozed / StatusActed).
	// Empty for the common case: advisor.Run() already excludes muted
	// (dismissed / snoozed-within-cooldown) suggestions before saving the
	// digest, so what ships here is already "active"; Status is carried
	// honestly rather than omitted so a suggestion caught mid-transition
	// (e.g. just marked Acted, digest not yet regenerated) still reports
	// its real state.
	Status string `json:"status,omitempty"`

	// ComputedAt / WindowDays are advisor.Suggestion's own per-suggestion
	// stamp (advisor.stamp); GeneratedAt is the digest-level
	// advisor_digest.generated_at snapshot timestamp shared by every
	// suggestion in the same push.
	ComputedAt  string `json:"computed_at"`
	WindowDays  int    `json:"window_days"`
	GeneratedAt string `json:"generated_at"`
}
