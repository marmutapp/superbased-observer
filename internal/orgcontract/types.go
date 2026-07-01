package orgcontract

// EnrollRequest is the body of POST /api/agent/enroll. The agent presents a
// one-time enrolment token (minted by an admin) and a freshly generated
// Ed25519 public key the server binds to the user record.
//
// The token is a compound "<token_id>.<secret>": the admin mints it once and
// hands the whole string to the developer, who runs `observer enroll <org-url>
// <token>`. The server resolves the user from the token_id and verifies the
// secret, so the developer never needs to know their SCIM user id — that is
// why this request carries no user_id field.
type EnrollRequest struct {
	OneTimeToken   string `json:"one_time_token"`   // "<token_id>.<secret>"
	AgentPublicKey string `json:"agent_public_key"` // base64url-encoded Ed25519 public key
}

// EnrollResponse is the 200 body of POST /api/agent/enroll. The bearer is
// a signed JSON envelope (see BearerClaims) the agent stores in its OS
// keychain and presents on every push.
//
// UserID is the server-resolved SCIM user id the compound enrolment token
// bound to. The agent persists it in org_enrolment.user_id (a NOT NULL
// column) — the request carries no user_id, so the server must echo it back
// here for the agent to record its own identity without re-deriving it from
// the bearer claims.
type EnrollResponse struct {
	Bearer          string `json:"bearer"`
	BearerExpiresAt string `json:"bearer_expires_at"` // RFC3339
	OrgID           string `json:"org_id"`
	OrgName         string `json:"org_name"`
	UserID          string `json:"user_id"` // SCIM user id resolved from the token
	UserEmail       string `json:"user_email"`

	// OrgPolicyPublicKey is the base64url Ed25519 public half of the org's
	// POLICY signing key (guard spec §14.2), delivered at enrol time so the
	// agent can pin it (sha256 of the raw key bytes, stored in
	// guard_policy_state) before the first policy-bundle fetch. omitempty on
	// both sides of the compat invariant: a pre-G13 server omits the field
	// and the agent falls back to trust-on-first-fetch pinning; a pre-G13
	// agent ignores the unknown key. Servers without a configured policy
	// signing key also omit it — the field is never required.
	OrgPolicyPublicKey string `json:"org_policy_public_key,omitempty"`
}

// BearerClaims is the JSON envelope signed with the server's Ed25519 key.
// It is JWT-shaped but is not a JWT: there is no algorithm negotiation and
// no JWS library — one key type, one algorithm, decoded by hand. exp/iat
// are Unix seconds.
type BearerClaims struct {
	Iss string `json:"iss"` // issuer: the org server external URL
	Sub string `json:"sub"` // subject: the SCIM user id
	Aud string `json:"aud"` // audience: the org id
	Exp int64  `json:"exp"` // expiry, Unix seconds
	Iat int64  `json:"iat"` // issued-at, Unix seconds
	Jti string `json:"jti"` // unique token id (for the revocation list)
}

