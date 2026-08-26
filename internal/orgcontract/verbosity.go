package orgcontract

// VerbosityLanguageBytes is one entry of a SessionVerbosityRow's
// CodeByLanguageJSON array: a canonical language name, its authored byte
// count, and the verbosity.Category it falls under. Shared between the
// store-side encoder (internal/store/verbosityorgrows.go) and the org
// rollup-side decoder (internal/orgserver/rollup/verbositysession.go) so both
// agree on the JSON shape without either importing the other.
type VerbosityLanguageBytes struct {
	Language string `json:"language"`
	Bytes    int64  `json:"bytes"`
	Category string `json:"category"`
}

// SessionVerbosityRow is the W3.1 output-composition (verbosity) wire row: one
// row per session, mirroring the node dashboard's VerbosityCard / the MCP
// get_output_composition tool (internal/mcp/tools_output_composition.go) and
// the node read-side internal/store/verbosity.go::LoadSessionVerbosity +
// AuthoredCaptureStats. It carries BYTE COUNTS + a small language→bytes enum
// map ONLY — no assistant text, no authored code, no file path or content of
// any kind ever crosses. This is the enterprise-first "admin_managed ships by
// default" class (see docs/plans/org-parity-full-depth-plan-2026-08-24.md §0),
// not a new opt-in tier: it ships whenever the node's ShareOptions already
// ships raw content (shipsRawContent()), same gate as the message-content
// viewer's underlying data (though this row itself has no content to gate).
//
// Category/byte semantics mirror internal/verbosity.Breakdown exactly:
//   - Visible bytes (assistant-text segmentation) split into Narrative /
//     Artifact / ArtifactUntagged / Written / Command channels
//     (verbosity.VisibleBreakdown).
//   - CodeBytes / ExplainBytes are Breakdown.CodeBytes() / ExplainBytes() —
//     the code:explain ratio inputs the node's VerbosityCard chart plots.
//   - ByCategory (Prose/Code/Docs/Config/Data/Unknown, from
//     verbosity.Category) is carried as five discrete byte fields rather than
//     a map so the server schema stays a flat table.
//   - CodeByLanguageJSON is a small JSON-encoded array of
//     {language, bytes, category} — the same shape as the MCP tool's
//     code_by_language — for the language byte split. It is JSON-encoded
//     (not exploded into columns) because the language set is open-ended;
//     the values are canonical language names + byte counts, never content.
//   - AuthoredCaptured mirrors internal/store::AuthoredCaptureStats — true
//     when actions.content_bytes was captured for every authored-write/
//     command action in the session (false means the byte totals are a
//     partial/undercount, same honesty flag the MCP tool and VerbosityCard
//     surface today).
type SessionVerbosityRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same stamping
	// rule as every other wire row — see ingest.go forcePusherOrgID /
	// forcePusherEmail).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	// SessionID identifies the session this summary is for. Natural key on
	// the server is (org_id, session_id) — one row per session, upserted on
	// every recompute (idempotent, like the Arc-4 day-bucketed summaries).
	SessionID string `json:"session_id"`

	// TotalBytes is the sum of all visible + written + command bytes
	// attributed to this session (Narrative + Artifact + ArtifactUntagged +
	// Written + Command).
	TotalBytes int64 `json:"total_bytes"`
	// CodeBytes / ExplainBytes are Breakdown.CodeBytes() / ExplainBytes() —
	// the numerator/denominator pair behind the VerbosityCard's
	// code:explain ratio. ExplainBytes is prose/narrative bytes; CodeBytes
	// is everything categorized Category.Code.
	CodeBytes    int64 `json:"code_bytes"`
	ExplainBytes int64 `json:"explain_bytes"`

	// ProseBytes / DocsBytes / ConfigBytes / DataBytes / UnknownBytes are
	// Breakdown.ByCategory(), split into discrete fields (CodeBytes above
	// doubles as the Category.Code bucket — the six verbosity.Category
	// values map onto these six fields).
	ProseBytes   int64 `json:"prose_bytes"`
	DocsBytes    int64 `json:"docs_bytes"`
	ConfigBytes  int64 `json:"config_bytes"`
	DataBytes    int64 `json:"data_bytes"`
	UnknownBytes int64 `json:"unknown_bytes"`

	// NarrativeBytes / ArtifactBytes / ArtifactUntaggedBytes / WrittenBytes /
	// CommandBytes are the five verbosity.VisibleBreakdown + Written/Command
	// channels — the same "channels" breakdown the MCP tool reports.
	NarrativeBytes        int64 `json:"narrative_bytes"`
	ArtifactBytes         int64 `json:"artifact_bytes"`
	ArtifactUntaggedBytes int64 `json:"artifact_untagged_bytes"`
	WrittenBytes          int64 `json:"written_bytes"`
	CommandBytes          int64 `json:"command_bytes"`

	// CodeByLanguageJSON is a JSON-encoded array of
	// {"language":"go","bytes":1234,"category":"code"} objects — the
	// language byte split, canonical language names + byte counts only.
	CodeByLanguageJSON string `json:"code_by_language_json"`

	// AuthoredCaptured is false when actions.content_bytes wasn't captured
	// for one or more authored actions in the session (older rows, or a
	// capture gap) — the same honesty flag as the node/MCP surfaces, so the
	// org panel can show "partial" rather than silently under-reporting.
	AuthoredCaptured bool `json:"authored_captured"`
}
