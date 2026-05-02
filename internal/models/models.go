package models

import "time"

// Tool identifiers. These are the stable string values stored in the `tool`
// column of sessions, actions, and token_usage. Adapters must return one of
// these from Adapter.Name().
const (
	ToolClaudeCode = "claude-code"
	ToolCodex      = "codex"
	ToolCursor     = "cursor"
	ToolCline      = "cline"
	ToolRooCode    = "roo-code"
	ToolCopilot    = "copilot"
	ToolOpenCode   = "opencode"
	ToolOpenClaw   = "openclaw"
	ToolPi         = "pi"
)

// Normalized action types. See spec §5. Adapters map their tool-specific
// action names onto this set; if no mapping fits, use ActionUnknown and keep
// the raw name in RawToolName.
const (
	ActionReadFile      = "read_file"
	ActionWriteFile     = "write_file"
	ActionEditFile      = "edit_file"
	ActionRunCommand    = "run_command"
	ActionSearchText    = "search_text"
	ActionSearchFiles   = "search_files"
	ActionWebSearch     = "web_search"
	ActionWebFetch      = "web_fetch"
	ActionBrowserAction = "browser_action"
	ActionMCPCall       = "mcp_call"
	// ActionSpawnSubagent is a sub-agent invocation. In Claude Code this
	// is the `Agent` tool — the parent thread emits a tool_use that
	// launches a sub-agent runtime; the sub-agent's activity is logged
	// inline in the SAME session JSONL with `isSidechain: true` per
	// line. Distinguishing this action type lets the dashboard count
	// "agent fan-out" separately from regular tool work.
	ActionSpawnSubagent = "spawn_subagent"
	// ActionTodoUpdate is a structured-todo-list management call. In
	// Claude Code this is TaskCreate / TaskUpdate / TaskList / TaskGet
	// / TaskOutput / TaskStop — administrative tools the agent uses to
	// track its own work plan. Distinct from spawn_subagent (Agent) and
	// from task_complete (legacy).
	ActionTodoUpdate   = "todo_update"
	ActionTaskComplete = "task_complete"
	ActionAskUser      = "ask_user"
	ActionUserPrompt   = "user_prompt"
	// ActionTurnAborted is a turn that was interrupted before completion
	// (user pressed esc, cancelled the agent, etc.). Distinct from
	// task_complete with success=false: aborted turns never finished
	// generating, so the model output is partial. Codex emits a
	// dedicated event_msg/turn_aborted for this; for analysts the
	// distinction matters for cost analysis (aborted turns still
	// consumed input/output tokens up to the abort point).
	ActionTurnAborted = "turn_aborted"
	// ActionContextCompacted is an upstream-emitted context-window
	// compaction event — the model (or its host) decided to summarize/
	// drop earlier turns to stay within context. Codex emits a top-
	// level `compacted` event whose payload carries the replaced
	// messages; the row records msg-count + byte/token estimate so the
	// dashboard can surface compaction frequency without polluting the
	// file-edit timeline. NOT searchable like ActionEditFile —
	// dashboard filters typically exclude it from action-type browsers.
	ActionContextCompacted = "context_compacted"
	// ActionSystemPrompt is a system-prompt-shaped message captured
	// from a platform that exposes the model's seed instructions: codex
	// session_meta.base_instructions, codex turn_context.
	// developer_instructions, codex response_item.message.role=developer,
	// or openclaw custom/bootstrap-context:full. Symmetric to
	// ActionUserPrompt — both are message-shaped rows where the body
	// IS the value (RawToolInput carries the scrubbed text; Target a
	// short preview; MessageID a content hash for cross-row dedup).
	// Adapters MUST hash-dedup within a session so a single base
	// system prompt repeated across every turn_context only emits
	// one row.
	ActionSystemPrompt = "system_prompt"
	// ActionAPIError captures upstream-API failures (Anthropic /
	// OpenAI / Gemini error responses) that the JSONL adapters or the
	// proxy observe. Surfaces content-policy blocks, rate limits,
	// invalid-request errors, etc. that pre-v1.4.20 were dropped on
	// the floor — the proxy filtered out non-2xx responses and the
	// claudecode adapter skipped the `type: "system"` records where
	// these land. Target carries the upstream `request_id` (joinable
	// to api_turns.request_id when both proxy + JSONL saw it),
	// ErrorMessage carries the human-readable body, RawToolName
	// preserves the upstream error class (`invalid_request_error` /
	// `rate_limit_error` / `overloaded_error` / etc.). Success is
	// always false.
	ActionAPIError = "api_error"
	ActionUnknown  = "unknown"
)

