package tooltax

import "sort"

// Canonical action types. These string VALUES are identical to the
// internal/models Action* constants; they are re-declared here because
// tooltax must not import internal/models (see doc.go — models imports
// tooltax, not the other way round). TestActionTypeValuesMatchModels in
// the external test package pins the equality.
const (
	ActionReadFile            = "read_file"
	ActionWriteFile           = "write_file"
	ActionEditFile            = "edit_file"
	ActionRunCommand          = "run_command"
	ActionSearchText          = "search_text"
	ActionSearchFiles         = "search_files"
	ActionWebSearch           = "web_search"
	ActionWebFetch            = "web_fetch"
	ActionBrowserAction       = "browser_action"
	ActionMCPCall             = "mcp_call"
	ActionSpawnSubagent       = "spawn_subagent"
	ActionTodoUpdate          = "todo_update"
	ActionTaskComplete        = "task_complete"
	ActionAskUser             = "ask_user"
	ActionUserPrompt          = "user_prompt"
	ActionAssistantMessage    = "assistant_message"
	ActionTurnAborted         = "turn_aborted"
	ActionContextCompacted    = "context_compacted"
	ActionSystemPrompt        = "system_prompt"
	ActionPromptContext       = "prompt_context"
	ActionAPIError            = "api_error"
	ActionToolFailure         = "tool_failure"
	ActionSubagentStart       = "subagent_start"
	ActionSubagentStop        = "subagent_stop"
	ActionSessionStart        = "session_start"
	ActionSessionEnd          = "session_end"
	ActionNotification        = "notification"
	ActionCwdChange           = "cwd_change"
	ActionUserPromptExpansion = "user_prompt_expansion"
	ActionPostToolBatch       = "post_tool_batch"
	ActionPermissionRequest   = "permission_request"
	ActionPermissionDenied    = "permission_denied"
	ActionPermissionMode      = "permission_mode"
	ActionSetup               = "setup"
	ActionInstructionsLoaded  = "instructions_loaded"
	ActionConfigChange        = "config_change"
	ActionWorktreeCreate      = "worktree_create"
	ActionWorktreeRemove      = "worktree_remove"
	ActionRateLimit           = "rate_limit"
	ActionUnknown             = "unknown"
)

// NEW canonical action types introduced by the taxonomy plan (§1). They
// close the measured `unknown` gap: the agent-orchestration + harness
// tool families that no existing action type described (codex
// wait/wait_agent/spawn_agent/send_message/write_stdin ≈ 1,900 rows;
// claude-code ToolSearch/Monitor/SendMessage/Skill/ScheduleWakeup/
// StructuredOutput ≈ 1,100 rows). Migration 024's comment deferred this
// explicitly: "adding categories for these is a separate taxonomy
// decision."
//
// WP-T4 promotes these to models.Action* constants and backfills the
// historical rows; until then tooltax is their only declaration site.
const (
	// ActionSubagentWait is a blocking wait on one or more already-spawned
	// sub-agents (codex `wait` / `wait_agent`). Distinct from
	// ActionSpawnSubagent (the launch) and from ActionSubagentStart/Stop
	// (the child's own lifecycle bracket) — this row is the PARENT
	// blocking on a child.
	ActionSubagentWait = "subagent_wait"
	// ActionAgentMessage is an inter-agent message: work or instructions
	// handed to an existing agent thread rather than to a new one
	// (codex `send_message` / `followup_task`, claude-code `SendMessage`).
	ActionAgentMessage = "agent_message"
	// ActionAgentControl is an orchestration verb that neither spawns,
	// waits, nor messages — enumerating, inspecting or interrupting the
	// agent/thread pool (codex `list_agents` / `interrupt_agent` /
	// `read_thread` / `list_threads` / `read_thread_terminal`,
	// claude-code `Monitor`). Extension beyond the plan's six named new
	// types; flagged for ratification in WP-T4.
	ActionAgentControl = "agent_control"
	// ActionSkillInvoke is a skill / packaged-instruction-set invocation
	// (claude-code + cowork `Skill`). Category "skill" exists so skill
	// usage is countable separately from MCP tool usage — the cowork
	// adapter currently folds Skill into mcp_call, which is why the
	// cowork row below keeps mcp_call (code wins) while the claude-code
	// row uses skill_invoke.
	ActionSkillInvoke = "skill_invoke"
	// ActionSchedule is a scheduling primitive: register/inspect/cancel a
	// future or recurring agent run (claude-code `ScheduleWakeup` /
	// `CronCreate` / `CronDelete` / `CronList`). Note droid's adapter
	// maps its own Cron*/Automation* family to config_change instead
	// ("the closest honest bucket" per droid/records.go:202-204) — code
	// wins there, and the divergence is recorded as a known discrepancy.
	ActionSchedule = "schedule"
	// ActionToolSearch is the deferred-tool loader: the agent searching
	// the tool registry for a tool to load (claude-code `ToolSearch`).
	// It is a search over the HARNESS, not over the project, so its
	// category is meta rather than search.
	ActionToolSearch = "tool_search"
	// ActionStdinWrite writes to the stdin of a live agent/exec session
	// (codex `write_stdin`). Categorised as agent/orchestration per the
	// plan: in codex it is the channel into an already-running agent or
	// exec_command thread, not a fresh shell invocation.
	ActionStdinWrite = "stdin_write"
	// ActionHarnessCall is the honest bucket for host-harness builtins
	// that are neither file, command, search, web, agent, skill nor MCP
	// work — claude-code `StructuredOutput` / `SendUserFile` /
	// `Artifact` / `Workflow`, codex `imagegen`, copilot-cli
	// `report_intent`, the `<tool>.step_finish` turn markers. Extension
	// beyond the plan's six named new types; flagged for ratification in
	// WP-T4. It exists so these rows stop being indistinguishable from
	// genuinely unmapped tools in the `unknown` bucket.
	ActionHarnessCall = "harness_call"
)

