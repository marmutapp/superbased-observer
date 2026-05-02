package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter parses OpenAI Codex CLI rollout JSONL files under
// ~/.codex/sessions/rollout-*.jsonl. See spec §4.2.
//
// The rollout format is event-based: session_configured / user_message /
// agent_message / tool_call / tool_output / token_count records. This
// adapter extracts tool_call + tool_output pairs into normalized ToolEvents
// and token_count events into TokenEvents.
type Adapter struct {
	scrubber  *scrub.Scrubber
	watchRoot string
}

// New returns a Codex adapter with defaults.
func New() *Adapter {
	return &Adapter{scrubber: scrub.New()}
}

// NewWithOptions customizes the scrubber and/or watch root.
func NewWithOptions(s *scrub.Scrubber, watchRoot string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	return &Adapter{scrubber: s, watchRoot: watchRoot}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolCodex }

// WatchPaths returns the canonical Codex sessions directory. Honors
// CODEX_HOME when set (single explicit path — cross-mount expansion
// is suppressed because the env var is the user telling us exactly
// where to look). Otherwise expands to ".codex/sessions" under every
// cross-mount-resolved $HOME so observer in WSL2 picks up sessions
// from /mnt/c/Users/<u>/.codex (and vice-versa).
func (a *Adapter) WatchPaths() []string {
	if a.watchRoot != "" {
		return []string{a.watchRoot}
	}
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return []string{filepath.Join(home, "sessions")}
	}
	var roots []string
	for _, h := range crossmount.AllHomes() {
		roots = append(roots, filepath.Join(h.Path, ".codex", "sessions"))
	}
	return roots
}

// IsSessionFile matches rollout-*.jsonl files.
func (*Adapter) IsSessionFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") && filepath.Ext(base) == ".jsonl"
}

// actionMap translates Codex tool names to the normalized taxonomy.
// Synonyms and search/list tool names added per audit C2 — Codex
// historically captured only the five core tools and silently routed
// everything else through ActionUnknown.
var actionMap = map[string]string{
	// Core tools
	"shell":       models.ActionRunCommand,
	"apply_patch": models.ActionEditFile,
	"file_read":   models.ActionReadFile,
	"file_write":  models.ActionWriteFile,
	"web_search":  models.ActionWebSearch,
	// Synonyms observed in newer Codex builds and IDE extensions
	"exec":           models.ActionRunCommand,
	"execute":        models.ActionRunCommand,
	"command":        models.ActionRunCommand,
	"read_file":      models.ActionReadFile,
	"open_file":      models.ActionReadFile,
	"write_file":     models.ActionWriteFile,
	"create_file":    models.ActionWriteFile,
	"edit_file":      models.ActionEditFile,
	"patch":          models.ActionEditFile,
	"replace":        models.ActionEditFile,
	"search":         models.ActionSearchText,
	"grep":           models.ActionSearchText,
	"find_text":      models.ActionSearchText,
	"find_in_files":  models.ActionSearchText,
	"file_search":    models.ActionSearchFiles,
	"find":           models.ActionSearchFiles,
	"glob":           models.ActionSearchFiles,
	"list_files":     models.ActionSearchFiles,
	"list_directory": models.ActionSearchFiles,
	"web_fetch":      models.ActionWebFetch,
	"fetch_url":      models.ActionWebFetch,
	// Function-call names emitted via response_item.payload.type=function_call
	// in current Codex Desktop builds. shell_command is the by-far dominant
	// one (~95% of function_calls in real sessions); update_plan is Codex's
	// structured todo planner; list_mcp_resources / list_mcp_resource_templates
	// are MCP discovery calls; view_image is image-file reading.
	"shell_command":               models.ActionRunCommand,
	"update_plan":                 models.ActionTodoUpdate,
	"list_mcp_resources":          models.ActionMCPCall,
	"list_mcp_resource_templates": models.ActionMCPCall,
	"view_image":                  models.ActionReadFile,
}

// rawLine is the top-level envelope; payload is decoded per type.
type rawLine struct {
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// sessionContext is payload for session_configured / session_start events —
// we cache cwd + model + branch for the whole file.
type sessionContext struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	TurnID    string `json:"turn_id"`
	Model     string `json:"model"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"git_branch"`
}

type payloadEnvelope struct {
	Type string `json:"type"`
}

type userMessage struct {
	Message string `json:"message"`
}

// sessionMetaPayload extends sessionContext with the base_instructions
// system prompt the runtime baked into the conversation. The text is
// large (18KB+ in observed corpora) so the adapter hash-dedups across
// the parse to avoid emitting one row per session_meta replay.
type sessionMetaPayload struct {
	sessionContext
	BaseInstructions struct {
		Text string `json:"text"`
	} `json:"base_instructions"`
}

// turnContextPayload extends sessionContext with developer_instructions —
// per-turn system-prompt-shaped overrides. In observed corpora this is
// 9KB+ and ALMOST ALWAYS identical across turns within a session, so
// hash dedup makes the difference between O(turns) and O(1) rows per
// session.
type turnContextPayload struct {
	sessionContext
	DeveloperInstructions string `json:"developer_instructions"`
}

// responseItemMessage covers response_item.payload when payload.type ==
// "message". Role discriminates assistant / user / developer; only
// developer-role messages route to ActionSystemPrompt (assistant +
// user are already covered by event_msg/agent_message and event_msg/
// user_message respectively, and re-emitting them here would
// double-count).
type responseItemMessage struct {
	Role    string                       `json:"role"`
	Content []responseItemMessageContent `json:"content"`
}

type responseItemMessageContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// agentMessage is the assistant's natural-language preamble that
// introduces a turn's tool work. Codex emits one or more of these
// per turn (`event_msg` payload type "agent_message"), interleaved
// with tool_call / function_call events. We capture them per-turn
// and propagate as PrecedingReasoning on every tool_call /
// exec_command_end / web_search_end that follows in the same turn,
// matching how claudecode threads assistant text through to its
// tool events.
type agentMessage struct {
	TurnID  string `json:"turn_id"`
	Message string `json:"message"`
}

type taskStarted struct {
	TurnID string `json:"turn_id"`
}

type taskComplete struct {
	TurnID           string `json:"turn_id"`
	LastAgentMessage string `json:"last_agent_message"`
	CompletedAt      int64  `json:"completed_at"`
	DurationMs       int64  `json:"duration_ms"`
}

// turnAborted is event_msg.payload for type="turn_aborted" — a turn
// interrupted before the model finishes generating (typically user
// pressed esc / cancelled). Same completed_at + duration_ms shape as
// taskComplete plus a `reason` discriminator (observed: "interrupted").
type turnAborted struct {
	TurnID      string `json:"turn_id"`
	Reason      string `json:"reason"`
	CompletedAt int64  `json:"completed_at"`
	DurationMs  int64  `json:"duration_ms"`
}

type execCommandEnd struct {
	CallID           string          `json:"call_id"`
	TurnID           string          `json:"turn_id"`
	Command          json.RawMessage `json:"command"`
	Cwd              string          `json:"cwd"`
	AggregatedOutput string          `json:"aggregated_output"`
	Stdout           string          `json:"stdout"`
	Stderr           string          `json:"stderr"`
	ExitCode         int             `json:"exit_code"`
	Duration         struct {
		Secs  int64 `json:"secs"`
		Nanos int64 `json:"nanos"`
	} `json:"duration"`
	Status string `json:"status"`
}

type webSearchEnd struct {
	CallID string `json:"call_id"`
	TurnID string `json:"turn_id"`
	Query  string `json:"query"`
	Action struct {
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
	} `json:"action"`
}

// responseItemReasoning is response_item.payload when payload.type ==
// "reasoning". The `summary` array MAY contain text segments
// {type:"summary_text"|"text", text:"..."} in future Codex builds; the
// `encrypted_content` field is opaque and not extractable. Adapter
// extracts whatever readable text is present and threads it through
// the turn's agentMessages cache for downstream PrecedingReasoning.
type responseItemReasoning struct {
	Summary []reasoningSummaryPart `json:"summary"`
}

type reasoningSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// mcpToolCallEnd is event_msg.payload for type="mcp_tool_call_end" —
// the executor result for an MCP tool call (typically paired with a
// response_item.function_call(list_mcp_resources*) intent emitted
// earlier in the same turn). The `invocation` block carries
// server/tool/arguments; `result` is a tagged-union {Ok|Err} where Ok
// carries content[*].text + isError, Err carries the failure message.
type mcpToolCallEnd struct {
	CallID     string        `json:"call_id"`
	TurnID     string        `json:"turn_id"`
	Invocation mcpInvocation `json:"invocation"`
	Duration   codexDuration `json:"duration"`
	Result     mcpCallResult `json:"result"`
}