// PushEnvelope is the body of POST /api/agent/push (gzip-compressed on the
// wire). cursor_from/cursor_to bound the batch against the agent's local
// ingest cursor so the server can ACK a next_cursor and the agent can
// resume exactly once.
type PushEnvelope struct {
	AgentVersion string          `json:"agent_version"`
	CursorFrom   int64           `json:"cursor_from"`
	CursorTo     int64           `json:"cursor_to"`
	Sessions     []SessionRow    `json:"sessions"`
	Actions      []ActionRow     `json:"actions"`
	APITurns     []APITurnRow    `json:"api_turns"`
	TokenUsage   []TokenUsageRow `json:"token_usage"`
	// RoutingSummaries is the OPTIONAL §R19.4 aggregate (counts +
	// dollars by tier/reason only) — present only when the node
	// operator opted in via [org_client.share] routing_summary.
	// Optional both directions: v1.8.x servers ignore the key,
	// future servers tolerate its absence.
	RoutingSummaries []RoutingSummaryRow `json:"routing_summaries,omitempty"`

	// GuardEvents are guard-layer verdict rows (v1.8.3+, guard spec
	// §14.3). omitempty keeps pre-guard envelopes byte-identical and
	// lets pre-guard servers ignore the key entirely (the standard
	// additive-compat posture: no required new fields in either
	// direction). Server-side ingest + rollups land with the G14
	// teams arc; until then an old server ACKs the batch and the
	// guard rows are simply not retained centrally.
	GuardEvents []GuardEventRow `json:"guard_events,omitempty"`

	// OTelContent are native-OTel content bodies (native-console Phase 2b,
	// v1.8.x+). Same additive-compat posture as GuardEvents: omitempty so
	// pre-feature envelopes stay byte-identical and an older server ignores
	// the key (ACKs the batch; content simply isn't retained centrally).
	// Present only when the node shares full content (full_content /
	// admin_managed) for the raw body — the hash ships regardless.
	OTelContent []OTelContentRow `json:"otel_content,omitempty"`

	// Org-tier observability (internal/obs) — the tiered, independently
	// node-opt-in disclosure ladder (docs/plans/obs-org-tier-plan-2026-06-29.md
	// §1). Each slice is present only under its own [org_client.share] flag
	// (default false, never server-forced) and is composed via the obs
	// provider seam in orgpush.go (which names no obs_* table — the privacy
	// sentinel stays green, like routing_summaries). Same additive-compat
	// posture as the other optional slices: pre-feature servers ignore the
	// keys, future servers tolerate their absence.
	//
	// T1 — aggregate rollup (counts + token/cost/latency sums; content-free).
	ObsSummaries []ObsSummaryRow `json:"obs_summaries,omitempty"`
	// T2 — trace + span STRUCTURE (topology/kind/name/model/tokens/cost/
	// latency/status/request_id; hashes only, never bodies).
	ObsTraces     []ObsTraceRow     `json:"obs_traces,omitempty"`
	ObsSpans      []ObsSpanRow      `json:"obs_spans,omitempty"`
	ObsSpanEvents []ObsSpanEventRow `json:"obs_span_events,omitempty"`
	// T3 — raw span CONTENT bodies (prompt/response/tool_io); content present
	// only under shipsRawContent(), content_hash always.
	ObsContent []ObsContentRow `json:"obs_content,omitempty"`
	// T4 — eval run health (summaries + scores; content-free).
	ObsEvalRuns []ObsEvalRow `json:"obs_eval_runs,omitempty"`
}

// PushResponse is the 200 body of POST /api/agent/push.
type PushResponse struct {
	AcceptedRows int64 `json:"accepted_rows"`
	DedupedRows  int64 `json:"deduped_rows"`
	NextCursor   int64 `json:"next_cursor"`
}

// PolicyBundle is the 200 body of GET /api/v1/policy-bundle (guard spec
// §14.2): one signed, versioned org guard-policy TOML rule set. The agent
// also persists the verified envelope verbatim as its local bundle cache
// (~/.observer/org-policy-bundle.json) so the guard's org layer loads with
// no network and re-checks the same signature at load time.
//
// Trust model: Signature covers PolicyBundleSigningMessage(Version,
// BundleTOML) under the org policy key. PublicKey rides along in EVERY
// envelope so verification is self-contained, but the key only counts as
// trusted when its PublicKeyPinHash matches the pin the agent recorded at
// enrolment (or on first fetch for pre-G13 enrolments). An envelope whose
// signature or pin check fails is REJECTED — the agent keeps its previous
// bundle and records an R-205 guard event.
//
// The bundle channel DISTRIBUTES policy (server → agent); it never widens
// content sharing (§14.1) — nothing in this type flows back to the server.
type PolicyBundle struct {
	// Version is the server-assigned monotonically increasing bundle
	// version. Agents reject a fetched version lower than the last one
	// they verified (downgrade protection); rolling back is done by
	// publishing the old content as a NEW version.
	Version int64 `json:"version"`
	// BundleTOML is the org guard-policy rule set in exactly the §4.4
	// user/project policy-file format ([[rule]] + [[override]] tables).
	BundleTOML string `json:"bundle_toml"`
	// Signature is base64url(Ed25519 signature) over
	// PolicyBundleSigningMessage(Version, BundleTOML).
	Signature string `json:"signature"`
	// PublicKey is the base64url Ed25519 public half of the signing key.
	PublicKey string `json:"public_key"`
	// SignedAt is the RFC3339 instant the bundle was signed (audit metadata;
	// not part of the signed message — Version is the integrity anchor).
	SignedAt string `json:"signed_at"`
	// Description is the operator's note for the version history.
	Description string `json:"description,omitempty"`
}