// Freshness classifications for file and command accesses. See spec §7.
const (
	FreshnessFresh             = "fresh"
	FreshnessStale             = "stale"
	FreshnessChangedBySelf     = "changed_by_self"
	FreshnessChangedExternally = "changed_externally"
	FreshnessUnknown           = "unknown"
)

// Token source and reliability tags. See spec §24 for the reliability matrix.
const (
	TokenSourceJSONL     = "jsonl"
	TokenSourceOTel      = "otel"
	TokenSourceHook      = "hook"
	TokenSourceProxy     = "proxy"
	TokenSourceEstimated = "estimated"

	ReliabilityAccurate    = "accurate"
	ReliabilityApproximate = "approximate"
	ReliabilityUnreliable  = "unreliable"
	ReliabilityUnknown     = "unknown"
)

// API providers recognized by the proxy (spec §9).
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
)

// Project is a git-root-scoped grouping of sessions. Non-git directories use
// the working directory as the project root. See spec §20.
type Project struct {
	ID            int64
	RootPath      string
	GitRemote     string
	Name          string
	CreatedAt     time.Time
	LastSessionAt time.Time
}

// Session is a single AI coding tool run. Session IDs are tool-supplied
// where possible and deterministic across re-parses.
type Session struct {
	ID           string
	ProjectID    int64
	Tool         string
	Model        string
	GitBranch    string
	StartedAt    time.Time
	EndedAt      time.Time
	TotalActions int
	Metadata     string // JSON blob for tool-specific extras.
}

// Action is one normalized tool call within a session. The
// (SourceFile, SourceEventID) pair uniquely identifies an action so that
// re-parsing a session file never inserts duplicates.
type Action struct {
	ID                 int64
	SessionID          string
	ProjectID          int64
	Timestamp          time.Time
	TurnIndex          int
	ActionType         string
	IsNativeTool       bool
	Target             string
	TargetHash         string
	Success            bool
	ErrorMessage       string
	DurationMs         int64
	ContentHash        string
	FileMtime          time.Time
	FileSizeBytes      int64
	Freshness          string
	PriorActionID      int64
	ChangeDetected     bool
	PrecedingReasoning string
	RawToolName        string
	RawToolInput       string // Pre-scrubbing, truncated to 2KB downstream.
	Tool               string
	SourceFile         string
	SourceEventID      string
	// IsSidechain marks actions emitted inside a sub-agent runtime
	// (spawned via the parent's `Agent` tool). Sub-agents share their
	// parent's SessionID; this flag is the only structural marker
	// distinguishing parent-thread work from sub-agent work. Used by
	// discover.staleReads to segment cross-thread redundancy and by
	// the Sessions tab to surface sub-agent volume.
	IsSidechain bool
	// MessageID is the upstream Anthropic message id (msg_xxx) that
	// produced this action. Populated by adapters that have access to
	// the parent message (claudecode reads it from each JSONL line's
	// `message.id` field). Empty for action types that don't have a
	// natural parent (user_prompt rows pre-backfill, platforms with
	// no upstream message id).
	MessageID string
}

// ToolEvent is the adapter → storage transport type for a single tool call.
// It carries everything needed to insert an Action plus upsert its Session
// and Project.
type ToolEvent struct {
	SourceFile         string
	SourceEventID      string
	SessionID          string
	ProjectRoot        string
	Timestamp          time.Time
	TurnIndex          int
	GitBranch          string
	Model              string
	Tool               string
	ActionType         string
	Target             string
	Success            bool
	ErrorMessage       string
	DurationMs         int64
	PrecedingReasoning string
	RawToolName        string
	RawToolInput       string
	// ToolOutput is the scrubbed tool_result body (for indexing into
	// action_excerpts). Empty when the adapter didn't see the paired
	// result.
	ToolOutput string
	// IsSidechain marks events emitted inside a sub-agent runtime.
	// See [Action.IsSidechain].
	IsSidechain bool
	// MessageID is the upstream Anthropic message id (msg_xxx) of the
	// API turn that contained this tool call. Populated by adapters
	// that have access to the parent message (claudecode reads it from
	// each JSONL line's `message.id` field). Empty when the adapter
	// can't determine the parent — e.g. user_prompt rows or platforms
	// where the upstream client doesn't surface a message id.
	MessageID string
}