type mcpInvocation struct {
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type codexDuration struct {
	Secs  int64 `json:"secs"`
	Nanos int64 `json:"nanos"`
}

type mcpCallResult struct {
	Ok  *mcpCallResultOk  `json:"Ok"`
	Err *mcpCallResultErr `json:"Err"`
}

type mcpCallResultOk struct {
	Content []mcpCallContent `json:"content"`
	IsError bool             `json:"isError"`
}

type mcpCallContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResultErr struct {
	Message string `json:"message"`
}

// compactedEvent is the top-level type="compacted" event Codex emits
// when the model decides to summarize earlier turns. The payload
// carries `message` (the runtime-substituted summary text) and
// `replacement_history` (the array of messages that got compacted
// away). Per user direction (2026-05-01): capture token/event
// information but do NOT make these rows searchable like file edits.
// One ActionContextCompacted row per event records msg-count + byte/
// token estimate so cost-analysis and compaction-frequency dashboards
// pick them up without polluting the file-edit browser.
type compactedEvent struct {
	Message            string                 `json:"message"`
	ReplacementHistory []compactedHistoryItem `json:"replacement_history"`
}

type compactedHistoryItem struct {
	Role    string                  `json:"role"`
	Content []compactedContentBlock `json:"content"`
}

type compactedContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// dynamicToolCallRequest is event_msg.payload for type=
// "dynamic_tool_call_request" — Codex's runtime-loaded tool invocation
// (e.g. load_workspace_dependencies). Note: this event uses camelCase
// `callId`/`turnId` field names, unlike the snake_case used elsewhere
// in event_msg payloads (the response variant uses snake_case). Both
// forms must be tolerated.
type dynamicToolCallRequest struct {
	CallID    string          `json:"callId"`
	CallIDAlt string          `json:"call_id"`
	TurnID    string          `json:"turnId"`
	TurnIDAlt string          `json:"turn_id"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

func (d dynamicToolCallRequest) callID() string { return firstNonEmpty(d.CallID, d.CallIDAlt) }
func (d dynamicToolCallRequest) turnID() string { return firstNonEmpty(d.TurnID, d.TurnIDAlt) }

// dynamicToolCallResponse is event_msg.payload for type=
// "dynamic_tool_call_response" — the executor-side result. Field
// names are snake_case in observed payloads, but we accept the
// camelCase form too for robustness.
type dynamicToolCallResponse struct {
	CallID       string                `json:"call_id"`
	CallIDAlt    string                `json:"callId"`
	TurnID       string                `json:"turn_id"`
	TurnIDAlt    string                `json:"turnId"`
	Tool         string                `json:"tool"`
	Arguments    json.RawMessage       `json:"arguments"`
	ContentItems []dynamicToolCallItem `json:"content_items"`
	Success      bool                  `json:"success"`
	Error        string                `json:"error"`
	Duration     codexDuration         `json:"duration"`
}

func (d dynamicToolCallResponse) callID() string { return firstNonEmpty(d.CallID, d.CallIDAlt) }
func (d dynamicToolCallResponse) turnID() string { return firstNonEmpty(d.TurnID, d.TurnIDAlt) }

type dynamicToolCallItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// viewImageToolCall is event_msg.payload for type="view_image_tool_call"
// — the executor side-channel for Codex's view_image function tool.
// Carries the resolved file path (the response_item.function_call's
// arguments do too, but this event lands post-resolution and is
// authoritative when the call resolves through a layer that rewrites
// paths).
type viewImageToolCall struct {
	CallID string `json:"call_id"`
	TurnID string `json:"turn_id"`
	Path   string `json:"path"`
}

// codexError is event_msg.payload for type="error" — upstream API
// failures the rollout writes when a turn cannot complete (usage limit,
// rate limit, content-policy, malformed-request, etc.). Mirrors
// claudecode's ActionAPIError capture; pre-v1.4.21 these were silently
// dropped because the adapter only knew the structured success-path
// event types.
type codexError struct {
	Message        string `json:"message"`
	CodexErrorInfo string `json:"codex_error_info"`
}

type toolCall struct {
	CallID string          `json:"call_id"`
	ID     string          `json:"id"` // some Codex builds use "id" rather than "call_id"
	Tool   string          `json:"tool"`
	Name   string          `json:"name"` // newer builds use "name"
	Input  json.RawMessage `json:"input"`
}

type toolOutput struct {
	CallID  string          `json:"call_id"`
	ID      string          `json:"id"`
	Output  json.RawMessage `json:"output"`
	Success *bool           `json:"success"`
	IsError *bool           `json:"is_error"`
}

// responseItemFunctionCall is response_item.payload when payload.type ==
// "function_call". This is the assistant-side tool intent, emitted before
// the corresponding executor side-channel (event_msg/exec_command_end for
// shell_command, event_msg/web_search_end for web_search_call,
// event_msg/patch_apply_end for the apply_patch custom tool). The
// `arguments` field is a JSON-string-encoded object — unwrap once.
type responseItemFunctionCall struct {
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
}

// responseItemFunctionCallOutput is response_item.payload when payload.type
// == "function_call_output". The output field is a string (often itself
// JSON-shaped) and lacks success/is_error metadata — when only this side
// of the pair is seen, we can attach the body but cannot infer success.
type responseItemFunctionCallOutput struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// responseItemCustomToolCall is response_item.payload when payload.type ==
// "custom_tool_call". In current Codex Desktop builds this is exclusively
// the `apply_patch` tool — input carries the raw patch text (not JSON),
// and the matching event_msg/patch_apply_end carries the structured
// `changes` map plus stdout/stderr/success.
type responseItemCustomToolCall struct {
	Status string `json:"status"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

// responseItemCustomToolCallOutput is the matching output: a single
// string field that's typically itself a JSON object
// {"output":"...","metadata":{"exit_code":0,...}}.
type responseItemCustomToolCallOutput struct {
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// patchApplyEnd is event_msg.payload for type="patch_apply_end" — the
// executor-side result for an apply_patch custom_tool_call. `changes` is
// a map of absolute path → {type, content} for each file the patch
// touched. We only use the file paths and overall success in Tier 1.
type patchApplyEnd struct {
	CallID  string                      `json:"call_id"`
	TurnID  string                      `json:"turn_id"`
	Stdout  string                      `json:"stdout"`
	Stderr  string                      `json:"stderr"`
	Success bool                        `json:"success"`
	Changes map[string]patchApplyChange `json:"changes"`
}

type patchApplyChange struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

type tokenCount struct {
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	Cached       int64  `json:"cached_input_tokens"`
	Reasoning    int64  `json:"reasoning_tokens"`
	Model        string `json:"model"`
}

type modernTokenCount struct {
	Info struct {
		LastTokenUsage  tokenUsage `json:"last_token_usage"`
		TotalTokenUsage tokenUsage `json:"total_token_usage"`
	} `json:"info"`
}

type tokenUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	ReasoningTokens   int64 `json:"reasoning_output_tokens"`
}

// ParseSessionFile implements adapter.Adapter.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("codex.ParseSessionFile: open %s: %w", path, err)
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("codex.ParseSessionFile: seek: %w", err)
		}
	}
	res := adapter.ParseResult{NewOffset: fromOffset}

	// Fall back to the filename stem as session id if no session_configured
	// event has landed yet (e.g. incremental parse starting mid-file).
	ctxState := sessionContext{SessionID: sessionIDFromPath(path)}
	rootCache := map[string]string{}
	pending := map[string]int{}         // call_id → res.ToolEvents index
	lastInputByID := map[string]int64{} // token_count counts are cumulative
	turnModels := map[string]string{}
	pendingToolModels := map[string][]int{}
	pendingTokenModels := map[string][]int{}
	pendingUserPromptIdx := -1
	pendingTurnlessTokenIdxs := []int{}
	// agentMessages caches the latest assistant preamble per turn so
	// every tool_call / exec_command_end / web_search_end inside that
	// turn picks it up as PrecedingReasoning. Keyed by turn_id; entries
	// stay around for the whole parse since one turn's preamble is
	// only valid for that turn's tool events.
	agentMessages := map[string]string{}
	// seenSystemPrompts dedups ActionSystemPrompt emissions across the
	// parse. Keyed by content hash (shortHash of the prompt body).
	// Codex repeats base_instructions in every session_meta and
	// developer_instructions in nearly every turn_context — without
	// dedup we'd emit 9KB+ rows N times per session.
	seenSystemPrompts := map[string]bool{}
	// seenModernTotal tracks the most recent total_token_usage per
	// session for the modern event_msg/token_count path. Codex
	// re-emits identical token_count records (same last_token_usage
	// AND total_token_usage) periodically — observed in real corpora
	// at lines 134/129 and 171/165 of one inspected rollout (user
	// reported 2026-05-01). The total is monotonic, so any new event
	// whose total matches a previously seen total is a re-emission;
	// summing both inflates session-wide token counts. Per-session map
	// keyed by SessionID; tokenUsage is a value-type struct so == is
	// correct.
	seenModernTotal := map[string]tokenUsage{}

	applyContext := func(sc sessionContext) {
		if sc.ID != "" {
			ctxState.SessionID = sc.ID
		}
		if sc.SessionID != "" {
			ctxState.SessionID = sc.SessionID
		}
		if sc.TurnID != "" {
			ctxState.TurnID = sc.TurnID
		}
		if sc.Model != "" {
			ctxState.Model = sc.Model
		}
		if sc.Cwd != "" {
			ctxState.Cwd = sc.Cwd
		}
		if sc.GitBranch != "" {
			ctxState.GitBranch = sc.GitBranch
		}
		if ctxState.TurnID == "" || ctxState.Model == "" {
			return
		}
		turnModels[ctxState.TurnID] = ctxState.Model
		for _, idx := range pendingToolModels[ctxState.TurnID] {
			if res.ToolEvents[idx].Model == "" {
				res.ToolEvents[idx].Model = ctxState.Model
			}
		}
		delete(pendingToolModels, ctxState.TurnID)
		for _, idx := range pendingTokenModels[ctxState.TurnID] {
			if res.TokenEvents[idx].Model == "" {
				res.TokenEvents[idx].Model = ctxState.Model
			}
		}
		delete(pendingTokenModels, ctxState.TurnID)
		if len(pendingTurnlessTokenIdxs) > 0 {
			for _, idx := range pendingTurnlessTokenIdxs {
				if res.TokenEvents[idx].MessageID == "" {
					res.TokenEvents[idx].MessageID = ctxState.TurnID
				}
				if res.TokenEvents[idx].Model == "" {
					res.TokenEvents[idx].Model = ctxState.Model
				}
			}
			pendingTurnlessTokenIdxs = nil
		}
		if pendingUserPromptIdx >= 0 && pendingUserPromptIdx < len(res.ToolEvents) {
			if res.ToolEvents[pendingUserPromptIdx].ActionType == models.ActionUserPrompt {
				res.ToolEvents[pendingUserPromptIdx].MessageID = "user:" + ctxState.TurnID
				if res.ToolEvents[pendingUserPromptIdx].Model == "" {
					res.ToolEvents[pendingUserPromptIdx].Model = ctxState.Model
				}
			}
			pendingUserPromptIdx = -1
		}
	}

	assistantTurnID := func(explicitTurnID string) string {
		return firstNonEmpty(explicitTurnID, ctxState.TurnID)
	}

	userMessageID := func(message string, lineNum int) string {
		if turnID := assistantTurnID(""); turnID != "" {
			return "user:" + turnID
		}
		return fmt.Sprintf("user:%s:L%d:%s", filepath.Base(path), lineNum, shortHash(strings.TrimSpace(message)))
	}

	modelForTurn := func(turnID string) string {
		return firstNonEmpty(turnModels[turnID], ctxState.Model)
	}

	scanner := bufio.NewScanner(f)
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	var bytesRead int64 = fromOffset
	lineNum := 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw := scanner.Bytes()
		lineBytes := int64(len(raw) + 1)
		bytesRead += lineBytes
		lineNum++

		if len(raw) == 0 {
			continue
		}
		var line rawLine
		if err := json.Unmarshal(raw, &line); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
			continue
		}
		res.NewOffset = bytesRead

		ts := parseTimestamp(line.Timestamp)
		payloadType := payloadType(line.Payload)

		switch line.Type {
		case "compacted":
			// Top-level type="compacted" event — emit one
			// ActionContextCompacted row carrying the message count and
			// byte estimate from replacement_history. The paired
			// event_msg/context_compacted (which has no payload) is a
			// marker for the same event; we no-op that to avoid
			// double-emission.
			var ce compactedEvent
			if err := json.Unmarshal(line.Payload, &ce); err != nil {
				continue
			}
			projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
			evt := a.buildCompactedEvent(path, ctxState, projectRoot, ts, ce, lineNum)
			if evt.Model == "" {
				if turnID := assistantTurnID(""); turnID != "" {
					evt.Model = modelForTurn(turnID)
					if evt.Model == "" {
						pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
					}
				}
			}
			res.ToolEvents = append(res.ToolEvents, evt)

		case "session_meta":
			var meta sessionMetaPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				applyContext(meta.sessionContext)
				if body := strings.TrimSpace(meta.BaseInstructions.Text); body != "" {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					if evt, ok := a.systemPromptEvent(path, "base", body, ts, ctxState, projectRoot, lineNum, seenSystemPrompts); ok {
						res.ToolEvents = append(res.ToolEvents, evt)
					}
				}
			}

		case "session_configured", "session_start", "turn_context":
			var meta turnContextPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				applyContext(meta.sessionContext)
				if body := strings.TrimSpace(meta.DeveloperInstructions); body != "" {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					if evt, ok := a.systemPromptEvent(path, "developer", body, ts, ctxState, projectRoot, lineNum, seenSystemPrompts); ok {
						res.ToolEvents = append(res.ToolEvents, evt)
					}
				}
			}

		case "event_msg":
			switch payloadType {
			case "task_started":
				var started taskStarted
				if err := json.Unmarshal(line.Payload, &started); err == nil && started.TurnID != "" {
					applyContext(sessionContext{TurnID: started.TurnID})
				}
			case "agent_message":
				var am agentMessage
				if err := json.Unmarshal(line.Payload, &am); err == nil {
					turnID := firstNonEmpty(am.TurnID, ctxState.TurnID)
					if turnID != "" {
						msg := strings.TrimSpace(am.Message)
						if msg != "" {
							agentMessages[turnID] = msg
						}
					}
				}
			case "user_message":
				var um userMessage
				if err := json.Unmarshal(line.Payload, &um); err == nil {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					evt := a.buildUserPromptEvent(path, ctxState, projectRoot, ts, lineNum, um.Message)
					evt.MessageID = userMessageID(um.Message, lineNum)
					res.ToolEvents = append(res.ToolEvents, evt)
					if ctxState.TurnID == "" {
						pendingUserPromptIdx = len(res.ToolEvents) - 1
					}
				}
			case "exec_command_end":
				var ex execCommandEnd
				if err := json.Unmarshal(line.Payload, &ex); err == nil {
					if ex.TurnID != "" {
						applyContext(sessionContext{TurnID: ex.TurnID})
					}
					projectRoot := a.resolveProjectRoot(firstNonEmpty(ex.Cwd, ctxState.Cwd), rootCache)
					preceding := agentMessages[firstNonEmpty(ex.TurnID, ctxState.TurnID)]
					if idx, ok := pending[ex.CallID]; ok && idx < len(res.ToolEvents) {
						mergeExecIntoPending(&res.ToolEvents[idx], a, ex)
						delete(pending, ex.CallID)
					} else {
						evt := a.buildExecCommandEvent(path, ctxState, projectRoot, ts, ex, preceding)
						if evt.Model == "" {
							if turnID := assistantTurnID(ex.TurnID); turnID != "" {
								evt.Model = modelForTurn(turnID)
								if evt.Model == "" {
									pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
								}
							}
						}
						res.ToolEvents = append(res.ToolEvents, evt)
					}
				}
			case "web_search_end":
				var ws webSearchEnd
				if err := json.Unmarshal(line.Payload, &ws); err == nil {
					if ws.TurnID != "" {
						applyContext(sessionContext{TurnID: ws.TurnID})
					}
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					preceding := agentMessages[firstNonEmpty(ws.TurnID, ctxState.TurnID)]
					if idx, ok := pending[ws.CallID]; ok && idx < len(res.ToolEvents) {
						mergeWebSearchIntoPending(&res.ToolEvents[idx], ws)
						delete(pending, ws.CallID)
					} else {
						evt := a.buildWebSearchEvent(path, ctxState, projectRoot, ts, ws, lineNum, preceding)
						if evt.Model == "" {
							if turnID := assistantTurnID(ws.TurnID); turnID != "" {
								evt.Model = modelForTurn(turnID)
								if evt.Model == "" {
									pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
								}
							}
						}
						res.ToolEvents = append(res.ToolEvents, evt)
					}
				}
			case "context_compacted":
				// Marker-only event paired with a top-level type="compacted"
				// in the same line range. No-op here — the top-level event
				// carries the data and emits the row.
			case "dynamic_tool_call_request":
				var dr dynamicToolCallRequest
				if err := json.Unmarshal(line.Payload, &dr); err != nil {
					continue
				}
				callID := firstNonEmpty(dr.callID(), fmt.Sprintf("%s:L%d", filepath.Base(path), lineNum))
				if dr.turnID() != "" {
					applyContext(sessionContext{TurnID: dr.turnID()})
				}
				if _, dupe := pending[callID]; dupe {
					continue
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				preceding := agentMessages[ctxState.TurnID]
				evt := a.buildToolEvent(path, callID, ctxState, projectRoot, ts, dr.Tool, dr.Arguments, preceding)
				evt.RawToolName = "dynamic_tool_call_request"
				if evt.Model == "" {
					if turnID := assistantTurnID(""); turnID != "" {
						evt.Model = modelForTurn(turnID)
						if evt.Model == "" {
							pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
						}
					}
				}
				pending[callID] = len(res.ToolEvents)
				res.ToolEvents = append(res.ToolEvents, evt)
			case "dynamic_tool_call_response":
				var dp dynamicToolCallResponse
				if err := json.Unmarshal(line.Payload, &dp); err != nil {
					continue
				}
				idx, ok := pending[dp.callID()]
				if !ok || idx >= len(res.ToolEvents) {
					continue
				}
				row := &res.ToolEvents[idx]
				body := dynamicToolCallBody(dp.ContentItems)
				row.ToolOutput = a.scrubber.String(body)
				row.Success = dp.Success
				if !dp.Success {
					row.ErrorMessage = truncate(firstNonEmpty(dp.Error, body), 2048)
				}
				row.DurationMs = dp.Duration.Secs*1000 + dp.Duration.Nanos/1_000_000
				row.RawToolName = "dynamic_tool_call_response"
				delete(pending, dp.callID())
			case "view_image_tool_call":
				var vi viewImageToolCall
				if err := json.Unmarshal(line.Payload, &vi); err != nil {
					continue
				}
				if vi.TurnID != "" {
					applyContext(sessionContext{TurnID: vi.TurnID})
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				preceding := agentMessages[firstNonEmpty(vi.TurnID, ctxState.TurnID)]
				targetPath := vi.Path
				if targetPath != "" && projectRoot != "" {
					targetPath = git.RelativePath(projectRoot, targetPath)
				}
				if idx, ok := pending[vi.CallID]; ok && idx < len(res.ToolEvents) {
					row := &res.ToolEvents[idx]
					row.ActionType = models.ActionReadFile
					if targetPath != "" {
						row.Target = truncate(targetPath, 200)
					}
					row.RawToolName = "view_image_tool_call"
					delete(pending, vi.CallID)
				} else {
					evt := models.ToolEvent{
						SourceFile:         path,
						SourceEventID:      firstNonEmpty(vi.CallID, fmt.Sprintf("view_image:%s:L%d", filepath.Base(path), lineNum)),
						SessionID:          ctxState.SessionID,
						ProjectRoot:        projectRoot,
						Timestamp:          ts,
						GitBranch:          ctxState.GitBranch,
						Model:              ctxState.Model,
						Tool:               models.ToolCodex,
						ActionType:         models.ActionReadFile,
						Target:             truncate(targetPath, 200),
						Success:            true,
						PrecedingReasoning: truncate(preceding, 500),
						RawToolName:        "view_image_tool_call",
						MessageID:          firstNonEmpty(vi.TurnID, ctxState.TurnID),
					}
					if evt.Model == "" {
						if turnID := assistantTurnID(vi.TurnID); turnID != "" {
							evt.Model = modelForTurn(turnID)
							if evt.Model == "" {
								pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
							}
						}
					}
					res.ToolEvents = append(res.ToolEvents, evt)
				}
			case "turn_aborted":
				var ta turnAborted
				if err := json.Unmarshal(line.Payload, &ta); err != nil {
					continue
				}
				if ta.TurnID != "" {
					applyContext(sessionContext{TurnID: ta.TurnID})
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				evt := a.buildTurnAbortedEvent(path, ctxState, projectRoot, ts, ta, lineNum)
				if evt.Model == "" {
					if turnID := assistantTurnID(ta.TurnID); turnID != "" {
						evt.Model = modelForTurn(turnID)
						if evt.Model == "" {
							pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
						}
					}
				}
				res.ToolEvents = append(res.ToolEvents, evt)
			case "mcp_tool_call_end":
				var mc mcpToolCallEnd
				if err := json.Unmarshal(line.Payload, &mc); err != nil {
					continue
				}
				if mc.TurnID != "" {
					applyContext(sessionContext{TurnID: mc.TurnID})
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				preceding := agentMessages[firstNonEmpty(mc.TurnID, ctxState.TurnID)]
				if idx, ok := pending[mc.CallID]; ok && idx < len(res.ToolEvents) {
					mergeMCPCallEndIntoPending(&res.ToolEvents[idx], a, mc)
					delete(pending, mc.CallID)
				} else {
					evt := a.buildMCPCallEndStandaloneEvent(path, ctxState, projectRoot, ts, mc, lineNum, preceding)
					if evt.Model == "" {
						if turnID := assistantTurnID(mc.TurnID); turnID != "" {
							evt.Model = modelForTurn(turnID)
							if evt.Model == "" {
								pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
							}
						}
					}
					res.ToolEvents = append(res.ToolEvents, evt)
				}
			case "error":
				var ce codexError
				if err := json.Unmarshal(line.Payload, &ce); err != nil {
					continue
				}
				if ce.Message == "" && ce.CodexErrorInfo == "" {
					continue
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				evt := a.buildCodexErrorEvent(path, ctxState, projectRoot, ts, ce, lineNum)
				if evt.Model == "" {
					if turnID := assistantTurnID(""); turnID != "" {
						evt.Model = modelForTurn(turnID)
						if evt.Model == "" {
							pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
						}
					}
				}
				res.ToolEvents = append(res.ToolEvents, evt)
			case "patch_apply_end":
				var pa patchApplyEnd
				if err := json.Unmarshal(line.Payload, &pa); err != nil {
					continue
				}
				if pa.TurnID != "" {
					applyContext(sessionContext{TurnID: pa.TurnID})
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				preceding := agentMessages[firstNonEmpty(pa.TurnID, ctxState.TurnID)]
				if idx, ok := pending[pa.CallID]; ok && idx < len(res.ToolEvents) {
					mergePatchApplyIntoPending(&res.ToolEvents[idx], a, pa, projectRoot)
					delete(pending, pa.CallID)
				} else {
					evt := a.buildPatchApplyStandaloneEvent(path, ctxState, projectRoot, ts, pa, lineNum, preceding)
					if evt.Model == "" {
						if turnID := assistantTurnID(pa.TurnID); turnID != "" {
							evt.Model = modelForTurn(turnID)
							if evt.Model == "" {
								pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
							}
						}
					}
					res.ToolEvents = append(res.ToolEvents, evt)
				}
			case "task_complete":
				var done taskComplete
				if err := json.Unmarshal(line.Payload, &done); err == nil {
					if done.TurnID != "" {
						applyContext(sessionContext{TurnID: done.TurnID})
					}
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					evt := a.buildTaskCompleteEvent(path, ctxState, projectRoot, ts, done, lineNum)
					if evt.Model == "" {
						if turnID := assistantTurnID(done.TurnID); turnID != "" {
							evt.Model = modelForTurn(turnID)
							if evt.Model == "" {
								pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
							}
						}
					}
					res.ToolEvents = append(res.ToolEvents, evt)
				}
			case "token_count":
				tk, total, ok := parseModernTokenCount(line.Payload)
				if !ok {
					continue
				}
				// Dedup re-emitted identical token_count events. Codex's
				// runtime sometimes writes the same event_msg/token_count
				// twice (observed at lines 134/129 and 171/165 of an
				// inspected rollout, ~2-3s apart, identical
				// last_token_usage AND total_token_usage). Total is
				// monotonic across a session — a non-advancing total
				// means re-emission, NOT a new model call. Skip
				// emission entirely so per-session sums match Codex's
				// own final cumulative figure.
				if total != (tokenUsage{}) {
					if prev, seen := seenModernTotal[ctxState.SessionID]; seen && total == prev {
						continue
					}
					seenModernTotal[ctxState.SessionID] = total
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				turnID := assistantTurnID("")
				evt := models.TokenEvent{
					SourceFile:      path,
					SourceEventID:   fmt.Sprintf("tk:%s:L%d", filepath.Base(path), lineNum),
					SessionID:       ctxState.SessionID,
					ProjectRoot:     projectRoot,
					GitBranch:       ctxState.GitBranch,
					Timestamp:       ts,
					Tool:            models.ToolCodex,
					Model:           modelForTurn(turnID),
					InputTokens:     tk.InputTokens,
					OutputTokens:    tk.OutputTokens,
					CacheReadTokens: tk.Cached,
					ReasoningTokens: tk.Reasoning,
					Source:          models.TokenSourceJSONL,
					Reliability:     models.ReliabilityApproximate,
					MessageID:       turnID,
				}
				res.TokenEvents = append(res.TokenEvents, evt)
				if turnID == "" {
					pendingTurnlessTokenIdxs = append(pendingTurnlessTokenIdxs, len(res.TokenEvents)-1)
				} else if evt.Model == "" {
					pendingTokenModels[turnID] = append(pendingTokenModels[turnID], len(res.TokenEvents)-1)
				}
			}

		case "response_item":
			// Codex Desktop wraps tool intent in a response_item envelope:
			// payload.type discriminates function_call (assistant intent),
			// function_call_output (executor result without success
			// metadata), reasoning (Tier 2), and message (Tier 3).
			//
			// Dedup design (per user requirement, 2026-05-01): when a
			// response_item.function_call lands first, we emit the row
			// and stash the index in pending[call_id]; the matching
			// side-channel event (event_msg/exec_command_end for shell,
			// event_msg/web_search_end for web_search_call) merges its
			// richer fields into that row instead of emitting a duplicate.
			// If the side-channel event was missed (e.g. mid-session
			// truncation, or this code path is mid-resume), the
			// function_call row stands alone — no double-counting, no
			// loss of the call itself.
			switch payloadType {
			case "function_call":
				var rc responseItemFunctionCall
				if err := json.Unmarshal(line.Payload, &rc); err != nil {
					res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: response_item.function_call: %v", lineNum, err))
					continue
				}
				callID := firstNonEmpty(rc.CallID, fmt.Sprintf("%s:L%d", filepath.Base(path), lineNum))
				if _, dupe := pending[callID]; dupe {
					// Same call_id already pending — this is a malformed
					// or replayed segment; skip the second intent.
					continue
				}
				rawInput := unwrapFunctionArguments(rc.Arguments)
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				preceding := agentMessages[ctxState.TurnID]
				evt := a.buildToolEvent(path, callID, ctxState, projectRoot, ts, rc.Name, rawInput, preceding)
				if evt.Model == "" {
					if turnID := assistantTurnID(""); turnID != "" {
						evt.Model = modelForTurn(turnID)
						if evt.Model == "" {
							pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
						}
					}
				}
				pending[callID] = len(res.ToolEvents)
				res.ToolEvents = append(res.ToolEvents, evt)
			case "function_call_output":
				var ro responseItemFunctionCallOutput
				if err := json.Unmarshal(line.Payload, &ro); err != nil {
					continue
				}
				idx, ok := pending[ro.CallID]
				if !ok || idx >= len(res.ToolEvents) {
					continue
				}
				body := ro.Output
				// The output is sometimes itself a JSON object with
				// {"output":"...","metadata":{...}} (e.g. apply_patch).
				body = unwrapStructuredOutput(body)
				row := &res.ToolEvents[idx]
				if row.ToolOutput == "" {
					row.ToolOutput = a.scrubber.String(body)
				}
				// Wall-clock duration: gap between when the function_call
				// was emitted and when its output arrived. Source-format
				// agnostic and works on every codex variant where the call
				// and output share a call_id (which is all of them).
				if row.DurationMs == 0 && !row.Timestamp.IsZero() && !ts.IsZero() {
					if d := ts.Sub(row.Timestamp).Milliseconds(); d > 0 {
						row.DurationMs = d
					}
				}
				delete(pending, ro.CallID)
			case "custom_tool_call":
				var rc responseItemCustomToolCall
				if err := json.Unmarshal(line.Payload, &rc); err != nil {
					res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: response_item.custom_tool_call: %v", lineNum, err))
					continue
				}
				callID := firstNonEmpty(rc.CallID, fmt.Sprintf("%s:L%d", filepath.Base(path), lineNum))
				if _, dupe := pending[callID]; dupe {
					continue
				}
				projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
				preceding := agentMessages[ctxState.TurnID]
				evt := a.buildCustomToolCallEvent(path, callID, ctxState, projectRoot, ts, rc, preceding)
				if evt.Model == "" {
					if turnID := assistantTurnID(""); turnID != "" {
						evt.Model = modelForTurn(turnID)
						if evt.Model == "" {
							pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
						}
					}
				}
				pending[callID] = len(res.ToolEvents)
				res.ToolEvents = append(res.ToolEvents, evt)
			case "custom_tool_call_output":
				var ro responseItemCustomToolCallOutput
				if err := json.Unmarshal(line.Payload, &ro); err != nil {
					continue
				}
				idx, ok := pending[ro.CallID]
				if !ok || idx >= len(res.ToolEvents) {
					continue
				}
				row := &res.ToolEvents[idx]
				body := unwrapStructuredOutput(ro.Output)
				if row.ToolOutput == "" {
					row.ToolOutput = a.scrubber.String(body)
				}
				if row.DurationMs == 0 && !row.Timestamp.IsZero() && !ts.IsZero() {
					if d := ts.Sub(row.Timestamp).Milliseconds(); d > 0 {
						row.DurationMs = d
					}
				}
				// Deliberately do NOT delete pending here. For apply_patch
				// the terminal event is event_msg/patch_apply_end which can
				// land either before or after custom_tool_call_output —
				// leaving the pending entry keeps it mergeable. The single
				// in-memory entry that survives if patch_apply_end never
				// fires is harmless (one-pass scan).
			case "reasoning":
				// response_item.reasoning currently carries only opaque
				// `encrypted_content` plus an empty `summary` array in
				// every Codex Desktop build inspected (838 reasoning
				// items, 0% non-empty summary as of 2026-05). Future
				// builds may populate summary[*].text — when they do,
				// thread the concatenated text into the per-turn
				// agentMessages cache so the next tool_call inherits
				// it as PrecedingReasoning, mirroring agent_message
				// semantics. Forward-compatible no-op for current data.
				var rr responseItemReasoning
				if err := json.Unmarshal(line.Payload, &rr); err == nil {
					if text := reasoningSummaryText(rr.Summary); text != "" {
						turnID := assistantTurnID("")
						if turnID != "" {
							existing := agentMessages[turnID]
							if existing == "" {
								agentMessages[turnID] = text
							} else {
								agentMessages[turnID] = existing + "\n" + text
							}
						}
					}
				}
			case "message":
				// response_item.payload.type=message — role discriminates.
				// role=assistant is captured via event_msg/agent_message
				// (and would duplicate here). role=developer is the
				// system-prompt-shaped channel for permissions/sandbox
				// context Codex Desktop injects mid-turn.
				//
				// role=user is mostly REAL user prompts (already captured
				// via event_msg/user_message — duplicating would
				// double-count). BUT a meaningful subset are XML-envelope
				// synthetic context injections — `<environment_context>`
				// (cwd, shell, current_date, timezone),
				// `<user_instructions>`, etc. — that look like user
				// messages to the model but originate from the runtime,
				// not the user. Capture those as system_prompt; skip the
				// plain-text and markdown ones (those are real user
				// prompts already covered by event_msg/user_message).
				var rm responseItemMessage
				if err := json.Unmarshal(line.Payload, &rm); err == nil {
					body := concatMessageContent(rm.Content)
					emit := false
					role := rm.Role
					switch rm.Role {
					case "developer":
						emit = body != ""
					case "user":
						// Envelope detection — body must START with `<`
						// (after trim) to qualify as synthetic injection.
						// Plain text and markdown headers are real user
						// prompts and stay with event_msg/user_message.
						trimmed := strings.TrimLeft(body, " \t\n\r")
						if strings.HasPrefix(trimmed, "<") {
							emit = true
							role = "user-envelope"
						}
					}
					if emit {
						projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
						if evt, ok := a.systemPromptEvent(path, role, body, ts, ctxState, projectRoot, lineNum, seenSystemPrompts); ok {
							res.ToolEvents = append(res.ToolEvents, evt)
						}
					}
				}
			case "web_search_call":
				// Has a paired event_msg/web_search_end that emits the row.
			}

		case "tool_call", "function_call":
			var tc toolCall
			if err := json.Unmarshal(line.Payload, &tc); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: tool_call: %v", lineNum, err))
				continue
			}
			toolName := firstNonEmpty(tc.Tool, tc.Name)
			callID := firstNonEmpty(tc.CallID, tc.ID)
			if callID == "" {
				// Fall back to rawLine.ID or a line-number synthesis.
				callID = firstNonEmpty(line.ID, fmt.Sprintf("%s:L%d", filepath.Base(path), lineNum))
			}
			projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
			preceding := agentMessages[ctxState.TurnID]
			evt := a.buildToolEvent(path, callID, ctxState, projectRoot, ts, toolName, tc.Input, preceding)
			if evt.Model == "" {
				if turnID := assistantTurnID(""); turnID != "" {
					evt.Model = modelForTurn(turnID)
					if evt.Model == "" {
						pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
					}
				}
			}
			pending[callID] = len(res.ToolEvents)
			res.ToolEvents = append(res.ToolEvents, evt)

		case "tool_output", "function_call_output":
			var to toolOutput
			if err := json.Unmarshal(line.Payload, &to); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: tool_output: %v", lineNum, err))
				continue
			}
			callID := firstNonEmpty(to.CallID, to.ID)
			idx, ok := pending[callID]
			if !ok {
				continue
			}
			body := decodeOutput(to.Output)
			scrubbed := a.scrubber.String(body)
			res.ToolEvents[idx].ToolOutput = scrubbed
			failed := (to.IsError != nil && *to.IsError) || (to.Success != nil && !*to.Success)
			if failed {
				res.ToolEvents[idx].Success = false
				res.ToolEvents[idx].ErrorMessage = truncate(scrubbed, 2048)
			}
			delete(pending, callID)

		case "token_count", "usage":
			var tk tokenCount
			if err := json.Unmarshal(line.Payload, &tk); err != nil {
				continue
			}
			// Codex emits cumulative totals. Convert to per-turn delta by
			// subtracting the running total we've seen in this session.
			//
			// Cold-start handling (audit C1): when fromOffset>0 we're
			// resuming an incremental parse and the in-memory
			// lastInputByID map starts empty even though prior turns
			// already landed in the DB. Treating tk.InputTokens as the
			// delta in that case would emit a single huge over-count
			// (the cumulative total minus zero). Instead, emit 0 for the
			// first event we see in a resume, then compute correct deltas
			// from there. We lose the true delta for that one event but
			// avoid a much larger over-report.
			prev, hasPrev := lastInputByID[ctxState.SessionID]
			var in int64
			switch {
			case !hasPrev && fromOffset == 0:
				// Fresh parse from start of file — first cumulative is
				// the delta.
				in = tk.InputTokens
			case !hasPrev && fromOffset > 0:
				// Resume — baseline this event's cumulative as prev so
				// subsequent events compute correct deltas.
				in = 0
			case tk.InputTokens >= prev:
				in = tk.InputTokens - prev
			default:
				// Negative delta — session reset or upstream resequencing.
				in = tk.InputTokens
			}
			lastInputByID[ctxState.SessionID] = tk.InputTokens

			projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
			turnID := assistantTurnID("")
			model := firstNonEmpty(tk.Model, modelForTurn(turnID))
			evt := models.TokenEvent{
				SourceFile:      path,
				SourceEventID:   fmt.Sprintf("tk:%s:L%d", filepath.Base(path), lineNum),
				SessionID:       ctxState.SessionID,
				ProjectRoot:     projectRoot,
				GitBranch:       ctxState.GitBranch,
				Timestamp:       ts,
				Tool:            models.ToolCodex,
				Model:           model,
				InputTokens:     in,
				OutputTokens:    tk.OutputTokens,
				CacheReadTokens: tk.Cached,
				ReasoningTokens: tk.Reasoning,
				Source:          models.TokenSourceJSONL,
				Reliability:     models.ReliabilityApproximate,
				MessageID:       turnID,
			}
			res.TokenEvents = append(res.TokenEvents, evt)
			if turnID == "" {
				pendingTurnlessTokenIdxs = append(pendingTurnlessTokenIdxs, len(res.TokenEvents)-1)
			} else if evt.Model == "" {
				pendingTokenModels[turnID] = append(pendingTokenModels[turnID], len(res.TokenEvents)-1)
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return res, fmt.Errorf("codex.ParseSessionFile: scan: %w", err)
	}
	return res, nil
}

// buildCustomToolCallEvent emits the assistant-side row for a
// response_item/custom_tool_call. In current Codex Desktop builds the
// only `name` is "apply_patch", but we route through actionMap so a
// future custom-tool name lands as ActionUnknown without crashing. The
// patch text is parsed for the first changed file path so the row's
// Target is meaningful even without the matching patch_apply_end.
func (a *Adapter) buildCustomToolCallEvent(
	sourceFile, callID string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	rc responseItemCustomToolCall,
	preceding string,
) models.ToolEvent {
	actionType, ok := actionMap[rc.Name]
	if !ok {
		actionType = models.ActionUnknown
	}
	target := ""
	if rc.Name == "apply_patch" {
		target = applyPatchTarget(rc.Input)
		if target != "" && projectRoot != "" {
			target = git.RelativePath(projectRoot, target)
		}
	}
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      callID,
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         actionType,
		Target:             truncate(target, 200),
		Success:            true,
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        rc.Name,
		RawToolInput:       a.scrubber.String(rc.Input),
		MessageID:          sess.TurnID,
	}
}

// buildPatchApplyStandaloneEvent emits a row when patch_apply_end lands
// without a matching pending custom_tool_call (mid-session resume,
// truncated rollout). Carries the structured `changes` summary as the
// authoritative source.
func (a *Adapter) buildPatchApplyStandaloneEvent(
	sourceFile string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	pa patchApplyEnd,
	lineNum int,
	preceding string,
) models.ToolEvent {
	target := patchApplyTargetFromChanges(pa.Changes, projectRoot)
	output := strings.TrimSpace(pa.Stdout + pa.Stderr)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      firstNonEmpty(pa.CallID, fmt.Sprintf("patch:%s:L%d", filepath.Base(sourceFile), lineNum)),
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionEditFile,
		Target:             truncate(target, 200),
		Success:            pa.Success,
		ErrorMessage:       errorIfFailed(pa.Success, output),
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        "patch_apply_end",
		ToolOutput:         a.scrubber.String(output),
		MessageID:          firstNonEmpty(pa.TurnID, sess.TurnID),
	}
}

// mergePatchApplyIntoPending merges a patch_apply_end side-channel into
// an already-emitted custom_tool_call row. The Changes map's first key
// is preferred over whatever applyPatchTarget extracted from the patch
// text, since it's the post-execution canonical path list.
func mergePatchApplyIntoPending(row *models.ToolEvent, a *Adapter, pa patchApplyEnd, projectRoot string) {
	row.ActionType = models.ActionEditFile
	row.Success = pa.Success
	output := strings.TrimSpace(pa.Stdout + pa.Stderr)
	row.ToolOutput = a.scrubber.String(output)
	row.ErrorMessage = errorIfFailed(pa.Success, output)
	row.RawToolName = "patch_apply_end"
	if t := patchApplyTargetFromChanges(pa.Changes, projectRoot); t != "" {
		row.Target = truncate(t, 200)
	}
}

// dynamicToolCallBody concatenates text content_items into a single
// string for the row's ToolOutput.
func dynamicToolCallBody(items []dynamicToolCallItem) string {
	var pieces []string
	for _, it := range items {
		text := strings.TrimSpace(it.Text)
		if text != "" {
			pieces = append(pieces, text)
		}
	}
	return strings.Join(pieces, "\n")
}

// systemPromptEvent emits an ActionSystemPrompt row for a piece of
// system-prompt-shaped content. Returns (zero, false) when the body is
// empty or its content hash has already been seen in this parse —
// codex repeats large (~9-18KB) base_instructions and
// developer_instructions across nearly every session_meta and
// turn_context, so dedup is mandatory or we'd emit thousands of
// duplicate rows.
//
// Body lives in RawToolInput (scrubbed). Target carries a 200-char
// preview. MessageID is "system:<hash>" so cross-row joins can group
// occurrences of the same prompt body. role discriminates 'base'
// (session-level system prompt) vs 'developer' (turn-level or
// response_item.message.role=developer instructions).
func (a *Adapter) systemPromptEvent(
	sourceFile, role, body string,
	ts time.Time,
	sess sessionContext,
	projectRoot string,
	lineNum int,
	seen map[string]bool,
) (models.ToolEvent, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return models.ToolEvent{}, false
	}
	hash := shortHash(body)
	if seen[hash] {
		return models.ToolEvent{}, false
	}
	seen[hash] = true
	preview := body
	if len(preview) > 200 {
		preview = preview[:200]
	}
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("sysprompt:%s:%s:L%d", role, hash, lineNum),
		SessionID:     sess.SessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     sess.GitBranch,
		Model:         sess.Model,
		Tool:          models.ToolCodex,
		ActionType:    models.ActionSystemPrompt,
		Target:        preview,
		Success:       true,
		RawToolName:   "system_prompt." + role,
		RawToolInput:  a.scrubber.String(body),
		MessageID:     "system:" + hash,
	}, true
}