// SessionRow is a session as pushed to the server.
//
// Privacy posture (v1.8.0+): a session identifies its project via the
// CONTENT-FREE hashes ProjectRootHash + GitRemoteHash, which are always
// present and let the server map the session to a team via pre-shared
// project-root hash registration without ever seeing the developer's
// filesystem layout.
//
// ProjectRoot + GitRemote are the raw / human-readable values; they ship
// ONLY when the node operator has set [org_client.share].full_content = true
// in their local config (a per-node opt-in; the org admin cannot force it
// on). With the default config they are empty strings (json omitempty).
type SessionRow struct {
	ID string `json:"id"`

	// Content-free hashes — always present.
	ProjectRootHash string `json:"project_root_hash"`
	GitRemoteHash   string `json:"git_remote_hash,omitempty"`

	// Raw values — present only when the node opted in to full-content sharing.
	ProjectRoot string `json:"project_root,omitempty"`
	GitRemote   string `json:"git_remote,omitempty"`

	Tool         string `json:"tool"`
	Model        string `json:"model,omitempty"`
	GitBranch    string `json:"git_branch,omitempty"`
	StartedAt    string `json:"started_at"` // RFC3339
	EndedAt      string `json:"ended_at,omitempty"`
	TotalActions int    `json:"total_actions"`
	OrgID        string `json:"org_id"`
	UserEmail    string `json:"user_email"`
}

// ActionRow is an action as pushed to the server.
//
// Privacy posture (v1.8.0+):
//
//   - The classic four content columns (raw_tool_input, raw_tool_output,
//     preceding_reasoning, error_message) are intentionally absent — they
//     have never shipped.
//   - TargetHash and SourceFileHash are content-free and ALWAYS present.
//     They give the server stable dedup / cardinality signals (how often
//     does the same shell command run? how many distinct JSONL files?)
//     without revealing the underlying bytes.
//   - Target and SourceFile are the raw, human-readable values. The
//     2026-06-02 teams test found that these were shipping raw and
//     contained command bodies (run_command), assistant prose
//     (task_complete), and raw filesystem paths. v1.8.0 ships them ONLY
//     when the node operator has set [org_client.share].full_content = true
//     — a per-node opt-in the org admin cannot force on. With the default
//     config they are empty strings (json omitempty).
type ActionRow struct {
	SessionID     string `json:"session_id"`
	SourceEventID string `json:"source_event_id"`
	Timestamp     string `json:"timestamp"` // RFC3339
	Tool          string `json:"tool"`
	ActionType    string `json:"action_type"`

	// Content-free hashes — always present.
	TargetHash     string `json:"target_hash,omitempty"`
	SourceFileHash string `json:"source_file_hash"`

	// Raw values — present only when the node opted in to full-content sharing.
	Target     string `json:"target,omitempty"`
	SourceFile string `json:"source_file,omitempty"`

	TurnIndex   int    `json:"turn_index"`
	Success     bool   `json:"success"`
	DurationMs  int64  `json:"duration_ms"`
	IsSidechain bool   `json:"is_sidechain"`
	OrgID       string `json:"org_id"`
	UserEmail   string `json:"user_email"`
}