// TokenEvent is the adapter → storage transport type for per-turn token
// usage. The proxy produces accurate values; JSONL adapters produce
// approximate or unreliable ones — hence the Source+Reliability fields.
//
// ProjectRoot and GitBranch are carried so the store layer can upsert the
// owning session even for JSONL lines that have usage data but no tool_use
// block (e.g. subagent compaction turns).
type TokenEvent struct {
	SourceFile          string
	SourceEventID       string
	SessionID           string
	ProjectRoot         string
	GitBranch           string
	Timestamp           time.Time
	Tool                string
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CacheCreation1hTokens is the subset of CacheCreationTokens that
	// landed in Anthropic's 1h ephemeral tier (priced at 2× the 5m
	// default). Zero means all cache_creation tokens are 5m — correct
	// for any provider that doesn't expose the breakdown.
	CacheCreation1hTokens int64
	ReasoningTokens       int64
	EstimatedCostUSD      float64
	Source                string
	Reliability           string
	// MessageID is the upstream Anthropic message id (msg_xxx) of the
	// API turn this token row belongs to. See ToolEvent.MessageID.
	MessageID string
}

// APITurn is one request/response pair observed by the proxy. Accurate token
// counts come from the provider's response body; session/project linkage is
// best-effort (nil session_id when the caller omits the X-Session-Id header).
// See spec §9 and the api_turns schema in §6.2.
type APITurn struct {
	ID                  int64
	SessionID           string
	ProjectID           int64
	Timestamp           time.Time
	Provider            string // anthropic | openai
	Model               string
	RequestID           string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CacheCreation1hTokens is the subset of CacheCreationTokens that
	// landed in Anthropic's 1h ephemeral tier. Zero means the proxy
	// didn't observe a tier breakdown (5m only) or the upstream
	// response didn't expose one.
	CacheCreation1hTokens int64
	CostUSD               float64
	MessageCount          int
	ToolUseCount          int
	SystemPromptHash      string
	// MessagePrefixHash is the SHA-256 of the stable cache-aligned message
	// prefix (spec §10 Layer 3). Empty when conversation compression is
	// disabled or no prefix was observable. See
	// internal/compression/conversation.PrefixHash.
	MessagePrefixHash string
	// CompressionOriginalBytes / CompressionCompressedBytes are the request
	// body size before and after conversation compression ran. Zero when
	// the compressor was disabled or skipped this turn.
	CompressionOriginalBytes   int64
	CompressionCompressedBytes int64
	// CompressionCount is how many tool_result bodies had their content
	// replaced by a per-type compressor.
	CompressionCount int64
	// CompressionDroppedCount is how many source messages were replaced
	// by a marker.
	CompressionDroppedCount int64
	// CompressionMarkerCount is how many marker messages were emitted.
	CompressionMarkerCount int64
	// CompressionEvents is the per-decision detail (one record per
	// compress or drop). Persisted into the compression_events table
	// (migration 009) by store.InsertAPITurn so the dashboard can
	// break down savings by mechanism. Empty when compression skipped.
	CompressionEvents  []CompressionEvent
	TimeToFirstTokenMS int64
	TotalResponseMS    int64
	StopReason         string
	// HTTPStatus / ErrorClass / ErrorMessage capture upstream API
	// failures (4xx / 5xx) the proxy observed. Pre-v1.4.20 these were
	// dropped — the proxy returned early on non-2xx responses. Now an
	// errored turn is recorded with zero token counts and these three
	// fields populated. ErrorClass is the parsed error type from the
	// Anthropic / OpenAI envelope (`invalid_request_error` /
	// `rate_limit_error` / `overloaded_error` / etc.); ErrorMessage is
	// the human-readable body after secrets scrubbing. Successful
	// turns leave HTTPStatus = 0 and the strings empty.
	HTTPStatus   int
	ErrorClass   string
	ErrorMessage string
}

// CompressionEvent is one mechanism-tagged compression decision
// recorded during the conversation-compression pipeline. Stored in
// the compression_events table keyed off APITurn.ID. Mechanism is
// 'json' / 'code' / 'logs' / 'text' / 'diff' / 'html' (per-content-
// type compressor) or 'drop' (low-importance message replaced by a
// marker).
type CompressionEvent struct {
	APITurnID       int64
	Timestamp       time.Time
	Mechanism       string
	OriginalBytes   int64
	CompressedBytes int64
	MsgIndex        int
	ImportanceScore float64 // set only for Mechanism == "drop"
}

// FileState is the cross-session record of a file's last observed content
// hash. Drives the freshness fast path (spec §7.2 step 2).
type FileState struct {
	ID             int64
	ProjectID      int64
	FilePath       string
	ContentHash    string
	FileMtime      time.Time
	FileSizeBytes  int64
	LastActionID   int64
	LastActionType string
	LastSeenAt     time.Time
	LastModifiedBy string
}