// concatMessageContent flattens a response_item.message content array
// into a single string. Joins the `text` field of every part (Codex
// developer-role messages use type="input_text"; assistant-role would
// use "output_text", but those are skipped at the call site).
func concatMessageContent(parts []responseItemMessageContent) string {
	var pieces []string
	for _, p := range parts {
		text := strings.TrimSpace(p.Text)
		if text != "" {
			pieces = append(pieces, text)
		}
	}
	return strings.Join(pieces, "\n")
}

// buildCompactedEvent emits an ActionContextCompacted row summarizing
// what got compacted: message count + byte estimate (sum of text
// content) + token estimate (bytes/4, matching the rest of the
// codebase's char-count → token heuristic for non-tokenized estimates).
// Per user direction (2026-05-01) these rows are not searchable like
// file edits — the action_type discriminator lets the dashboard
// suppress them from action-type browsers while keeping them
// available for cost / compaction-frequency analytics.
func (a *Adapter) buildCompactedEvent(
	sourceFile string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	ce compactedEvent,
	lineNum int,
) models.ToolEvent {
	msgCount := len(ce.ReplacementHistory)
	bytesEst := 0
	for _, msg := range ce.ReplacementHistory {
		for _, blk := range msg.Content {
			bytesEst += len(blk.Text)
		}
	}
	tokensEst := bytesEst / 4
	target := fmt.Sprintf("%d msgs, ~%d tokens", msgCount, tokensEst)
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("compacted:%s:L%d", filepath.Base(sourceFile), lineNum),
		SessionID:     sess.SessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     sess.GitBranch,
		Model:         sess.Model,
		Tool:          models.ToolCodex,
		ActionType:    models.ActionContextCompacted,
		Target:        truncate(target, 200),
		Success:       true,
		RawToolName:   "compacted",
		RawToolInput:  fmt.Sprintf(`{"messages":%d,"bytes_estimate":%d,"tokens_estimate":%d}`, msgCount, bytesEst, tokensEst),
		ToolOutput:    a.scrubber.String(truncate(ce.Message, 2048)),
		MessageID:     sess.TurnID,
	}
}

