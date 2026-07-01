package models

import "time"

// Tool identifiers. These are the stable string values stored in the `tool`
// column of sessions, actions, and token_usage. Adapters must return one of
// these from Adapter.Name().
const (
	ToolClaudeCode = "claude-code"
	ToolCodex      = "codex"
	ToolCursor     = "cursor"
	// ToolCline is the VS Code Cline extension (`saoudrizwan.claude-dev`)
	// — stores task history as a JSON array at
	// `<globalStorage>/saoudrizwan.claude-dev/tasks/<task-id>/
	// api_conversation_history.json`. Distinct from ToolClineCLI
	// (the npm-distributed `cline` 3.x CLI), which is a different
	// product from the same authors with its own SQLite-backed
	// persistence at `~/.cline/data/`.
	ToolCline    = "cline"
	ToolRooCode  = "roo-code"
	ToolCopilot  = "copilot"
	ToolOpenCode = "opencode"
	ToolOpenClaw = "openclaw"
	ToolPi       = "pi"
	// ToolGeminiCLI is Google's Gemini CLI agent (`@google/gemini-cli`),
	// the Node.js terminal AI tool that writes plain JSON / JSONL
	// session files under ~/.gemini/tmp/<project_hash>/chats/.
	// Unrelated to ToolAntigravity despite the shared parent dir.
	ToolGeminiCLI = "gemini-cli"
	// ToolAntigravity is Google's Antigravity IDE (VS Code fork shipped
	// alongside Gemini 3, Nov 2025). Stores conversation state as
	// AES-encrypted Protocol Buffer files under
	// ~/.gemini/antigravity/conversations/<uuid>.pb plus a SQLite-
	// backed index in state.vscdb.
	ToolAntigravity = "antigravity"
	// ToolCowork is Anthropic's Claude Cowork (the "knowledge-work"
	// desktop product layered on top of Claude Code's CLI). Stores
	// session data as audit.jsonl per local-instance under
	// %LOCALAPPDATA%\Packages\Claude_*\LocalCache\Roaming\Claude\local-agent-mode-sessions\
	// (MSIX-redirected on Windows) or
	// ~/Library/Application Support/Claude/local-agent-mode-sessions
	// (macOS). Each local-instance directory is one Observer session;
	// audit.jsonl carries the canonical assistant/user/system/result
	// records plus the inner-Claude-Code session's rich usage payload
	// (5m/1h cache split, service_tier, inference_geo).
	ToolCowork = "cowork"
	// ToolCopilotCLI is GitHub's agentic Copilot CLI (`@github/copilot`
	// npm package, binary at `~/.nvm/.../bin/copilot`), distinct from
	// ToolCopilot (the VS Code Copilot Chat extension). Stores session
	// data as event-stream JSONL at
	// ~/.copilot/session-state/<uuid>/events.jsonl plus per-process
	// debug logs at ~/.copilot/logs/process-*.log. With
	// `--log-level debug` set, the log captures full upstream API
	// response usage objects (prompt_tokens, completion_tokens,
	// cached_tokens, reasoning_tokens) that are NOT exposed in
	// events.jsonl. The adapter joins log usage to events.jsonl
	// assistant.message rows via Request-ID.
	ToolCopilotCLI = "copilot-cli"
	// ToolHermes is Nous Research's Hermes Agent, an open-source
	// multi-platform autonomous AI agent (MIT, Python, schema v14
	// SQLite-backed at ~/.hermes/state.db). 70+ built-in tools across
	// ~28 toolsets, MCP client+server, persistent SOUL.md / MEMORY.md,
	// 18+ LLM providers (Anthropic / OpenAI / OpenRouter / Nous Portal /
	// Gemini / Nvidia / …). Capture is hooks-primary (the documented
	// public plugin API — ctx.register_hook("post_tool_call", …) etc.
	// — installed as a Python plugin at ~/.hermes/plugins/superbased-
	// observer/) with SQLite backfill for historical sessions. The
	// SQLite backfill path reads model strings with provider prefixes
	// (e.g. "nvidia/nemotron-3-ultra:free") and OpenRouter-style :suffix
	// tails that the cost engine strips before pricing lookup.
	// Distinct from Nous Research's Hermes LLM family (Hermes 3, …) —
	// this is the agent runtime.
	ToolHermes = "hermes"
	// ToolKiloCode is the legacy Kilo Code IDE extension (`kilocode.kilo-code`,
	// a Cline + Roo Code fork distributed as a VS Code / JetBrains extension).
	// Persistence layout is byte-identical to ToolCline: per-task directories
	// under `<vsCodeGlobalStorage>/kilocode.kilo-code/tasks/<taskId>/` carrying
	// `api_conversation_history.json` (Anthropic-shaped) + `ui_messages.json`.
	// The kilo-code adapter shares the Cline parse loop and re-tags emitted
	// rows with Tool = "kilo-code" rather than "cline". Distinct from
	// ToolKiloCodeCLI (the npm-distributed @kilocode/cli, an OpenCode fork
	// with its own SQLite store).
	ToolKiloCode = "kilo-code"
	// ToolKiloCodeCLI is the current Kilo Code CLI (`@kilocode/cli` npm
	// package, binary `kilo`, a fork of sst/opencode). The all-new IDE
	// extension is rebuilt on this CLI runtime. Captures via a SQLite store
	// at `~/.local/share/kilo/kilo.db` — same path on Linux, macOS, AND
	// Windows (Kilo intentionally mirrors XDG on every OS). Schema is
	// OpenCode-shaped (`message`/`part`/`todo`) plus Kilo extensions
	// (`project`/`workspace`/`event`/`session_message`/`account`/
	// `permission`/`session_share`). Tokens land on every assistant message
	// in `data.tokens = {total, input, output, reasoning, cache: {read, write}}`.
	// Provider id is `kilo` (the bundled Kilo Gateway,
	// pkg=@kilocode/kilo-gateway); model id form is `kilo-auto/<tier>` for
	// Gateway auto-routing or `<provider>/<model>` for direct providers.
	// Distinct from ToolOpenCode (sst/opencode itself, watching
	// `~/.local/share/opencode/opencode.db`). The cross-mount stageMirror
	// pattern from the opencode adapter applies — foreign-mount kilo.db
	// reads via /mnt/c on WSL need a local mirror before SQLite can open.
	ToolKiloCodeCLI = "kilo-code-cli"
	// ToolClineCLI is the npm-distributed `cline` 3.x CLI (Cline Bot Inc.,
	// Apache-2.0; binary at `%APPDATA%\npm\node_modules\cline\bin\cline`
	// on Windows or `~/.local/bin/cline` on Linux/macOS), distinct from
	// ToolCline (the VS Code Cline extension). Stores all session data
	// under `~/.cline/data/` (every OS, NOT %LOCALAPPDATA% on Windows):
	// `db/sessions.db` (WAL, 28-column sessions table + subagent_spawn_queue
	// + schedules + schedule_executions) paired with per-session JSON at
	// `sessions/<id>/<id>.json` (metadata + cost aggregates) and
	// `sessions/<id>/<id>.messages.json` (Anthropic-shaped content-block
	// conversation history). Capture strategy is SQLite-backfill primary +
	// messages.json content-block walker; optional hook-log JSONL tailer
	// against `logs/hooks.jsonl` once the operator registers hook commands
	// under `<workspace>/.clinerules/hooks/` or `<CLINE_DIR>/hooks/`.
	// First-class sub-agent model — `parent_session_id` / `parent_agent_id` /
	// `agent_id` / `is_subagent` columns surface directly onto the dashboard's
	// parent-child grouping. The 18 `team_*` tools (team_spawn_teammate,
	// team_send_message, team_broadcast, …) all map to `mcp_call` for v1;
	// dedicated team-comm action types may come in v2. NEVER reads
	// `settings/providers.json` (carries WorkOS OAuth tokens + per-provider
	// API keys), `cache/user_input_history.jsonl` (cross-session prompts),
	// or `secrets.json`.
	ToolClineCLI = "cline-cli"
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
	// ActionAssistantMessage is the model's natural-language response text
	// (an assistant message / agent reply). Historically adapters recorded
	// this as ActionTaskComplete with RawToolName "<tool>.assistant_text",
	// which is the wrong semantic label (the assistant talking is not a task
	// completion). New/swept adapters use this dedicated type; the dashboard
	// assistant-text surface keys off the RawToolName "<tool>.assistant_text"
	// suffix (not the ActionType), so the relabel is display-safe. Reasoning
	// rows reuse "<tool>.reasoning"; this is for the spoken response.
	ActionAssistantMessage = "assistant_message"
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
	// ActionPromptContext is a NON-content prompt-budget component: a
	// named slice of the prompt (tool definitions, rules, skills,
	// subagent definitions, …) whose CONTENT the source tool does not
	// persist, but whose token/char COUNT it records. Distinct from
	// ActionSystemPrompt (which carries real content) so the dashboard
	// renders it as "Prompt context" rather than "System prompt".
	// Emitted as a zero-cost informational row (no token_usage) so the
	// operator can reconcile a turn's large input — the per-section
	// counts sum to ~the gross prompt the model received. First used by
	// the cursor adapter (store.db root-blob section index, where tools
	// + rules typically dominate the input). Target carries
	// "<Section> — N tokens, M chars"; RawToolName is
	// "prompt_section.<name>".
	ActionPromptContext = "prompt_context"
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
	// ActionToolFailure captures a tool call that failed at the host level
	// (the host returned an error to the model, distinct from an upstream
	// API error). Surfaces hook-side observability for tool failures whose
	// pairing in the JSONL transcript is awkward (the transcript carries
	// tool_result with is_error=true but not the structured failure_type
	// or duration_ms that the post-tool-failure hook does). Target carries
	// the tool name, RawToolName the failure_type when reported, ErrorMessage
	// the human-readable body. Success is always false.
	ActionToolFailure = "tool_failure"
	// ActionSubagentStart / ActionSubagentStop bracket a sub-agent's own
	// runtime, distinct from the parent's tool_use that launched it
	// (which remains ActionSpawnSubagent). The pair carries agent_id +
	// agent_type so dashboards can chart per-subagent fan-out, total
	// time, and final response length.
	ActionSubagentStart = "subagent_start"
	ActionSubagentStop  = "subagent_stop"
	// ActionSessionStart / ActionSessionEnd are explicit session-lifecycle
	// markers from hook events. Sources that capture sessions via JSONL
	// watcher (claude-code) infer these from the first/last record;
	// hook-only or proxy-only sources (codex pre-watcher, cursor) need
	// the explicit rows. Target carries the source/exit reason
	// ("startup|resume|clear|compact" for start; "clear|resume|logout|
	// prompt_input_exit|bypass_permissions_disabled|other" for end).
	ActionSessionStart = "session_start"
	ActionSessionEnd   = "session_end"
	// ActionNotification captures host-level notification dispatches —
	// permission_prompt, idle_prompt, auth_success, elicitation_*. Target
	// carries the notification_type, ErrorMessage carries the message body.
	ActionNotification = "notification"
	// ActionCwdChange records a working-directory change observed by the
	// host (Claude Code's CwdChanged hook). Target carries the new cwd,
	// PrecedingReasoning carries the previous cwd (so before/after pairs
	// are diffable from a single row).
	ActionCwdChange = "cwd_change"
	// ActionUserPromptExpansion captures a user prompt that expanded into a
	// slash-command or MCP-prompt invocation. Distinct from ActionUserPrompt
	// (the free-text submit): UserPromptExpansion fires AFTER UserPromptSubmit
	// when the input matches a registered slash-command or mcp-prompt name.
	// Target carries command_name; RawToolName carries expansion_type
	// ("slash_command" | "mcp_prompt"); RawToolInput carries the original
	// prompt text (slashes intact) plus command_source / command_args as
	// JSON so analysts can see what the user typed before expansion.
	ActionUserPromptExpansion = "user_prompt_expansion"
	// ActionPostToolBatch is the end-of-batch summary fired after a run
	// of consecutive tool calls. Distinct from per-tool PostToolUse rows:
	// PostToolBatch carries the LIST of tool_calls in the batch (their
	// names + serialized results) as one row. Target carries the batch
	// tool-count summary ("N tool call(s)"); RawToolInput carries the
	// tool_calls JSON array (scrubbed/truncated) so analysts can see the
	// batch composition without joining N rows.
	ActionPostToolBatch = "post_tool_batch"
	// ActionPermissionRequest captures an explicit permission-check fire
	// where the host asks the user (or auto-mode classifier) to authorize
	// a tool call. Target carries the tool_name being asked about;
	// RawToolName the tool_name verbatim; RawToolInput the tool_input
	// arguments scrubbed; PrecedingReasoning the permission_suggestions
	// JSON (e.g. addRules / setMode proposals) when present so analysts
	// can see WHAT was suggested as the resolution. Success is true (the
	// request itself is just the prompt — the outcome lands as either
	// a continued tool execution or an ActionPermissionDenied row).
	ActionPermissionRequest = "permission_request"
	// ActionPermissionDenied captures an auto-mode classifier denial.
	// Distinct from ActionToolFailure: ToolFailure is the tool itself
	// failing; PermissionDenied is the permission layer refusing to
	// dispatch the tool in the first place. Target carries tool_name;
	// RawToolName tool_name; RawToolInput the tool_input arguments;
	// ErrorMessage the classifier's reason text. Success is always false.
	ActionPermissionDenied = "permission_denied"
	// ActionPermissionMode captures a permission-mode toggle — Claude
	// Code's `permission-mode` line type, written whenever the user
	// enters or exits plan mode / acceptEdits / similar. Target carries
	// the new mode value ("plan" | "acceptEdits" | "default"); RawToolName
	// stays empty; RawToolInput holds the raw line's JSON for any
	// future-added fields. Lifecycle marker — not a tool call.
	//
	// Pre-v1.6.10 these lines were silently dropped on the claudecode
	// JSONL path (audit B4, operator-confirmed oversight 2026-05-18).
	ActionPermissionMode = "permission_mode"
	// ActionSetup captures Claude Code's per-session setup / maintenance
	// fire (`--init-only`, `-p --init`, `-p --maintenance`). Lifecycle
	// marker distinct from ActionSessionStart: Setup fires only on init/
	// maintenance modes, not on every session launch. Target carries the
	// trigger ("init" | "maintenance").
	ActionSetup = "setup"
	// ActionInstructionsLoaded captures a CLAUDE.md / instructions file
	// load fire. Lifecycle marker for which file landed in context and
	// why. Target carries the file_path; RawToolName the memory_type
	// ("User" | "Project" | "Local" | "Managed"); RawToolInput the
	// load_reason and optional globs / trigger_file_path / parent_file_path
	// fields as JSON.
	ActionInstructionsLoaded = "instructions_loaded"
	// ActionConfigChange captures a settings.json mutation observed by
	// the host. Lifecycle marker for cross-session policy / permission /
	// MCP-server changes. Target carries the file_path (when reported);
	// RawToolName the source ("user_settings" | "project_settings" |
	// "local_settings" | "policy_settings" | "skills").
	ActionConfigChange = "config_change"
	// ActionWorktreeCreate captures Claude Code's WorktreeCreate hook
	// fired when an Agent spawn requests `isolation: "worktree"`.
	// Blocking hook — observer's handler must output a worktree path
	// on stdout (per `code.claude.com/docs/en/hooks` matrix) or the
	// spawn fails. Target carries the worktree name; RawToolInput
	// carries the echoed path (so dashboards can confirm where the
	// worktree was placed). NOT in the default claudeCodeEvents
	// registration list — opt-in only via manual settings.json edit
	// (see docs/claude-worktree-hook.md).
	ActionWorktreeCreate = "worktree_create"
	// ActionWorktreeRemove captures Claude Code's WorktreeRemove hook
	// fired when a worktree is cleaned up. Non-blocking (logging only
	// per the docs matrix). Target carries the worktree_path; safe to
	// register by default — incorrect handler behavior cannot break
	// spawns.
	ActionWorktreeRemove = "worktree_remove"
	// ActionRateLimit captures a host-emitted rate-limit status check.
	// Cowork's audit.jsonl emits a `rate_limit_event` record per poll
	// (~50/session in observed data) carrying the current window
	// status (allowed/rejected) and reset time. Codex 0.130+ emits the
	// same shape inside `token_count.rate_limits.{primary,secondary,
	// credits}` per turn — when that landing lands, the codex adapter
	// emits ActionRateLimit too. Target carries the rateLimitType
	// (e.g. "five_hour" / "primary"); RawToolName the status; Success
	// is true when status=="allowed". The full rate_limit_info JSON
	// is scrubbed into RawToolInput; typed fields land on
	// ActionMetadata.RateLimit*.
	ActionRateLimit = "rate_limit"
	ActionUnknown   = "unknown"
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
	// TokenSourceLogDelta is the copilot-cli Tier 2 estimate — derived
	// from `CompactionProcessor: Utilization X% (CTX/128000 tokens)`
	// snapshots in the process log when no upstream usage block was
	// captured for the matching response. Carries InputTokens only
	// (the gross prompt size at the time of the request); OutputTokens
	// is filled in by the Tier 3 (events.jsonl) row that shares the
	// same MessageID. Always reliability='approximate'.
	TokenSourceLogDelta = "log_delta"
	// TokenSourceSessionSummary is the copilot-cli Tier 0 capture —
	// derived from `session.shutdown.data.modelMetrics` in events.jsonl.
	// Each entry covers one model's cumulative usage delta for the work
	// span between the most recent `session.resume` and this
	// `session.shutdown`. Carries InputTokens / CacheReadTokens /
	// CacheCreationTokens (from `cacheWriteTokens`) / ReasoningTokens;
	// OutputTokens is left zero because Tier 3 (`source='jsonl'`)
	// already captures per-message outputTokens and including them
	// here would double-count. Superseded by Tier 1 (`source='otel'`)
	// when debug logging is on — the store-layer dedup drops
	// session_summary rows for any session that has an otel row, since
	// Tier 1 already has full per-request breakdowns. Always
	// reliability='approximate'.
	TokenSourceSessionSummary = "session_summary"

	ReliabilityAccurate    = "accurate"
	ReliabilityApproximate = "approximate"
	ReliabilityUnreliable  = "unreliable"
	ReliabilityUnknown     = "unknown"
)