// Canonical categories (plan §1). The first nine mirror
// web/src/lib/actions.ts CATEGORY_COLOR; "skill" is new and needs a
// --act-skill colour var when WP-T2 regenerates the TS side.
const (
	CategoryFile   = "file"
	CategoryCmd    = "cmd"
	CategorySearch = "search"
	CategoryWeb    = "web"
	CategoryAgent  = "agent"
	CategorySkill  = "skill"
	CategoryMCP    = "mcp"
	CategoryMeta   = "meta"
	CategoryFail   = "fail"
	CategoryUser   = "user"
)

// categoryOrder is the canonical display order of the categories —
// concrete work first, then coordination, then noise.
var categoryOrder = []string{
	CategoryFile, CategoryCmd, CategorySearch, CategoryWeb,
	CategoryAgent, CategorySkill, CategoryMCP,
	CategoryUser, CategoryMeta, CategoryFail,
}

// ActionTypeMeta is the display/aggregation metadata for one canonical
// action type — the Go counterpart of the TypeScript ACTION_REGISTRY
// (web/src/lib/actions.ts:34-107), which WP-T2 will generate from here.
type ActionTypeMeta struct {
	// Category is one of the ten canonical categories.
	Category string
	// Label is the human-readable display label.
	Label string
}

// actionTypes is the canonical action-type registry. Values for the
// action types that already exist in ACTION_REGISTRY are copied
// verbatim from it so WP-T2's generated mirror is a no-op diff for
// those rows.
var actionTypes = map[string]ActionTypeMeta{
	// file ops
	ActionReadFile:  {CategoryFile, "Read file"},
	ActionWriteFile: {CategoryFile, "Write file"},
	ActionEditFile:  {CategoryFile, "Edit file"},
	// commands
	ActionRunCommand: {CategoryCmd, "Run command"},
	// search
	ActionSearchText:  {CategorySearch, "Search text"},
	ActionSearchFiles: {CategorySearch, "Search files"},
	// web
	ActionWebSearch: {CategoryWeb, "Web search"},
	ActionWebFetch:  {CategoryWeb, "Web fetch"},
	// browser_action has no ACTION_REGISTRY row today (it falls through
	// to the TS "meta" fallback). Classified web here: it drives a
	// browser against the web, same dimension as web_fetch.
	ActionBrowserAction: {CategoryWeb, "Browser action"},
	// sub-agents / orchestration
	ActionSpawnSubagent: {CategoryAgent, "Spawn subagent"},
	ActionSubagentStart: {CategoryAgent, "Subagent start"},
	ActionSubagentStop:  {CategoryAgent, "Subagent stop"},
	ActionSubagentWait:  {CategoryAgent, "Wait for subagent"},
	ActionAgentMessage:  {CategoryAgent, "Agent message"},
	ActionAgentControl:  {CategoryAgent, "Agent control"},
	ActionStdinWrite:    {CategoryAgent, "Write stdin"},
	// skills
	ActionSkillInvoke: {CategorySkill, "Invoke skill"},
	// mcp
	ActionMCPCall: {CategoryMCP, "MCP call"},
	// user
	ActionUserPrompt:          {CategoryUser, "User prompt"},
	ActionUserPromptExpansion: {CategoryUser, "Prompt expansion"},
	ActionAskUser:             {CategoryUser, "Ask user"},
	// prompt scaffolding
	ActionSystemPrompt:  {CategoryMeta, "System prompt"},
	ActionPromptContext: {CategoryMeta, "Prompt context"},
	// meta / session
	ActionTaskComplete:       {CategoryMeta, "Task complete"},
	ActionPermissionRequest:  {CategoryMeta, "Permission request"},
	ActionPostToolBatch:      {CategoryMeta, "Post-tool batch"},
	ActionSetup:              {CategoryMeta, "Setup"},
	ActionInstructionsLoaded: {CategoryMeta, "Instructions loaded"},
	ActionConfigChange:       {CategoryMeta, "Config change"},
	ActionSessionStart:       {CategoryMeta, "Session start"},
	ActionSessionEnd:         {CategoryMeta, "Session end"},
	ActionNotification:       {CategoryMeta, "Notification"},
	ActionCwdChange:          {CategoryMeta, "CWD change"},
	ActionTodoUpdate:         {CategoryMeta, "Todo update"},
	ActionContextCompacted:   {CategoryMeta, "Context compacted"},
	ActionRateLimit:          {CategoryMeta, "Rate limit"},
	ActionTurnAborted:        {CategoryMeta, "Turn aborted"},
	// meta rows with no ACTION_REGISTRY entry today (TS falls back to
	// the humanized "meta" default, so these are drift-free additions).
	ActionAssistantMessage: {CategoryMeta, "Assistant message"},
	ActionPermissionMode:   {CategoryMeta, "Permission mode"},
	ActionWorktreeCreate:   {CategoryMeta, "Worktree create"},
	ActionWorktreeRemove:   {CategoryMeta, "Worktree remove"},
	ActionSchedule:         {CategoryMeta, "Schedule"},
	ActionToolSearch:       {CategoryMeta, "Tool search"},
	ActionHarnessCall:      {CategoryMeta, "Harness call"},
	ActionUnknown:          {CategoryMeta, "Unknown"},
	// failures
	ActionPermissionDenied: {CategoryFail, "Permission denied"},
	ActionToolFailure:      {CategoryFail, "Tool failure"},
	ActionAPIError:         {CategoryFail, "API error"},
}