// buildTurnAbortedEvent emits an ActionTurnAborted row for a Codex
// turn that was interrupted before completing. Distinct from a
// task_complete with success=false: aborted turns never finished
// generating, so the model output is partial — analysts filtering
// for completed turns vs aborts need the action_type discriminator.
func (a *Adapter) buildTurnAbortedEvent(
	sourceFile string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	ta turnAborted,
	lineNum int,
) models.ToolEvent {
	if ta.CompletedAt > 0 {
		ts = time.Unix(ta.CompletedAt, 0).UTC()
	}
	reason := firstNonEmpty(ta.Reason, "interrupted")
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("aborted:%s:%d", firstNonEmpty(ta.TurnID, sess.SessionID, filepath.Base(sourceFile)), lineNum),
		SessionID:     firstNonEmpty(sess.SessionID, ta.TurnID),
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     sess.GitBranch,
		Model:         sess.Model,
		Tool:          models.ToolCodex,
		ActionType:    models.ActionTurnAborted,
		Target:        truncate(reason, 200),
		Success:       false,
		ErrorMessage:  "turn aborted: " + reason,
		DurationMs:    ta.DurationMs,
		RawToolName:   "turn_aborted",
		MessageID:     firstNonEmpty(ta.TurnID, sess.TurnID),
	}
}