// APITurnRow is a proxy-observed API turn as pushed. Prompt/completion
// bodies are never present; only token counts, cost, timing, hashes
// (not content), and a parsed error class (a category, not a message).
//
// Privacy posture (v1.8.0+): ProjectRootHash is always present;
// ProjectRoot ships only when the node opted in to full-content sharing.
type APITurnRow struct {
	SessionID             string  `json:"session_id"`
	ProjectRootHash       string  `json:"project_root_hash,omitempty"`
	ProjectRoot           string  `json:"project_root,omitempty"`
	Timestamp             string  `json:"timestamp"` // RFC3339
	Provider              string  `json:"provider"`
	Model                 string  `json:"model,omitempty"`
	RequestID             string  `json:"request_id,omitempty"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheCreation1hTokens int64   `json:"cache_creation_1h_tokens"`
	WebSearchRequests     int64   `json:"web_search_requests"`
	CostUSD               float64 `json:"cost_usd"`
	MessageCount          int     `json:"message_count"`
	ToolUseCount          int     `json:"tool_use_count"`
	SystemPromptHash      string  `json:"system_prompt_hash,omitempty"`
	MessagePrefixHash     string  `json:"message_prefix_hash,omitempty"`
	TimeToFirstTokenMS    int64   `json:"time_to_first_token_ms"`
	TotalResponseMS       int64   `json:"total_response_ms"`
	StopReason            string  `json:"stop_reason,omitempty"`
	HTTPStatus            int     `json:"http_status"`
	ErrorClass            string  `json:"error_class,omitempty"`
	OrgID                 string  `json:"org_id"`
	UserEmail             string  `json:"user_email"`
}

// GuardEventRow is a guard-layer audit event as pushed to the server
// (guard spec §14.3 central reporting).
//
// Privacy posture (guard spec §10.2 — mirrors ActionRow):
//
//   - rule_id, category, severity, decision, degraded_from, enforced,
//     source, tool, event_kind, timestamps and TargetHash are
//     content-free and always ship.
//   - Reason, TargetExcerpt and TaintOrigin are content-bearing
//     (verdict prose, a bounded excerpt of the command/path, a taint
//     source description). They ship ONLY when the node operator has
//     set [org_client.share].full_content = true — the same per-node
//     opt-in gating actions.target; the org admin cannot force it on.
//     With the default config they are empty strings (json omitempty).
//   - ChainPrev/ChainHash are SHA-256 hex links of the node's
//     tamper-evidence chain (guard spec §10.4) — content-free, shipped
//     so server-side rollups can detect broken/truncated chains.
//
// Local row-id anchors (action_id / api_turn_id) are deliberately
// absent: they are meaningless outside the originating node.
type GuardEventRow struct {
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"timestamp"` // RFC3339
	Tool      string `json:"tool,omitempty"`
	EventKind string `json:"event_kind,omitempty"`
	RuleID    string `json:"rule_id"`
	Category  string `json:"category,omitempty"`
	Severity  string `json:"severity,omitempty"`
	Decision  string `json:"decision,omitempty"`
	// DegradedFrom is the pre-degradation decision when a capability
	// downgrade applied (guard spec §6.2); empty otherwise.
	DegradedFrom string `json:"degraded_from,omitempty"`
	Enforced     bool   `json:"enforced"`
	Source       string `json:"source,omitempty"`

	// Content-free hash — always present when the event had a target.
	TargetHash string `json:"target_hash,omitempty"`

	// Content-bearing values — present only when the node opted in to
	// full-content sharing.
	Reason        string `json:"reason,omitempty"`
	TargetExcerpt string `json:"target_excerpt,omitempty"`
	TaintOrigin   string `json:"taint_origin,omitempty"`

	// Tamper-evidence chain links (content-free SHA-256 hex).
	ChainPrev string `json:"chain_prev,omitempty"`
	ChainHash string `json:"chain_hash,omitempty"`

	OrgID     string `json:"org_id"`
	UserEmail string `json:"user_email"`
}

// TokenUsageRow is an adapter-derived token-usage row as pushed. All
// fields are counts/metadata — no content.
//
// Privacy posture (v1.8.0+): ProjectRootHash + SourceFileHash are always
// present; ProjectRoot + SourceFile ship only when the node opted in to
// full-content sharing.
type TokenUsageRow struct {
	SessionID             string  `json:"session_id"`
	ProjectRootHash       string  `json:"project_root_hash,omitempty"`
	ProjectRoot           string  `json:"project_root,omitempty"`
	Timestamp             string  `json:"timestamp"` // RFC3339
	Tool                  string  `json:"tool"`
	Model                 string  `json:"model,omitempty"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheCreation1hTokens int64   `json:"cache_creation_1h_tokens"`
	ReasoningTokens       int64   `json:"reasoning_tokens"`
	WebSearchRequests     int64   `json:"web_search_requests"`
	EstimatedCostUSD      float64 `json:"estimated_cost_usd"`
	Source                string  `json:"source"`
	Reliability           string  `json:"reliability"`
	SourceFileHash        string  `json:"source_file_hash"`
	SourceFile            string  `json:"source_file,omitempty"`
	SourceEventID         string  `json:"source_event_id"`
	OrgID                 string  `json:"org_id"`
	UserEmail             string  `json:"user_email"`
}

// OTelContentRow is one captured native-OTel content body on the wire
// (native-console integration, Phase 2b body-ingest Layer B). ContentHash
// always ships; Content (the raw, scrubbed body) ships only when the node
// shares full content (full_content or admin_managed) — gated in
// SelectUnpushedSince exactly like the other content-bearing columns.
type OTelContentRow struct {
	RequestID   string `json:"request_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	ToolUseID   string `json:"tool_use_id,omitempty"`
	Kind        string `json:"kind"`
	ContentHash string `json:"content_hash"`
	Content     string `json:"content,omitempty"`
	Timestamp   string `json:"timestamp"` // RFC3339
	OrgID       string `json:"org_id"`
	UserEmail   string `json:"user_email"`
}