// API providers recognized by the proxy (spec §9).
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	// ProviderGoogle is Google's Gemini generateContent API
	// (generativelanguage.googleapis.com). Distinct wire shape from
	// Anthropic/OpenAI — the proxy parses usageMetadata for token capture.
	ProviderGoogle = "google"
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
	// OrgID / UserEmail are org-attribution (Teams & Org Visibility,
	// migration 029). Empty on solo-local installs; stamped by
	// identity.Stamper at insert time only when the agent is enrolled,
	// persisted as NULL otherwise.
	OrgID     string
	UserEmail string
}

// ActionMetadata is the per-event JSON-marshaled metadata column on
// actions (migration 017). Captures fields Claude Code and Codex
// hook payloads emit on every fire that don't fit the typed columns:
// the permission mode the host was in (default | bypass_permissions
// | plan), the Codex reasoning effort level (minimal | low | medium
// | high), and whether a tool failure was a user interrupt vs a
// genuine error.
//
// v1.4.52 added codex 0.130+ turn_context fields:
//   - CollaborationMode  ("default" | "plan" — high-signal because
//     plan mode is read-only-thinking)
//   - Personality        (Codex Desktop persona; "friendly" etc.)
//   - RealtimeActive     (bool — true while the real-time/voice
//     surface is active; unstable signal until docs land)
//   - TruncationMode +   (codex's per-turn truncation strategy +
//     TruncationLimit     token budget — useful for "why was this
//     turn shortened" forensics)
//   - TimeToFirstTokenMS (latency from task_started to first
//     assistant token on task_complete events
//     only; signals model warmup + queue time)
//
// All fields are omitempty — a zero-valued struct marshals to {} and
// the store layer persists NULL instead, so the column stays dense.
//
// Note: Codex Desktop's `speed` toggle (standard | fast) is NOT
// captured here because Codex 0.130.0-alpha.5 does not persist it
// into the rollout JSONL. Empirically verified by flipping the
// toggle mid-session on session 019e22b1-… and re-grepping — no
// `speed`/`priority`/`tier`/`latency` field appears anywhere in the
// post-flip rollout. Tracked as deferred until Codex emits it.
//
// v1.4.53 added Cowork-specific fields plus shared fields generalizable
// to other Anthropic-API consumers:
//   - CoworkProcessName  (per-local-instance "adj-adj-name" identifier
//     from sidecar.processName)
//   - CoworkTitle        (Cowork's auto-generated session title)
//   - HostLoopMode       (true = uses host filesystem; false = sandbox)
//   - ServiceTier        (assistant.message.usage.service_tier;
//     "standard" / "priority"). Generalizes — codex
//     0.130+ also emits this on token_count rows.
//   - InferenceGeo       (assistant.message.usage.inference_geo)
//   - CacheCreate5mTok / (5m vs 1h split inside cache_creation_input_tokens —
//     CacheCreate1hTok    the 1h tier is priced ~2× the 5m default; this
//     pair is the first time observer captures it on
//     the action row, complementing the existing
//     TokenEvent.CacheCreation1hTokens proxy field)
//   - TotalCostUSD       (Cowork-authoritative cost per task on result rows;
//     calibration target for observer's derived cost)
type ActionMetadata struct {
	PermissionMode     string `json:"permission_mode,omitempty"`
	EffortLevel        string `json:"effort_level,omitempty"`
	IsInterrupt        bool   `json:"is_interrupt,omitempty"`
	CollaborationMode  string `json:"collaboration_mode,omitempty"`
	Personality        string `json:"personality,omitempty"`
	RealtimeActive     bool   `json:"realtime_active,omitempty"`
	TruncationMode     string `json:"truncation_mode,omitempty"`
	TruncationLimit    int64  `json:"truncation_limit,omitempty"`
	TimeToFirstTokenMS int64  `json:"time_to_first_token_ms,omitempty"`
	CoworkProcessName  string `json:"cowork_process_name,omitempty"`
	CoworkTitle        string `json:"cowork_title,omitempty"`
	HostLoopMode       bool   `json:"host_loop_mode,omitempty"`
	ServiceTier        string `json:"service_tier,omitempty"`
	InferenceGeo       string `json:"inference_geo,omitempty"`
	// StopReason is the assistant message's terminal reason
	// (end_turn / max_tokens / tool_use / stop_sequence / refusal /
	// pause_turn). Per-message; surfaced per message in session review.
	// Captured from the on-disk transcript (claude-code, cowork) — the
	// hook payloads don't carry it. Distinct from api_turns.stop_reason
	// (the proxy path); this is the watcher/transcript path.
	StopReason             string  `json:"stop_reason,omitempty"`
	CacheCreate5mTok       int64   `json:"cache_create_5m_tok,omitempty"`
	CacheCreate1hTok       int64   `json:"cache_create_1h_tok,omitempty"`
	TotalCostUSD           float64 `json:"total_cost_usd,omitempty"`
	CoworkToolSummary      string  `json:"cowork_tool_summary,omitempty"`
	RateLimitStatus        string  `json:"rate_limit_status,omitempty"`
	RateLimitType          string  `json:"rate_limit_type,omitempty"`
	RateLimitResetsAt      int64   `json:"rate_limit_resets_at,omitempty"`
	RateLimitOverageStatus string  `json:"rate_limit_overage_status,omitempty"`
	// PermissionApprovalKind captures the specific approval granularity
	// reported by `permission.completed.result.kind`. Empirically observed
	// values: "approved" (generic, single-call), "approved-for-location"
	// (scoped to a filesystem prefix in LocationKey), "approved-for-session"
	// (scoped to the lifetime of the session), "denied". For plain "approved"
	// the field stays empty so the column doesn't churn — only the
	// non-default kinds are recorded. Source: copilotcli (v1.6.13).
	PermissionApprovalKind string `json:"permission_approval_kind,omitempty"`
	// PermissionLocationKey is the filesystem prefix bound to an
	// approved-for-location permission grant. Captured verbatim from
	// `permission.completed.result.locationKey`. Example: "D:\\OneDrive -
	// Microsoft". Source: copilotcli (v1.6.13).
	PermissionLocationKey string `json:"permission_location_key,omitempty"`
	// ParentSessionID identifies the parent of a sub-agent session.
	// Populated by the clinecli adapter from sessions.parent_session_id
	// (and by future adapters with first-class sub-agent models). Empty
	// for lead sessions and for sessions on platforms without explicit
	// parent linkage. Distinct from the existing Action.IsSidechain
	// flag — IsSidechain marks Claude-Code-style same-session sub-agent
	// activity; ParentSessionID marks a sub-agent whose lifecycle lives
	// in its OWN session row (the cline-cli / hermes model).
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// ParentAgentID identifies the lead agent that spawned this sub-
	// agent. Pairs with ParentSessionID. Populated by clinecli from
	// sessions.parent_agent_id.
	ParentAgentID string `json:"parent_agent_id,omitempty"`
	// AgentID is the running session's own agent identifier — usually
	// a short string like "agt_lead_<sid>" or a teammate name. Empty
	// for sessions on platforms without an agent_id column.
	AgentID string `json:"agent_id,omitempty"`
	// IsSubagent marks whether the owning session is a sub-agent
	// (clinecli sessions.is_subagent = 1). Pre-existing
	// Action.IsSidechain serves the same role for Claude-Code-style
	// sub-agents; the two are independent.
	IsSubagent bool `json:"is_subagent,omitempty"`
	// TeamName is the team this session is enrolled in (Cline CLI's
	// teams.db namespace). Populated when sessions.team_name is set —
	// note that a session can have team_name without is_subagent=1
	// (the workspace has a team config but this run isn't spawned as
	// a teammate). Phase 0 reality-check finding.
	TeamName string `json:"team_name,omitempty"`
}