// mergeMCPCallEndIntoPending overwrites the pending function_call row
// with structured MCP call result data: server:tool target, content
// text as ToolOutput, success/error from the Ok|Err tagged union, and
// duration. Promotes the ActionType to ActionMCPCall if it wasn't
// already (response_item.function_call may have routed list_mcp_*
// names to mcp_call via actionMap, but other server-defined tool
// names fall to Unknown without this).
func mergeMCPCallEndIntoPending(row *models.ToolEvent, a *Adapter, mc mcpToolCallEnd) {
	row.ActionType = models.ActionMCPCall
	row.Target = truncate(mcpCallTarget(mc.Invocation), 200)
	output, success, errMsg := mcpCallResultBody(mc.Result)
	row.ToolOutput = a.scrubber.String(output)
	row.Success = success
	if !success {
		row.ErrorMessage = truncate(errMsg, 2048)
	} else {
		row.ErrorMessage = ""
	}
	row.DurationMs = mc.Duration.Secs*1000 + mc.Duration.Nanos/1_000_000
	row.RawToolName = "mcp_tool_call_end"
}

// buildMCPCallEndStandaloneEvent emits a row when mcp_tool_call_end
// fires without a preceding response_item.function_call (mid-session
// resume, or the response_item never landed). Carries everything the
// merge would have populated.
func (a *Adapter) buildMCPCallEndStandaloneEvent(
	sourceFile string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	mc mcpToolCallEnd,
	lineNum int,
	preceding string,
) models.ToolEvent {
	output, success, errMsg := mcpCallResultBody(mc.Result)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      firstNonEmpty(mc.CallID, fmt.Sprintf("mcp:%s:L%d", filepath.Base(sourceFile), lineNum)),
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionMCPCall,
		Target:             truncate(mcpCallTarget(mc.Invocation), 200),
		Success:            success,
		ErrorMessage:       errorIfFailed(success, errMsg),
		DurationMs:         mc.Duration.Secs*1000 + mc.Duration.Nanos/1_000_000,
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        "mcp_tool_call_end",
		RawToolInput:       a.scrubber.RawJSON(mc.Invocation.Arguments),
		ToolOutput:         a.scrubber.String(output),
		MessageID:          firstNonEmpty(mc.TurnID, sess.TurnID),
	}
}