// RoutingSummaryRow is the §R19.4 org rollup wire shape: AGGREGATE
// ONLY — counts and dollars by (day, tier, reason, mode). No model
// ids, no session detail, no per-decision rows: router_decisions and
// model_calibration stay node-local (privacy-sentinel-pinned); this
// aggregate is the only routing data that ever crosses the wire, and
// only when the node operator opts in via
// [org_client.share] routing_summary = true.
type RoutingSummaryRow struct {
	// OrgID / UserEmail are the agent-stamped attribution (same
	// stamping rule as every other wire row).
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	// Day is the UTC date (YYYY-MM-DD).
	Day string `json:"day"`
	// Tier is the ORIGINAL model's tier class (an enum, not a model id).
	Tier string `json:"tier"`
	// Reason is the decision's primary closed-enum reason code.
	Reason string `json:"reason"`
	// Mode is advise | enforce.
	Mode string `json:"mode"`
	// Decisions / Applied are row counts.
	Decisions int64 `json:"decisions"`
	Applied   int64 `json:"applied"`
	// EstSavingsUSD / CacheForfeitUSD are decision-time estimate sums.
	EstSavingsUSD   float64 `json:"est_savings_usd"`
	CacheForfeitUSD float64 `json:"cache_forfeit_usd"`
}

// --- Org-tier observability wire shapes (obs-org-tier plan §3) -------------
//
// All four tiers are AGGREGATE-or-STRUCTURE-or-GATED-CONTENT; none carries a
// raw body except ObsContentRow (gated by shipsRawContent()). They are
// composed in orgpush.go via the obs provider seam, so the privacy sentinel
// never sees an obs_* table name there. OrgID/UserEmail are agent-stamped like
// every other wire row.

// ObsSummaryRow is the T1 AGGREGATE rollup: per (day, model, provider,
// project_hash, source) counts + token/cost/latency sums. CONTENT-FREE — no
// trace ids, no span topology, no names, no bodies. Pinned aggregate-only by a
// reflect test. project_hash is the content-free key (sha256 of project_root),
// the same posture sessions/api_turns already ship; the raw path never enters
// this row.
type ObsSummaryRow struct {
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	Day       string `json:"day"` // UTC YYYY-MM-DD

	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
	ProjectHash string `json:"project_hash,omitempty"`
	Source      string `json:"source,omitempty"` // provenance tag (otlp_trace/sdk_otlp/…)

	Traces           int64   `json:"traces"`
	Spans            int64   `json:"spans"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	ErrorTraces      int64   `json:"error_traces"`
	DurationMsSum    int64   `json:"duration_ms_sum"` // sum+count → server derives mean
	DurationMsCount  int64   `json:"duration_ms_count"`
}

// ObsTraceRow is the T2 trace skeleton (STRUCTURE, hashes only). ProjectRoot
// ships only under shipsRawContent(); ProjectHash always.
type ObsTraceRow struct {
	OrgID       string  `json:"org_id,omitempty"`
	UserEmail   string  `json:"user_email,omitempty"`
	TraceID     string  `json:"trace_id"`
	SessionID   string  `json:"session_id,omitempty"`
	ThreadID    string  `json:"thread_id,omitempty"`
	Source      string  `json:"source,omitempty"`
	Status      string  `json:"status,omitempty"`
	StartedAt   string  `json:"started_at,omitempty"`
	EndedAt     string  `json:"ended_at,omitempty"`
	ProjectHash string  `json:"project_hash,omitempty"`
	ProjectRoot string  `json:"project_root,omitempty"` // gated by shipsRawContent()
	RootSpanID  string  `json:"root_span_id,omitempty"`
	SpanCount   int64   `json:"span_count"`
	TotalTokens int64   `json:"total_tokens"`
	CostUSD     float64 `json:"cost_usd"`
}

// ObsSpanRow is the T2 span skeleton. RequestID is the CONTENT-FREE soft join
// key the server uses for the proxy-exact wedge (obs_spans × api_turns). Name
// is an operation label (chat/get_weather/…), not a body (§8 decision 2).
type ObsSpanRow struct {
	OrgID            string  `json:"org_id,omitempty"`
	UserEmail        string  `json:"user_email,omitempty"`
	TraceID          string  `json:"trace_id"`
	SpanID           string  `json:"span_id"`
	ParentSpanID     string  `json:"parent_span_id,omitempty"`
	Kind             string  `json:"kind,omitempty"`
	Name             string  `json:"name,omitempty"`
	StartedAt        string  `json:"started_at,omitempty"`
	EndedAt          string  `json:"ended_at,omitempty"`
	DurationMs       int64   `json:"duration_ms"`
	Status           string  `json:"status,omitempty"`
	Model            string  `json:"model,omitempty"`
	Provider         string  `json:"provider,omitempty"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CostSource       string  `json:"cost_source,omitempty"`
	RequestID        string  `json:"request_id,omitempty"`
	ToolCallID       string  `json:"tool_call_id,omitempty"`
}