// MetaForActionType returns the registry metadata for a canonical action
// type. ok is false for an unregistered type; callers that just need a
// category should use CategoryForActionType, which falls back to
// CategoryMeta the same way the dashboard does.
func MetaForActionType(actionType string) (ActionTypeMeta, bool) {
	m, ok := actionTypes[actionType]
	return m, ok
}

// CategoryForActionType returns the canonical category of an action
// type, falling back to CategoryMeta for anything unregistered —
// mirroring the dashboard's actionMeta() fallback so an action type
// added ahead of its registry row still renders sanely.
func CategoryForActionType(actionType string) string {
	if m, ok := actionTypes[actionType]; ok {
		return m.Category
	}
	return CategoryMeta
}

// ActionTypes returns every registered canonical action type, sorted.
func ActionTypes() []string {
	out := make([]string, 0, len(actionTypes))
	for k := range actionTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Categories returns the ten canonical categories in display order.
func Categories() []string {
	out := make([]string, len(categoryOrder))
	copy(out, categoryOrder)
	return out
}

// ActionTypesInCategory returns every registered action type whose
// canonical category is the requested one, sorted. An unknown category
// yields an empty slice (never nil-vs-empty ambiguity: callers build SQL
// IN-lists from this and an empty list must be an obvious no-op, not a
// silent match-everything).
//
// This is the accessor the Patterns engine (WP-T5) derives its action-type
// sets from, replacing the literal sets it used to inline in SQL.
func ActionTypesInCategory(category string) []string {
	out := make([]string, 0, 4)
	for at, m := range actionTypes {
		if m.Category == category {
			out = append(out, at)
		}
	}
	sort.Strings(out)
	return out
}