// mcpCallTarget formats "server:tool" for the row's Target field, with
// safe fallbacks when one or the other is empty.
func mcpCallTarget(inv mcpInvocation) string {
	switch {
	case inv.Server != "" && inv.Tool != "":
		return inv.Server + ":" + inv.Tool
	case inv.Tool != "":
		return inv.Tool
	default:
		return inv.Server
	}
}

// mcpCallResultBody flattens the Ok|Err tagged union into (output,
// success, errorMessage). Success requires Ok present and isError
// false; if Ok.isError is true the success is false but we still
// surface the content text as the error body. If Err is present that
// message wins.
func mcpCallResultBody(r mcpCallResult) (string, bool, string) {
	if r.Err != nil {
		return r.Err.Message, false, r.Err.Message
	}
	if r.Ok != nil {
		var pieces []string
		for _, c := range r.Ok.Content {
			if c.Type == "text" && c.Text != "" {
				pieces = append(pieces, c.Text)
			}
		}
		body := strings.Join(pieces, "\n")
		if r.Ok.IsError {
			return body, false, body
		}
		return body, true, ""
	}
	// Neither Ok nor Err — defensively succeed-empty.
	return "", true, ""
}

// buildCodexErrorEvent emits an ActionAPIError row from event_msg/error.
// Maps to the same shape claudecode uses for type=system / subtype=api_error
// records: Target carries the error class (`codex_error_info`),
// ErrorMessage carries the human-readable body, RawToolName preserves
// the upstream class for filtering, Success is always false.
func (a *Adapter) buildCodexErrorEvent(
	sourceFile string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	ce codexError,
	lineNum int,
) models.ToolEvent {
	class := firstNonEmpty(ce.CodexErrorInfo, "api_error")
	scrubbed := a.scrubber.String(ce.Message)
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("error:%s:L%d:%s", filepath.Base(sourceFile), lineNum, shortHash(class+":"+ce.Message)),
		SessionID:     sess.SessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     sess.GitBranch,
		Model:         sess.Model,
		Tool:          models.ToolCodex,
		ActionType:    models.ActionAPIError,
		Target:        truncate(class, 200),
		Success:       false,
		ErrorMessage:  truncate(scrubbed, 2048),
		RawToolName:   class,
		MessageID:     sess.TurnID,
	}
}