// IsZero reports whether the struct has no non-zero fields. Used by
// the store layer to decide between writing the JSON blob and NULL.
//
// IMPORTANT (Invariant #50): every new field added to ActionMetadata
// MUST be added to this check or sparse-zero rows will marshal to
// non-NULL "{}" and pollute the column. Pinned by the reflection
// invariant TestActionMetadata_IsZeroCoversEveryField.
func (m ActionMetadata) IsZero() bool {
	return m.PermissionMode == "" && m.EffortLevel == "" && !m.IsInterrupt &&
		m.CollaborationMode == "" && m.Personality == "" && !m.RealtimeActive &&
		m.TruncationMode == "" && m.TruncationLimit == 0 && m.TimeToFirstTokenMS == 0 &&
		m.CoworkProcessName == "" && m.CoworkTitle == "" && !m.HostLoopMode &&
		m.ServiceTier == "" && m.InferenceGeo == "" &&
		m.CacheCreate5mTok == 0 && m.CacheCreate1hTok == 0 && m.TotalCostUSD == 0 &&
		m.CoworkToolSummary == "" && m.RateLimitStatus == "" && m.RateLimitType == "" &&
		m.RateLimitResetsAt == 0 && m.RateLimitOverageStatus == "" &&
		m.PermissionApprovalKind == "" && m.PermissionLocationKey == "" &&
		m.ParentSessionID == "" && m.ParentAgentID == "" && m.AgentID == "" &&
		!m.IsSubagent && m.TeamName == "" && m.StopReason == ""
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
	RawToolInput       string // Scrubbed tool input as rendered for the dashboard. Adapter-capped at 1 MiB via internal/contentcap.
	// RawToolOutput is the full, scrubbed tool_result body. Adapter-
	// capped at 1 MiB via internal/contentcap and stored verbatim in
	// actions.raw_tool_output so the dashboard's on-demand full-text
	// endpoint can serve the operator the real bytes (not just the
	// 2 KB FTS5 excerpt in action_excerpts). Empty when the adapter
	// never saw the paired result.
	RawToolOutput string
	Tool          string
	SourceFile    string
	SourceEventID string
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
	// Metadata is per-event JSON metadata captured by hook adapters
	// (permission_mode / effort_level / is_interrupt). Nil when no
	// fields apply; the store layer marshals non-nil values to JSON
	// for the actions.metadata column. See ActionMetadata.
	Metadata *ActionMetadata
	// OrgID / UserEmail are org-attribution (Teams & Org Visibility,
	// migration 029). Empty on solo-local installs; stamped by
	// identity.Stamper at insert time only when the agent is enrolled,
	// persisted as NULL otherwise.
	OrgID     string
	UserEmail string
	// ContentBytes is the byte length of the code the model AUTHORED in
	// this action (Output Composition / Verbosity feature, migration 054):
	// Write `content`, Edit `new_string`, MultiEdit Σ`new_string`,
	// NotebookEdit `new_source`, or the `command` for a run_command. It is
	// a length, never the content. 0 (→ NULL on disk) for actions that
	// authored nothing (reads, searches) and on adapters that don't yet
	// compute it. NODE-LOCAL — not on the org-push seam.
	ContentBytes int64
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
	// ContentBytes is the byte length of code the model authored in this
	// action (see [Action.ContentBytes]). Computed by the adapter from the
	// untruncated tool input; flows to actions.content_bytes.
	ContentBytes int64
	// ToolOutput is the scrubbed tool_result body. Lands in two places
	// at store time: (a) the actions.raw_tool_output column verbatim
	// (capped at 1 MiB by the adapter via internal/contentcap), so the
	// dashboard's on-demand full-text endpoint can serve it; and (b)
	// the FTS5 action_excerpts table, trimmed to 2 KiB by the Indexer
	// for search. Empty when the adapter didn't see the paired result.
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
	// Metadata is per-event hook metadata that survives the
	// ToolEvent → Action conversion in store.Ingest. Nil when the
	// adapter has no metadata to record. See [ActionMetadata].
	Metadata *ActionMetadata
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
	// WebSearchRequests is the count of server-side web_search invocations
	// billed under Anthropic's "$10 per 1,000 searches" fee — separate from
	// per-token costs. Zero for non-Anthropic providers and for events that
	// didn't trigger web_search. The cost engine adds
	// web_search_requests × Pricing.WebSearchPerRequest to the total.
	WebSearchRequests int64
	// Fast marks turns served in the provider's low-latency "fast" tier
	// (Anthropic Opus 4.8 with speed:"fast" on the Messages API). When
	// true AND the model's Pricing.FastMultiplier > 0, the cost engine
	// scales the AI token cost by FastMultiplier. Stamped by the proxy
	// when the outbound request body carries "speed":"fast", and by the
	// claude-code JSONL adapter when the usage envelope echoes
	// `speed:"fast"`; defaults false everywhere else. Persisted to
	// token_usage.fast via migration 035.
	Fast             bool
	EstimatedCostUSD float64
	Source           string
	Reliability      string
	// MessageID is the per-API-call identifier this token row belongs to.
	// For Anthropic adapters (claudecode, cline, openclaw, …) it's the
	// upstream `msg_xxx` returned by the Messages API — one MessageID
	// per API request. For codex (v1.7.24+) it's the per-event identifier
	// derived from the rollout JSONL line (`tk:<file>:L<n>`), since codex
	// emits one token_count event per model inference and a single user-
	// turn typically produces multiple inferences. See TurnID below to
	// recover the turn-level grouping.
	MessageID string
	// TurnID groups token rows that belong to the same user-turn. Populated
	// by adapters whose per-API-call granularity is finer than the user-
	// turn boundary (codex emits N token_count events per turn). NULL on
	// adapters where MessageID already corresponds 1:1 to a user-turn
	// (claudecode and the other Anthropic adapters). Persisted to
	// token_usage.turn_id by migration 032.
	TurnID string
	// OrgID / UserEmail are org-attribution (Teams & Org Visibility,
	// migration 029). Empty on solo-local installs; stamped by
	// identity.Stamper at insert time only when the agent is enrolled,
	// persisted as NULL otherwise.
	OrgID     string
	UserEmail string
}