// ObsSpanEventRow is a T2 span-event (metadata only — name + time, no
// attribute bodies).
type ObsSpanEventRow struct {
	OrgID     string `json:"org_id,omitempty"`
	UserEmail string `json:"user_email,omitempty"`
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id"`
	Time      string `json:"time,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ObsContentRow is the T3 raw span body, mirroring OTelContentRow: Content
// present only under shipsRawContent(), ContentHash always.
type ObsContentRow struct {
	OrgID       string `json:"org_id,omitempty"`
	UserEmail   string `json:"user_email,omitempty"`
	TraceID     string `json:"trace_id,omitempty"`
	SpanID      string `json:"span_id"`
	Kind        string `json:"kind"` // prompt/response/tool_io
	ContentHash string `json:"content_hash"`
	Content     string `json:"content,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// ObsEvalRow is the T4 eval-run health summary (content-free).
type ObsEvalRow struct {
	OrgID       string  `json:"org_id,omitempty"`
	UserEmail   string  `json:"user_email,omitempty"`
	Day         string  `json:"day"`
	DatasetName string  `json:"dataset_name,omitempty"`
	RunName     string  `json:"run_name,omitempty"`
	ScorerName  string  `json:"scorer_name,omitempty"`
	Total       int64   `json:"total"`
	Passed      int64   `json:"passed"`
	MeanScore   float64 `json:"mean_score"`
	MinScore    float64 `json:"min_score"`
	Source      string  `json:"source,omitempty"` // offline/online
}

// ObsCursor bounds an incremental obs structure/content push (T2/T3). v1 uses
// windowed-recompute (the server upserts by composite key), so the cursor is a
// simple since-day; the obs-owned high-water cursor is a documented follow-up.
type ObsCursor struct {
	SinceDay string `json:"since_day,omitempty"`
}

// ObsSpanBatch is the T2 structure push payload (traces + spans + events +
// cursor) returned by the obs provider in one shot.
type ObsSpanBatch struct {
	Traces []ObsTraceRow     `json:"traces"`
	Spans  []ObsSpanRow      `json:"spans"`
	Events []ObsSpanEventRow `json:"events"`
	Cursor ObsCursor         `json:"cursor"`
}

// RoutingPolicyDoc is the §R19.1 org-distributed policy document. The
// body is a TOML fragment using the [routing] vocabulary; the agent
// composes it with hard-constraints-first semantics
// (routingconfig.ComposeOrgPolicy) and STRUCTURALLY ignores any
// enabled/mode keys — enforcement is node-side opt-in by design
// (§R23: no remote enforce toggle exists).
type RoutingPolicyDoc struct {
	Version int64 `json:"version"`
	// Body is the TOML policy fragment.
	Body string `json:"body"`
	// BodyHash is hex(SHA-256(body)).
	BodyHash string `json:"body_hash"`
	// Signature is base64(Ed25519 signature over body bytes) made with
	// the org server's policy signing key.
	Signature string `json:"signature"`
	// PublicKey is base64(Ed25519 public key) — TOFU-pinned by the
	// agent on first receipt (enrolment-channel trust, §R19.1).
	PublicKey string `json:"public_key"`
}