// reasoningSummaryText concatenates any text fields present in a
// response_item.reasoning summary array. Returns "" when the array is
// empty or carries no text segments — current Codex Desktop emits
// {summary:[], encrypted_content:"..."} so the typical return is "".
func reasoningSummaryText(parts []reasoningSummaryPart) string {
	var pieces []string
	for _, p := range parts {
		text := strings.TrimSpace(p.Text)
		if text == "" {
			continue
		}
		pieces = append(pieces, text)
	}
	return strings.Join(pieces, "\n")
}

// applyPatchTarget pulls the first changed file path out of the
// pseudo-diff format Codex apply_patch uses. Looks for `*** Add File:`,
// `*** Update File:`, `*** Delete File:`, or `*** Move File:` headers.
// Returns "" if the patch text doesn't follow that format.
func applyPatchTarget(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		const prefix = "*** "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := line[len(prefix):]
		for _, header := range []string{"Add File:", "Update File:", "Delete File:", "Move File:"} {
			if strings.HasPrefix(rest, header) {
				return strings.TrimSpace(rest[len(header):])
			}
		}
	}
	return ""
}

// patchApplyTargetFromChanges picks the first key from a patch_apply_end
// changes map. Maps in Go have non-deterministic iteration order, but
// the codex executor typically emits a single-file patch — when there
// are multiple, any one is reasonable for the row's Target field.
func patchApplyTargetFromChanges(changes map[string]patchApplyChange, projectRoot string) string {
	for path := range changes {
		if path == "" {
			continue
		}
		if projectRoot != "" {
			return git.RelativePath(projectRoot, path)
		}
		return path
	}
	return ""
}

// mergeExecIntoPending overwrites the pending function_call row with the
// richer data from event_msg/exec_command_end. The row keeps its
// source_event_id (the call_id) and Tool/SessionID/MessageID/Model from
// the function_call side; everything else is updated.
func mergeExecIntoPending(row *models.ToolEvent, a *Adapter, ex execCommandEnd) {
	command := commandString(ex.Command)
	output := firstNonEmpty(ex.AggregatedOutput, ex.Stdout+ex.Stderr)
	scrubbedOutput := a.scrubber.String(output)
	success := ex.Status != "failed" && ex.ExitCode == 0
	row.ActionType = models.ActionRunCommand
	row.Target = truncate(a.scrubber.String(command), 200)
	row.Success = success
	row.ErrorMessage = errorIfFailed(success, scrubbedOutput)
	row.DurationMs = ex.Duration.Secs*1000 + ex.Duration.Nanos/1_000_000
	row.ToolOutput = scrubbedOutput
	row.RawToolName = "exec_command_end"
	row.RawToolInput = a.scrubber.RawJSON(ex.Command)
}

// mergeWebSearchIntoPending overwrites the pending function_call row's
// Target field with the resolved query from event_msg/web_search_end. The
// call-side intent does not include the query text, so this merge is
// strictly additive.
func mergeWebSearchIntoPending(row *models.ToolEvent, ws webSearchEnd) {
	query := firstNonEmpty(ws.Query, ws.Action.Query, strings.Join(ws.Action.Queries, "; "))
	if query != "" {
		row.Target = truncate(query, 200)
	}
	row.ActionType = models.ActionWebSearch
	row.RawToolName = "web_search_end"
}

