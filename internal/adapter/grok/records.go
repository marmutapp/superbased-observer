package grok

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// acpLine is one JSONL line of a grok session's updates.jsonl stream. Grok
// speaks the Agent Client Protocol (ACP): every line is a JSON-RPC
// notification whose `method` is "session/update" (or the xAI vendor
// extension "_x.ai/session/update") and whose payload is under `params`.
type acpLine struct {
	Timestamp int64     `json:"timestamp"`
	Method    string    `json:"method"`
	Params    acpParams `json:"params"`
}

// acpParams is the ACP notification payload: the owning session id, the
// discriminated `update` body, and an outer `_meta` carrying the stable
// event id + cumulative token watermark + the wall-clock timestamp.
type acpParams struct {
	SessionID string       `json:"sessionId"`
	Update    acpUpdate    `json:"update"`
	Meta      acpOuterMeta `json:"_meta"`
}

// acpUpdate is the discriminated session-update body. `sessionUpdate`
// selects the variant; the remaining fields are a superset across all
// observed variants (user_message_chunk / agent_thought_chunk /
// agent_message_chunk / tool_call / tool_call_update / hook_execution /
// turn_completed), so a single struct decodes every shape and unused
// fields stay zero.
type acpUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Content       *acpContent     `json:"content"`
	ToolCallID    string          `json:"toolCallId"`
	Title         string          `json:"title"`
	Status        string          `json:"status"`
	RawInput      json.RawMessage `json:"rawInput"`
	RawOutput     json.RawMessage `json:"rawOutput"`
	StopReason    string          `json:"stop_reason"`
	PromptID      string          `json:"prompt_id"`
	Meta          *acpUpdateMeta  `json:"_meta"`
}

// acpContent is a message chunk's content block. Grok persists each chunk
// pre-assembled (one chunk carries the whole message text), so a variant
// that observes multiple chunks per turn simply emits multiple rows.
type acpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// acpUpdateMeta is the inner (update-scoped) `_meta`. The per-turn model
// id rides here on user_message_chunk records.
type acpUpdateMeta struct {
	ModelID string `json:"modelId"`
}

// acpOuterMeta is the params-scoped `_meta`. eventId is grok's stable,
// monotonically-suffixed identifier (`<sessionId>-<n>`) used verbatim as
// the deterministic SourceEventID; agentTimestampMs is the millisecond
// wall-clock; totalTokens is the cumulative session watermark (decoded
// for cross-check only — the accurate per-request splits come from
// unified.jsonl, see unifiedLine).
type acpOuterMeta struct {
	TotalTokens      int64  `json:"totalTokens"`
	EventID          string `json:"eventId"`
	AgentTimestampMs int64  `json:"agentTimestampMs"`
	PromptID         string `json:"promptId"`
}

// sessionSummary is the session bundle's summary.json — the project-root
// + model + branch seam. git_root_dir is the primary project-root signal
// (already a git worktree root); info.cwd is the raw OS working directory
// (decoded from the percent-encoded parent dir name) used as the
// fallback. current_model_id is the session's model.
type sessionSummary struct {
	Info struct {
		ID  string `json:"id"`
		Cwd string `json:"cwd"`
	} `json:"info"`
	CurrentModelID  string `json:"current_model_id"`
	GitRootDir      string `json:"git_root_dir"`
	HeadBranch      string `json:"head_branch"`
	ReasoningEffort string `json:"reasoning_effort"`
	AgentName       string `json:"agent_name"`
}

// unifiedLine is one JSONL line of the GLOBAL ~/.grok/logs/unified.jsonl
// diagnostic log. The `shell.turn.inference_done` lines carry the accurate
// per-request token splits AND a `sid` (session id) correlation key, so
// they are attributed to the owning session without any timestamp
// heuristics. Every other msg value is skipped.
type unifiedLine struct {
	Ts  string     `json:"ts"`
	Sid string     `json:"sid"`
	Msg string     `json:"msg"`
	Ctx unifiedCtx `json:"ctx"`
}

// unifiedCtx is the per-inference token block. prompt_tokens is GROSS
// (it INCLUDES cached_prompt_tokens — OpenAI convention, verified live
// 2026-07-09: prompt=26346 with cached=25984 ⊂ prompt), so the net
// non-cached input is prompt−cached. reasoning_tokens bill at the output
// rate (mapped to TokenEvent.ReasoningTokens).
type unifiedCtx struct {
	LoopIndex          int64 `json:"loop_index"`
	PromptTokens       int64 `json:"prompt_tokens"`
	CachedPromptTokens int64 `json:"cached_prompt_tokens"`
	CompletionTokens   int64 `json:"completion_tokens"`
	ReasoningTokens    int64 `json:"reasoning_tokens"`
}