// APITurn is one request/response pair observed by the proxy. Accurate token
// counts come from the provider's response body; session/project linkage is
// best-effort (nil session_id when the caller omits the X-Session-Id header).
// See spec §9 and the api_turns schema in §6.2.
// LimitSnapshot is one rate-limit / subscription-window observation
// parsed from an upstream HTTP response on the proxy path — the limit
// half of the Next-Message Cost & Limit Predictor. NODE-LOCAL (never
// pushed to an org server; migration 049). Optional window/classic
// fields are pointers so an absent header maps to a NULL column rather
// than a misleading zero.
type LimitSnapshot struct {
	ID         int64
	ScopeHash  string // auth-identity hash (R4); "default" fallback
	Provider   string // anthropic | openai
	SessionID  string // best-effort; "" when the proxy couldn't resolve one
	ObservedAt time.Time

	Window5hUtil  *float64 // 0..1
	Window5hReset *int64   // unix seconds
	Window7dUtil  *float64
	Window7dReset *int64

	ReqLimit     *int64
	ReqRemaining *int64
	ReqReset     *int64
	TokLimit     *int64
	TokRemaining *int64
	TokReset     *int64

	Status string // unified-status passthrough
	Raw    string // allow-listed, scrubbed header subset; "" when none
}

// HasAnyWindow reports whether the snapshot carries at least one
// subscription-window or classic limit signal — the gate for persisting
// it (a response with no limit headers at all is not worth a row).
func (s LimitSnapshot) HasAnyWindow() bool {
	return s.Window5hUtil != nil || s.Window7dUtil != nil ||
		s.ReqRemaining != nil || s.TokRemaining != nil
}

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
	// WebSearchRequests mirrors TokenEvent.WebSearchRequests on the proxy
	// path — number of server-side web_search invocations billed under
	// Anthropic's $10/1000 search fee, independent of per-token costs.
	WebSearchRequests int64
	// Fast marks turns served in the provider's low-latency "fast" tier
	// (Anthropic Opus 4.8 with speed:"fast" on the Messages API).
	// Mirrors TokenEvent.Fast — captured by the proxy when the outbound
	// request body carries "speed":"fast". Persisted to api_turns.fast
	// via migration 035. The CostUSD on this row is the FastMultiplier-
	// applied total computed at insert time.
	Fast             bool
	CostUSD          float64
	MessageCount     int
	ToolUseCount     int
	SystemPromptHash string
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
	// OrgID / UserEmail are org-attribution (Teams & Org Visibility,
	// migration 029). Empty on solo-local installs; stamped by
	// identity.Stamper at insert time only when the agent is enrolled,
	// persisted as NULL otherwise.
	OrgID     string
	UserEmail string
	// Source is the provenance of this turn observation for cross-source
	// dedup (native-console integration, migration 047): "proxy" (or empty,
	// the legacy default), "cc_otel", or "jsonl". It maps to a
	// turnmerge.Fidelity at the store boundary, which decides precedence when
	// the same request_id is seen by more than one source. Node-local
	// metadata — never pushed to the org wire.
	Source string
}