// unwrapFunctionArguments converts Codex's `arguments` field (a JSON
// string containing a JSON object, e.g. `"{\"command\":\"...\"}"` decoded
// to the Go string `{"command":"..."}`) into a json.RawMessage suitable
// for buildToolEvent. Empty input returns nil.
func unwrapFunctionArguments(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	return json.RawMessage(args)
}

// unwrapStructuredOutput peels one level of JSON-string wrapping when the
// output payload is itself a JSON object with an "output" key (the codex
// custom_tool_call_output convention). Falls back to the raw string.
func unwrapStructuredOutput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '{' {
		return s
	}
	var m struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal([]byte(s), &m); err == nil && m.Output != "" {
		return m.Output
	}
	return s
}

func payloadType(raw json.RawMessage) string {
	var env payloadEnvelope
	_ = json.Unmarshal(raw, &env)
	return env.Type
}

func (a *Adapter) buildUserPromptEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, lineNum int, message string) models.ToolEvent {
	message = strings.TrimSpace(message)
	msgID := ""
	if sess.TurnID != "" {
		msgID = "user:" + sess.TurnID
	}
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("user:%s:L%d:%s", filepath.Base(sourceFile), lineNum, shortHash(message)),
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionUserPrompt,
		Target:             truncate(message, 200),
		Success:            true,
		PrecedingReasoning: truncate(message, 200),
		RawToolName:        "user_message",
		RawToolInput:       a.scrubber.String(message),
		MessageID:          msgID,
	}
}

func (a *Adapter) buildExecCommandEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, ex execCommandEnd, preceding string) models.ToolEvent {
	command := commandString(ex.Command)
	output := firstNonEmpty(ex.AggregatedOutput, ex.Stdout+ex.Stderr)
	scrubbedOutput := a.scrubber.String(output)
	success := ex.Status != "failed" && ex.ExitCode == 0
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      firstNonEmpty(ex.CallID, "exec:"+shortHash(command+ts.String())),
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionRunCommand,
		Target:             truncate(a.scrubber.String(command), 200),
		Success:            success,
		ErrorMessage:       errorIfFailed(success, scrubbedOutput),
		DurationMs:         ex.Duration.Secs*1000 + ex.Duration.Nanos/1_000_000,
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        "exec_command_end",
		RawToolInput:       a.scrubber.RawJSON(ex.Command),
		ToolOutput:         scrubbedOutput,
		MessageID:          firstNonEmpty(ex.TurnID, sess.TurnID),
	}
}

func (a *Adapter) buildWebSearchEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, ws webSearchEnd, lineNum int, preceding string) models.ToolEvent {
	query := firstNonEmpty(ws.Query, ws.Action.Query, strings.Join(ws.Action.Queries, "; "))
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      firstNonEmpty(ws.CallID, fmt.Sprintf("web:%s:L%d:%s", filepath.Base(sourceFile), lineNum, shortHash(query))),
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionWebSearch,
		Target:             truncate(query, 200),
		Success:            true,
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        "web_search_end",
		RawToolInput:       a.scrubber.String(query),
		MessageID:          firstNonEmpty(ws.TurnID, sess.TurnID),
	}
}

func (a *Adapter) buildTaskCompleteEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, done taskComplete, lineNum int) models.ToolEvent {
	if done.CompletedAt > 0 {
		ts = time.Unix(done.CompletedAt, 0).UTC()
	}
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("complete:%s:%d", firstNonEmpty(done.TurnID, sess.SessionID, filepath.Base(sourceFile)), lineNum),
		SessionID:          firstNonEmpty(sess.SessionID, done.TurnID),
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionTaskComplete,
		Target:             "task_complete",
		Success:            true,
		DurationMs:         done.DurationMs,
		PrecedingReasoning: truncate(done.LastAgentMessage, 200),
		RawToolName:        "task_complete",
		MessageID:          firstNonEmpty(done.TurnID, sess.TurnID),
	}
}

func (a *Adapter) buildToolEvent(
	sourceFile, callID string,
	sess sessionContext,
	projectRoot string,
	ts time.Time,
	toolName string,
	rawInput json.RawMessage,
	preceding string,
) models.ToolEvent {
	actionType, ok := actionMap[toolName]
	if !ok {
		actionType = models.ActionUnknown
	}
	scrubbedInput := a.scrubber.RawJSON(rawInput)
	target := a.extractTarget(toolName, rawInput, projectRoot)

	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      callID,
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         actionType,
		Target:             target,
		Success:            true,
		PrecedingReasoning: truncate(preceding, 500),
		RawToolName:        toolName,
		RawToolInput:       firstNonEmpty(scrubbedInput, scrub.Truncate(string(rawInput))),
		MessageID:          sess.TurnID,
	}
}

func (a *Adapter) extractTarget(toolName string, rawInput json.RawMessage, projectRoot string) string {
	if len(rawInput) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(rawInput, &m); err != nil {
		return ""
	}
	pickStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch toolName {
	case "shell", "shell_command":
		// Codex shell inputs: {"command": ["bash", "-lc", "..."]} or {"command": "..."}
		if arr, ok := m["command"].([]any); ok && len(arr) > 0 {
			parts := make([]string, 0, len(arr))
			for _, p := range arr {
				if s, ok := p.(string); ok {
					parts = append(parts, s)
				}
			}
			return a.scrubber.String(strings.Join(parts, " "))
		}
		return a.scrubber.String(pickStr("command", "cmd"))
	case "file_read", "file_write", "apply_patch", "view_image":
		fp := pickStr("path", "file_path", "filename", "target")
		if fp == "" {
			return ""
		}
		if projectRoot != "" {
			return git.RelativePath(projectRoot, fp)
		}
		return fp
	case "web_search":
		return pickStr("query", "q")
	}
	return ""
}

func (a *Adapter) resolveProjectRoot(cwd string, cache map[string]string) string {
	if cwd == "" {
		return ""
	}
	// Codex on Windows records cwd as a Windows-style path (e.g.
	// "c:\programsx\regulation"). When that JSONL is parsed by an
	// observer running in WSL2, filepath.Abs treats the string as
	// relative because Linux doesn't recognise the drive prefix —
	// which prepends the observer's CWD and then findGitRoot walks UP
	// looking for .git. In the worst case it lands on observer's own
	// repo and every codex action gets misattributed. Translate to
	// the WSL2 mount equivalent ("/mnt/c/programsx/regulation") so
	// git.Resolve operates on the actual cross-mount path. No-op on
	// Windows hosts and on cwds that already look like native paths.
	cwd = crossmount.TranslateForeignPath(cwd)
	if root, ok := cache[cwd]; ok {
		return root
	}
	info, err := git.Resolve(cwd)
	if err != nil {
		cache[cwd] = cwd
		return cwd
	}
	cache[cwd] = info.Root
	return info.Root
}

func decodeOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var m struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Output string `json:"output"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(raw, &m); err == nil {
		switch {
		case m.Output != "":
			return m.Output
		case m.Text != "":
			return m.Text
		case m.Stdout != "" || m.Stderr != "":
			return m.Stdout + m.Stderr
		}
	}
	return string(raw)
}

// parseModernTokenCount extracts the per-call usage (last_token_usage)
// AND the cumulative session total (total_token_usage) from a Codex
// modern event_msg/token_count payload. The total is returned for
// dedup purposes — Codex sometimes re-emits an identical token_count
// record (same last + same total) which, if not skipped, double-counts
// that turn's usage in the database. Caller uses the total as a
// fingerprint and skips emission when it matches the previous total
// for the same session. Total is monotonic, so a non-advancing total
// is always a re-emission.
func parseModernTokenCount(raw json.RawMessage) (tokenCount, tokenUsage, bool) {
	var mt modernTokenCount
	if err := json.Unmarshal(raw, &mt); err != nil {
		return tokenCount{}, tokenUsage{}, false
	}
	usage := mt.Info.LastTokenUsage
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 &&
		usage.CachedInputTokens == 0 && usage.ReasoningTokens == 0 {
		return tokenCount{}, tokenUsage{}, false
	}
	return tokenCount{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
		Cached:       usage.CachedInputTokens,
		Reasoning:    usage.ReasoningTokens,
	}, mt.Info.TotalTokenUsage, true
}

func commandString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []string
	if err := json.Unmarshal(raw, &parts); err == nil {
		return strings.Join(parts, " ")
	}
	return string(raw)
}

func errorIfFailed(success bool, output string) string {
	if success {
		return ""
	}
	if output == "" {
		return "(no output)"
	}
	return truncate(output, 2048)
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end {
		c := b[start]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			start++
			continue
		}
		break
	}
	for end > start {
		c := b[end-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			end--
			continue
		}
		break
	}
	return b[start:end]
}

func parseTimestamp(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	return time.Time{}
}

func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".jsonl")
	base = strings.TrimPrefix(base, "rollout-")
	return base
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