const inferenceDoneMsg = "shell.turn.inference_done"

// tokenParts is the net-token view derived from an inference_done record.
type tokenParts struct {
	inputNet  int64
	output    int64
	cacheRead int64
	reasoning int64
}

// tokenBundle nets grok's GROSS prompt_tokens into the cost engine's
// NET-non-cached input contract: input = prompt−cached (clamped ≥0),
// cache read = cached, reasoning carried separately (billed at the output
// rate by the cost engine).
func tokenBundle(c unifiedCtx) tokenParts {
	net := c.PromptTokens - c.CachedPromptTokens
	if net < 0 {
		net = 0
	}
	return tokenParts{
		inputNet:  net,
		output:    c.CompletionTokens,
		cacheRead: c.CachedPromptTokens,
		reasoning: c.ReasoningTokens,
	}
}

// mapToolName collapses grok's tool vocabulary onto the normalized action
// set. Matching is case-insensitive with `_`/`-` stripped so both grok's
// snake_case names and any camelCase variant resolve. The raw name is
// preserved on ToolEvent.RawToolName. An unrecognised name maps to
// ActionUnknown; MCP-routed tools (namespaced with `__` or an `mcp`
// prefix) map to ActionMCPCall. The grok tool taxonomy is mapped
// DEFENSIVELY: the operator's live capture used the read-only
// grok-build-plan agent, so write/edit/run tool-exec shapes are mapped
// from grok's documented vocabulary and fall through honestly.
func mapToolName(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	switch key {
	case "readfile", "read", "view", "viewfile", "cat":
		return models.ActionReadFile
	case "writefile", "write", "createfile", "create", "newfile":
		return models.ActionWriteFile
	case "editfile", "edit", "replace", "applypatch", "patch", "strreplace", "multiedit":
		return models.ActionEditFile
	case "runterminalcommand", "runcommand", "terminal", "bash", "shell", "exec",
		"execute", "cmd", "powershell", "pwsh":
		return models.ActionRunCommand
	case "grep", "searchtext", "ripgrep", "codesearch", "findtext", "search":
		return models.ActionSearchText
	case "listdir", "listdirectory", "glob", "findfiles", "filesearch", "ls", "readfolder":
		return models.ActionSearchFiles
	case "websearch", "search_web", "browsersearch":
		return models.ActionWebSearch
	case "webfetch", "fetch", "fetchurl", "readurl", "browse", "openurl":
		return models.ActionWebFetch
	case "task", "subagent", "spawnagent", "dispatchagent", "delegate", "agent":
		return models.ActionSpawnSubagent
	case "todowrite", "todo", "todoupdate", "updateplan", "planupdate":
		return models.ActionTodoUpdate
	default:
		if strings.HasPrefix(key, "mcp") || strings.Contains(name, "__") {
			return models.ActionMCPCall
		}
		return models.ActionUnknown
	}
}

// targetFromRawInput picks a representative target from a tool_call's raw
// input JSON, trying common path/command/query keys and falling back to
// the tool name.
func targetFromRawInput(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fallback
	}
	for _, key := range []string{
		"target_file", "file_path", "filePath", "absolute_path", "absolutePath",
		"path", "file", "command", "query", "url", "pattern", "prompt", "directory",
	} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fallback
}

// rawOutputText extracts a human-readable string from a tool_call_update's
// rawOutput object. Grok wraps tool output as {type, Content:{content,
// absolute_root_path}} (and other tool-specific shapes); a nested
// `content` string one level down is preferred, then a top-level string
// value, else the whole object's JSON is returned so nothing is silently
// dropped.
func rawOutputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return string(raw)
	}
	// A one-level-nested {"content":"..."} (grok's read/list wrapper).
	for _, v := range top {
		var nested struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(v, &nested) == nil && strings.TrimSpace(nested.Content) != "" {
			return nested.Content
		}
	}
	// A top-level string value (e.g. {"output":"..."}).
	for _, key := range []string{"content", "output", "text", "stdout"} {
		if v, ok := top[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return string(raw)
}

// msToTime converts a millisecond epoch to a UTC time. Returns the zero
// time for a non-positive value.
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// parseUnifiedTS decodes unified.jsonl's RFC3339 millisecond timestamps.
// Returns the zero time on an empty or unparseable value.
func parseUnifiedTS(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// truncate caps a preview string to n bytes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