// OTelContent is one captured content body from a coding assistant's native
// OTel stream (native-console integration, migration 048): a user prompt, tool
// input/output, or raw API body, emitted only when the admin enabled the
// OTEL_LOG_* flags. Content is scrubbed for secrets before storage. Like
// actions.raw_tool_input, the node STORES it locally; the org-push boundary
// ships ContentHash always and Content (raw) only under the content-sharing
// gate (full_content / admin_managed).
type OTelContent struct {
	ID          int64
	RequestID   string // turn join key (may be empty for session-level prompts)
	SessionID   string
	ToolUseID   string // set for tool_input/tool_output kinds
	Kind        string // prompt | tool_input | tool_output | raw_body
	Content     string // scrubbed raw body
	ContentHash string // sha256-hex of Content
	Timestamp   time.Time
	Source      string // cc_otel
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
	// BodyHash is sha256-hex of the pre-compression body bytes
	// (V7-9, v1.7.12+). Empty for pre-v1.7.12 rows and 'drop'
	// events. Persisted into compression_events.body_hash via
	// migration 031.
	BodyHash string
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

// CacheBlockMeta is one element of a Tier-2 cache observation's
// block chain (docs/plans/cache-tracking-implementation-spec-2026-06-08.md
// §9). Adapters populate this from the JSONL message-content
// blocks they parse; the engine consumes it via the
// internal/cachetrack package (the engine, NOT this models type,
// is where canonicalization + hashing happens).
//
// LevelLabel uses the schema-stable strings 'tools' / 'system' /
// 'message' so the boundary between adapter and engine stays
// data-only — adapters don't import the cachetrack package
// (spec §24.1) and the engine doesn't import models for its
// internal types (the BlockLevel enum stays internal).
type CacheBlockMeta struct {
	// LevelLabel is 'tools' | 'system' | 'message'. The empty
	// string is treated as 'message' by the engine — Tier-2
	// transcripts never expose tools/system blocks (R1 finding).
	LevelLabel string
	// Kind is the block type label: 'text' | 'tool_use' |
	// 'tool_result' | 'image' | 'thinking' | 'document' |
	// 'attachment' (spec §0 R1: attachment lines fold into the
	// chain).
	Kind string
	// CanonicalBytes is the JSON-serialized block body (sorted
	// keys, no HTML escape — same shape internal/compression/
	// conversation/anthropic.go::marshalEnvelope produces). The
	// engine wraps this in its own role/type envelope at hash
	// time (see internal/cachetrack/block.go::CanonicalizeTranscript).
	CanonicalBytes []byte
	// Role is 'user' / 'assistant' / 'system' / 'tool' — the
	// schema-stable transcript role label. Empty for blocks
	// that don't carry one (rare).
	Role string
}

// CacheUsage carries the per-turn provider-reported usage fields
// the engine reconciles against (spec §10). All counts are
// non-negative; zero means "not observed" rather than "actively
// zero." NetInputTokens is the input field AFTER subtracting
// CacheReadTokens (per the cost engine's net-input invariant —
// see internal/intelligence/cost::TokenBundle.Input).
type CacheUsage struct {
	NetInputTokens        int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64
}

// CacheTurnObservation is one assistant turn's cache-relevant
// view, emitted by adapters that can see content blocks + usage
// (Tier 2 — claudecode JSONL, codex rollout, opencode, kilo-cli,
// cline-cli per spec §14.3 rollout). The watcher's store.Ingest
// receives a slice of these via store.IngestOptions and feeds
// the cachetrack engine in C7+; for C6 the slice plumbing is
// additive only.
//
// SourceFile + SourceEventID are the idempotency key: a
// re-parse of the same JSONL file (Rescan, pollCursors) must
// produce the same observations, and the engine's dedup gate
// (store.CacheEventExistsForMessage) tolerates Tier-1 having
// already written events for the same MessageID.
type CacheTurnObservation struct {
	SourceFile    string
	SourceEventID string // adapter-deterministic per-turn id (idempotency)
	SessionID     string
	MessageID     string // upstream msg_xxx (joins to api_turns.request_id)
	Timestamp     time.Time
	Model         string
	Fast          bool
	// BlockHashes is the per-turn block chain, IN ORDER. Empty
	// slice signals "could not reconstruct" (e.g. cold-start
	// incremental parse) — the engine treats the observation as
	// kind=reanchor in that case.
	BlockHashes []CacheBlockMeta
	// Usage is the provider-reported usage envelope for this turn.
	Usage CacheUsage
	// CompactionSeen is true when a compact_boundary lifecycle
	// marker landed between the prior assistant turn and this
	// one in the same session. The engine emits a
	// kind=compaction_reset event when set.
	CompactionSeen bool
	// ImplicitCache is the §15.3 boundary-overlay flag: adapters
	// emitting against OpenAI / OpenAI-compatible providers (codex
	// Tier-2, cline-cli when routed through deepseek/etc.,
	// opencode/kilo when routed through non-Anthropic gateways)
	// set this so the engine dispatches to the reduced attribution
	// path (cachetrack.attributeImplicit) instead of the marker-
	// aware Anthropic decision tree. When true, BlockHashes is
	// IGNORED by the engine (the implicit path doesn't push the
	// chain) and only Usage.CacheReadTokens / Usage.NetInputTokens
	// are consumed. Default false → existing Anthropic-shape
	// behavior unchanged.
	ImplicitCache bool
}
