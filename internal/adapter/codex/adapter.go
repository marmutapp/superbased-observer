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
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/contentcap"
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

// IsSessionFile matches rollout-*.jsonl files under one of this
// adapter's WatchPaths. The under-WatchPaths constraint enforces the
// v1.4.51 dispatch contract: predicates self-limit to paths the
// adapter could actually own, so a future broad-predicate adapter
// can't accidentally claim a Codex rollout file by alphabetical sort.
func (a *Adapter) IsSessionFile(path string) bool {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "rollout-") || filepath.Ext(base) != ".jsonl" {
		return false
	}
	return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
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
	"shell_command":                models.ActionRunCommand,
	"update_plan":                  models.ActionTodoUpdate,
	"list_mcp_resources":           models.ActionMCPCall,
	"list_mcp_resource_templates":  models.ActionMCPCall,
	"search_past_outputs":          models.ActionMCPCall,
	"get_session_summary":          models.ActionMCPCall,
	"get_project_patterns":         models.ActionMCPCall,
	"get_last_test_result":         models.ActionMCPCall,
	"get_session_recovery_context": models.ActionMCPCall,
	"get_cost_summary":             models.ActionMCPCall,
	"check_command_freshness":      models.ActionMCPCall,
	"get_failure_context":          models.ActionMCPCall,
	"load_workspace_dependencies":  models.ActionMCPCall,
	"view_image":                   models.ActionReadFile,
	// exec_command is the modern Codex Desktop (>=v0.130) shell tool
	// name — distinct from the older "shell" / "shell_command". Without
	// this mapping these rows land as ActionUnknown; live maintainer DB
	// shows 260 such rows (Issue #6 cross-adapter sweep).
	"exec_command": models.ActionRunCommand,
	// PowerShell / pwsh / cmd.exe surface when Codex on Windows
	// invokes its shell tool with a windows-shell payload. The tool
	// name in the function_call envelope is the literal interpreter.
	"powershell": models.ActionRunCommand,
	"pwsh":       models.ActionRunCommand,
	"cmd.exe":    models.ActionRunCommand,
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
	// EffortLevel is the per-turn reasoning effort the model was
	// asked to use (minimal | low | medium | high). Populated from
	// turn_context.payload.collaboration_mode.settings.reasoning_effort
	// — verified path on real codex 0.129+ JSONL fixtures. Not
	// JSON-parsed directly (the field doesn't ride on the same
	// flat envelope as the other context fields) — the
	// turn_context handler reads it out of the nested struct
	// and assigns it onto ctxState.
	EffortLevel string `json:"-"`
	// v1.4.52 added codex 0.130+ turn_context fields. All "sticky":
	// once seen, they ride every subsequent action until the next
	// turn_context updates them. Same pattern as EffortLevel.
	CollaborationMode string `json:"-"`
	Personality       string `json:"-"`
	RealtimeActive    bool   `json:"-"`
	TruncationMode    string `json:"-"`
	TruncationLimit   int64  `json:"-"`
	// ServiceTier is the operator's requested OpenAI processing tier
	// ("priority" = Codex Fast mode, "flex" = slow/discount, "default"/
	// "auto" = standard). It is NOT present anywhere in the rollout JSONL
	// (confirmed 2026-06-08: service_tier appears only as a tool-schema
	// definition, never a value), so the adapter reads it once per file
	// from the owning ~/.codex/config.toml — see codexServiceTier. Sticky
	// for the whole file; never updated from a parsed line. Drives the
	// per-message ServiceTier pill and TokenEvent.Fast (priority → fast).
	ServiceTier string `json:"-"`
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
//
// CollaborationMode.Settings.ReasoningEffort is the canonical per-turn
// effort signal in codex 0.129+ JSONL: minimal | low | medium | high,
// or null when the user hasn't overridden the model default. Verified
// against a real local fixture; *string distinguishes "field absent"
// (no nesting at all) from "field present but null" (Codex sent the
// envelope but the user didn't override) — both collapse to the
// empty-string sentinel for our purposes downstream.
type turnContextPayload struct {
	sessionContext
	DeveloperInstructions string `json:"developer_instructions"`
	CollaborationMode     struct {
		// Mode discriminates the user-facing collaboration surface in
		// codex 0.130+: "default" (free-to-edit) vs "plan"
		// (think-only). High-signal because the same model in plan
		// mode produces zero side-effects — costs and apparent
		// quality should be interpreted differently from a default-
		// mode session.
		Mode     string `json:"mode"`
		Settings struct {
			ReasoningEffort *string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
	// Personality is the active Codex Desktop persona ("friendly",
	// etc.) — controls the base-instructions tone.
	Personality string `json:"personality"`
	// RealtimeActive is true while codex 0.130+'s real-time/voice
	// surface is active. Currently rare; capture for future signals.
	RealtimeActive bool `json:"realtime_active"`
	// TruncationPolicy carries codex 0.130+'s per-turn truncation
	// strategy + budget (e.g. {mode:"tokens", limit:10000}). Useful
	// forensics when assistant output got cut short.
	TruncationPolicy struct {
		Mode  string `json:"mode"`
		Limit int64  `json:"limit"`
	} `json:"truncation_policy"`
}

// EffortFromPayload returns the effort string (minimal | low | medium
// | high) from the turn_context's collaboration_mode envelope, or ""
// when not set / explicit null. Helper so the parse loop reads
// cleanly.
func (p turnContextPayload) EffortFromPayload() string {
	if p.CollaborationMode.Settings.ReasoningEffort == nil {
		return ""
	}
	return *p.CollaborationMode.Settings.ReasoningEffort
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
	// TimeToFirstTokenMS is codex 0.130+'s gap between task_started
	// and the first streamed assistant token. Captures model warmup +
	// upstream queue latency separately from total duration.
	TimeToFirstTokenMS int64 `json:"time_to_first_token_ms"`
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
// the turn's agentMessages cache for downstream PrecedingReasoning,
// AND emits a standalone ToolEvent so reasoning is visible in the
// action stream (zero-text encrypted-only items appear as opaque
// placeholders with a byte-size proxy).
type responseItemReasoning struct {
	Summary          []reasoningSummaryPart `json:"summary"`
	EncryptedContent string                 `json:"encrypted_content"`
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
	// RateLimits is the Codex 0.130+ envelope carried alongside
	// `info`. Present even when `info` is null (the startup
	// token_count fires with rate_limits-only). Emitted as
	// ActionRateLimit ToolEvent rows reusing the cowork-introduced
	// schema (RateLimitStatus / Type / ResetsAt / OverageStatus).
	RateLimits *codexRateLimits `json:"rate_limits"`
}

// codexRateLimits is the Codex 0.130+ token_count.rate_limits
// envelope. Two windows (primary / secondary) with
// used_percent + window_minutes + resets_at; plus a session-level
// plan_type ("plus" / "pro" / "team") and rate_limit_reached_type
// (null normally, set to "primary" / "secondary" when hit). Limit_id
// is the rate-limit family ("codex" today).
type codexRateLimits struct {
	LimitID              string                `json:"limit_id"`
	LimitName            *string               `json:"limit_name"`
	Primary              *codexRateLimitWindow `json:"primary"`
	Secondary            *codexRateLimitWindow `json:"secondary"`
	Credits              *json.RawMessage      `json:"credits"`
	PlanType             string                `json:"plan_type"`
	RateLimitReachedType *string               `json:"rate_limit_reached_type"`
}

type codexRateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
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
	fi, err := f.Stat()
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("codex.ParseSessionFile: stat %s: %w", path, err)
	}
	fileSize := fi.Size()

	res := adapter.ParseResult{NewOffset: fromOffset}

	// Fall back to the filename stem as session id if no real
	// session-bearing envelope ever lands (e.g. incremental parse
	// starting mid-file). This fallback is provisional only: the first
	// real SessionID from the file replaces it, and later replayed
	// session_meta records must not overwrite that real owner.
	fallbackSessionID := sessionIDFromPath(path)
	ctxState := sessionContext{}
	hasRealSessionID := false

	// On incremental resume (fromOffset > 0) the chunk we're about to
	// parse usually does NOT contain the leading session_meta /
	// session_configured / turn_context lines that carry SessionID, Cwd,
	// Model, GitBranch. Without those, every event emitted from the
	// resumed chunk lands with the date-prefixed filename as SessionID
	// and an empty ProjectRoot — store.Ingest then drops the lot
	// silently because empty ProjectRoot is a hard skip. Prefetch the
	// context-bearing leading lines so the resumed events inherit the
	// real session id and cwd. Bounded by `fromOffset` so the cost is at
	// most one extra read of the bytes already on disk before resume.
	// lineOffset tracks how many lines preceded fromOffset on
	// incremental resume. SourceEventIDs that embed `:L<linenum>:`
	// (user_prompt, task_complete, system_prompt, mcp, web, view_image,
	// patch, compacted, error, plus the call_id fallback paths) must
	// stay stable across re-parses of the same file — otherwise a full
	// rescan with `observer scan --force` produces duplicate rows
	// because the L-num is chunk-relative on resume but absolute on
	// rescan. prefetchSessionContext returns the count so the main
	// parse can resume from there. 2026-05-11 maintainer dogfood
	// surfaced this: a scan --force on today's sessions created 17
	// dup rows (user_prompt + task_complete + unknown + system_prompt)
	// before the fix landed.
	lineOffset := 0
	if fromOffset > 0 {
		if hdr, lineCount, ok := prefetchSessionContext(f, fromOffset); ok {
			ctxState = mergeSessionContext(sessionContext{}, hdr)
			hasRealSessionID = ctxState.SessionID != ""
			lineOffset = lineCount
		}
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("codex.ParseSessionFile: seek: %w", err)
		}
	}
	if !hasRealSessionID {
		ctxState.SessionID = fallbackSessionID
	}
	// Codex Fast mode (service_tier:"priority") is never recorded in the
	// rollout JSONL, so read the operator's requested tier once from the
	// owning ~/.codex/config.toml. Sticky for the whole file; surfaces as
	// the per-message ServiceTier pill (via withEffort) and drives
	// TokenEvent.Fast (priority → fast → Pricing.FastMultiplier).
	if tier := codexServiceTier(path); tier != "" {
		ctxState.ServiceTier = tier
	}
	rootCache := map[string]string{}
	pending := map[string]int{}            // call_id → res.ToolEvents index
	lastInputByID := map[string]int64{}    // legacy gross-cumulative input tracker; preserved for the unused branch in case a fixture still drives it (see lastNetInputByID below for the net-cumulative variant the active math uses)
	lastNetInputByID := map[string]int64{} // tracks (gross_input - cached_input) cumulative per session so the per-turn delta we store excludes the cached portion (Anthropic-shape convention, see cost engine TokenBundle docstring)
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
	// runningWebSearchCount tallies event_msg/web_search_end records
	// seen since the last NON-deduped token_count emission. Flushed
	// onto TokenEvent.WebSearchRequests when the next token_count row
	// is appended (so cost engine can apply OpenAI's per-request fee
	// via Pricing.WebSearchPerRequest). Reset to 0 after each
	// flushed emission. Cross-turn behavior: counts accumulate
	// across turns whose token_count was dedup-skipped, then attach
	// to the next surviving emission — under-counting is impossible,
	// double-counting is prevented by the reset. Live-watch
	// limitation: on incremental resume (fromOffset > 0) the
	// counter is fresh, so web_searches whose paired token_count is
	// in a later poll chunk will land with WebSearchRequests=0;
	// `observer backfill --codex-rescan` re-walks from offset 0 and
	// re-attributes them correctly.
	runningWebSearchCount := int64(0)

	applyContext := func(sc sessionContext) {
		// Session ownership is file-local: once a rollout establishes
		// the owning SessionID, later replayed context (notably the
		// parent session_meta in forked child rollouts) must NOT
		// overwrite it. Other fields still refresh normally.
		if !hasRealSessionID {
			if sc.ID != "" {
				ctxState.SessionID = sc.ID
				hasRealSessionID = true
			}
			if !hasRealSessionID && sc.SessionID != "" {
				ctxState.SessionID = sc.SessionID
				hasRealSessionID = true
			}
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
		// EffortLevel is sticky: a later turn_context that omits
		// collaboration_mode (or sends null reasoning_effort) must NOT
		// wipe a previously-established value. Same precedence rule as
		// the other context fields above.
		if sc.EffortLevel != "" {
			ctxState.EffortLevel = sc.EffortLevel
		}
		// v1.4.52 codex 0.130+ turn_context fields. Sticky for the
		// string + int fields (zero-value means "not in this update"
		// so we don't wipe). RealtimeActive is handled directly in
		// the turn_context handler instead of here, because
		// applyContext is also called from task_started / exec_started
		// with a fresh sessionContext{TurnID:…} that would wrongly
		// reset RealtimeActive to false on every action.
		if sc.CollaborationMode != "" {
			ctxState.CollaborationMode = sc.CollaborationMode
		}
		if sc.Personality != "" {
			ctxState.Personality = sc.Personality
		}
		if sc.TruncationMode != "" {
			ctxState.TruncationMode = sc.TruncationMode
		}
		if sc.TruncationLimit > 0 {
			ctxState.TruncationLimit = sc.TruncationLimit
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
				// MessageID is per-event for codex (v1.7.24+ contract)
				// and was set at emit time; only the turn association
				// and (lazy-resolved) model need backfilling here.
				if res.TokenEvents[idx].TurnID == "" {
					res.TokenEvents[idx].TurnID = ctxState.TurnID
				}
				// Defensive: pre-v1.7.24 emitters set MessageID="" on
				// turnless events expecting the legacy turnID-as-message
				// backfill. Keep filling that case for any caller that
				// still emits without MessageID.
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

	// withEffort stamps the current ctxState metadata fields onto an
	// outgoing ToolEvent's Metadata when the parser knows one (and
	// the event doesn't already carry one — defensive against future
	// builders that populate Metadata explicitly). Called at every
	// res.ToolEvents append site so per-turn attribution rides every
	// row that turn produced. Migration 017 added the column;
	// v1.4.52 extended it with codex 0.130+ turn_context fields.
	withEffort := func(ev models.ToolEvent) models.ToolEvent {
		hasMeta := ctxState.EffortLevel != "" || ctxState.CollaborationMode != "" ||
			ctxState.Personality != "" || ctxState.RealtimeActive ||
			ctxState.TruncationMode != "" || ctxState.TruncationLimit > 0 ||
			ctxState.ServiceTier != ""
		if !hasMeta {
			return ev
		}
		if ev.Metadata == nil {
			ev.Metadata = &models.ActionMetadata{}
		}
		if ev.Metadata.EffortLevel == "" && ctxState.EffortLevel != "" {
			ev.Metadata.EffortLevel = ctxState.EffortLevel
		}
		if ev.Metadata.CollaborationMode == "" && ctxState.CollaborationMode != "" {
			ev.Metadata.CollaborationMode = ctxState.CollaborationMode
		}
		if ev.Metadata.Personality == "" && ctxState.Personality != "" {
			ev.Metadata.Personality = ctxState.Personality
		}
		if !ev.Metadata.RealtimeActive && ctxState.RealtimeActive {
			ev.Metadata.RealtimeActive = true
		}
		if ev.Metadata.TruncationMode == "" && ctxState.TruncationMode != "" {
			ev.Metadata.TruncationMode = ctxState.TruncationMode
		}
		if ev.Metadata.TruncationLimit == 0 && ctxState.TruncationLimit > 0 {
			ev.Metadata.TruncationLimit = ctxState.TruncationLimit
		}
		// ServiceTier is the operator's requested OpenAI processing tier
		// ("priority" = Codex Fast mode). Read once per file from
		// ~/.codex/config.toml (the rollout JSONL never carries the value);
		// surfaced as a per-message pill, mirroring claudecode's
		// usage.service_tier capture.
		if ev.Metadata.ServiceTier == "" && ctxState.ServiceTier != "" {
			ev.Metadata.ServiceTier = ctxState.ServiceTier
		}
		return ev
	}

	scanner := bufio.NewScanner(f)
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	var bytesRead int64 = fromOffset
	// Seed from prefetch so :L<linenum>: SourceEventIDs are absolute-
	// file-line-number (stable across re-parses), not chunk-relative.
	lineNum := lineOffset
	for scanner.Scan() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw := scanner.Bytes()
		lineBytes := int64(len(raw))
		if bytesRead+lineBytes < fileSize {
			lineBytes++
		}
		nextOffset := bytesRead + lineBytes
		lineNum++

		if len(raw) == 0 {
			bytesRead = nextOffset
			if nextOffset > res.NewOffset {
				res.NewOffset = nextOffset
			}
			continue
		}
		var line rawLine
		if err := json.Unmarshal(raw, &line); err != nil {
			// A malformed final line is often just an in-progress append:
			// keep the cursor at the last fully parsed line so a later
			// incremental pass can re-read the completed JSON record.
			if nextOffset >= fileSize {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: deferred trailing JSON fragment: %v", lineNum, err))
				break
			}
			bytesRead = nextOffset
			if nextOffset > res.NewOffset {
				res.NewOffset = nextOffset
			}
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
			continue
		}
		bytesRead = nextOffset
		res.NewOffset = nextOffset

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
			res.ToolEvents = append(res.ToolEvents, withEffort(evt))

		case "session_meta":
			var meta sessionMetaPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				applyContext(meta.sessionContext)
				if body := strings.TrimSpace(meta.BaseInstructions.Text); body != "" {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					if evt, ok := a.systemPromptEvent(path, "base", body, ts, ctxState, projectRoot, lineNum, seenSystemPrompts); ok {
						res.ToolEvents = append(res.ToolEvents, withEffort(evt))
					}
				}
			}

		case "session_configured", "session_start", "turn_context":
			var meta turnContextPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				// Lift the nested fields onto the context shape
				// applyContext consumes — they live on
				// collaboration_mode.{mode,settings.reasoning_effort},
				// personality, realtime_active, truncation_policy.*,
				// not on the flat envelope, so the embedded
				// sessionContext can't pick them up directly. v1.4.52
				// added the four non-effort fields after the codex
				// 0.130+ turn_context schema introduced them.
				sc := meta.sessionContext
				if effort := meta.EffortFromPayload(); effort != "" {
					sc.EffortLevel = effort
				}
				if mode := meta.CollaborationMode.Mode; mode != "" {
					sc.CollaborationMode = mode
				}
				if persona := meta.Personality; persona != "" {
					sc.Personality = persona
				}
				if tmode := meta.TruncationPolicy.Mode; tmode != "" {
					sc.TruncationMode = tmode
				}
				if tlimit := meta.TruncationPolicy.Limit; tlimit > 0 {
					sc.TruncationLimit = tlimit
				}
				applyContext(sc)
				// RealtimeActive: bool — can't distinguish absent vs
				// explicit false at JSON-decode time. Always write
				// here in the turn_context handler so each turn_context
				// authoritatively sets the value. Done OUTSIDE
				// applyContext because applyContext is called from
				// other handlers (task_started, exec_started) with a
				// zero-value sessionContext that would wrongly reset
				// it to false on every action.
				ctxState.RealtimeActive = meta.RealtimeActive
				if body := strings.TrimSpace(meta.DeveloperInstructions); body != "" {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					if evt, ok := a.systemPromptEvent(path, "developer", body, ts, ctxState, projectRoot, lineNum, seenSystemPrompts); ok {
						res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
					msg := strings.TrimSpace(am.Message)
					if turnID != "" && msg != "" {
						agentMessages[turnID] = msg
					}
					if msg != "" {
						projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
						sess := ctxState
						sess.TurnID = turnID
						evt := a.buildAgentMessageEvent(path, sess, projectRoot, ts, lineNum, msg)
						res.ToolEvents = append(res.ToolEvents, withEffort(evt))
					}
				}
			case "user_message":
				var um userMessage
				if err := json.Unmarshal(line.Payload, &um); err == nil {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					evt := a.buildUserPromptEvent(path, ctxState, projectRoot, ts, lineNum, um.Message)
					evt.MessageID = userMessageID(um.Message, lineNum)
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
						mergeExecIntoPending(&res.ToolEvents[idx], a, ex, ts)
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
						res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
						res.ToolEvents = append(res.ToolEvents, withEffort(evt))
					}
					// Count for cost-engine billing via the next non-dedup
					// token_count emission's TokenEvent.WebSearchRequests
					// (see runningWebSearchCount declaration). Increment
					// whether the row was emitted standalone or merged
					// into a pending response_item/web_search_call — both
					// paths represent one billable Anthropic/OpenAI
					// server-side web_search call.
					runningWebSearchCount++
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
				res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
				res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
				res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
				}
			case "token_count":
				// Rate-limit snapshot is independent of token-usage: the
				// startup token_count fires with `info: null` but already
				// carries the per-window rate_limits envelope. Emit one
				// ActionRateLimit row per token_count line that carries
				// rate_limits, reusing the schema cowork introduced
				// (RateLimitStatus / Type / ResetsAt / OverageStatus).
				// Dedup is handled at store.Ingest via the stable
				// source_event_id below — re-parses are idempotent.
				if rl := parseModernRateLimits(line.Payload); rl != nil && rl.PlanType != "" {
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					evt := buildCodexRateLimitEvent(path, ctxState, projectRoot, ts, rl, lineNum)
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
				}
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
				// OpenAI / Codex `input_tokens` is the TOTAL prompt
				// count INCLUDING the cached portion; `cached_input_
				// tokens` is a subset of it. The cost engine treats
				// TokenBundle.Input as NET non-cached input (Anthropic
				// shape), so we net here at adapter-emit time to keep
				// the pricing math correct. Without this subtraction,
				// the cached portion gets billed at BOTH the full input
				// rate (as part of Input) AND the discounted cache_read
				// rate (as CacheRead) — observed ~3.4× over-billing on
				// short cached sessions (cost-engine audit 2026-05-24).
				netInput := tk.InputTokens - tk.Cached
				if netInput < 0 {
					netInput = 0
				}
				// v1.7.24+ (migration 032): codex emits one token_count
				// event per model inference (last_token_usage is the
				// per-call delta, NOT cumulative). MessageID is set to
				// the per-event identifier so each inference becomes its
				// own row, matching claudecode's per-API-call MessageID
				// semantic. TurnID groups them back to the user-turn
				// for the dashboard's default rollup view. Pre-v1.7.24
				// rows have turn_id NULL; the dashboard's COALESCE
				// fallback (turn_id → message_id → source_event_id)
				// keeps them rendering correctly.
				perEventID := fmt.Sprintf("tk:%s:L%d", filepath.Base(path), lineNum)
				evt := models.TokenEvent{
					SourceFile:        path,
					SourceEventID:     perEventID,
					SessionID:         ctxState.SessionID,
					ProjectRoot:       projectRoot,
					GitBranch:         ctxState.GitBranch,
					Timestamp:         ts,
					Tool:              models.ToolCodex,
					Model:             modelForTurn(turnID),
					InputTokens:       netInput,
					OutputTokens:      tk.OutputTokens,
					CacheReadTokens:   tk.Cached,
					ReasoningTokens:   tk.Reasoning,
					WebSearchRequests: runningWebSearchCount,
					Source:            models.TokenSourceJSONL,
					Reliability:       models.ReliabilityApproximate,
					MessageID:         perEventID,
					TurnID:            turnID,
					// Codex Fast mode: service_tier:"priority" (read from
					// config.toml) bills at Pricing.FastMultiplier (gpt-5.5
					// 2.5×, gpt-5.4 2×). This is the *requested* tier — the
					// proxy path (api_turns) captures the authoritative
					// served tier from the OpenAI response.
					Fast: ctxState.ServiceTier == "priority",
				}
				runningWebSearchCount = 0
				res.TokenEvents = append(res.TokenEvents, evt)
				// CACHETRACK §15.3 (was: §14.3 scaffold-only). Codex
				// Tier-2 emits CacheTurnObservation under
				// ImplicitCache=true so the engine dispatches to the
				// reduced attribution path
				// (cachetrack.attributeImplicit). Provider exposes
				// cached_input_tokens only (a single scalar) — no
				// cache_control markers, no cache_creation count;
				// the implicit-cache path consumes the scalar via
				// Usage.CacheReadTokens. BlockHashes is intentionally
				// EMPTY (the engine ignores it on the implicit path
				// — it skips the chain push entirely). Idempotency:
				// SourceFile + SourceEventID match the TokenEvent's
				// perEventID, so a re-parse of the same JSONL
				// produces a byte-identical CacheTurnObservation and
				// the cross-tier CacheEventExistsForMessage dedup
				// gate at store.Ingest catches replays.
				if evt.SessionID != "" && evt.Model != "" {
					res.CacheObservations = append(res.CacheObservations, models.CacheTurnObservation{
						SourceFile:    path,
						SourceEventID: perEventID,
						SessionID:     evt.SessionID,
						MessageID:     perEventID,
						Timestamp:     ts,
						Model:         evt.Model,
						Fast:          false, // codex has no speed flag
						BlockHashes:   nil,   // implicit path ignores this
						Usage: models.CacheUsage{
							NetInputTokens:        evt.InputTokens,
							OutputTokens:          evt.OutputTokens,
							CacheReadTokens:       evt.CacheReadTokens,
							CacheCreationTokens:   0,
							CacheCreation1hTokens: 0,
						},
						CompactionSeen: false,
						ImplicitCache:  true,
					})
				}
				if turnID == "" {
					// Token event arrived before the assistant turn
					// boundary (cold-resume or session-startup tokens).
					// Queue for retroactive TurnID + Model backfill once
					// the turn context arrives — see the prelude block.
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
				res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
				// exec_command output carries a footer with the command's
				// TRUE wall time + numeric exit code (e.g. "Wall time: 0.6788
				// seconds\nProcess exited with code 0"). Prefer it over the
				// call->output gap below: the gap includes yield_time_ms +
				// model/queue latency, so it over-states a fast command's
				// runtime. The exit code also gives an accurate Success.
				if row.ActionType == models.ActionRunCommand {
					if wallMs, exitCode, ok := parseExecFooter(body); ok {
						if wallMs > 0 {
							row.DurationMs = wallMs
						}
						row.Success = exitCode == 0
					}
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
				res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
				// semantics.
				//
				// v1.4.53: ALSO emit a standalone ToolEvent so the
				// reasoning is visible in the action stream rather
				// than only feeding PrecedingReasoning downstream.
				// Empty-summary items still emit a row with an opaque
				// placeholder + encrypted byte count so the operator
				// can see that reasoning happened at this position
				// even when the content is unrecoverable.
				var rr responseItemReasoning
				if err := json.Unmarshal(line.Payload, &rr); err == nil {
					text := reasoningSummaryText(rr.Summary)
					if text != "" {
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
					projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
					evt := a.buildCodexReasoningEvent(path, ctxState, projectRoot, ts, rr, text, lineNum)
					if evt.Model == "" {
						if turnID := assistantTurnID(""); turnID != "" {
							evt.Model = modelForTurn(turnID)
							if evt.Model == "" {
								pendingToolModels[turnID] = append(pendingToolModels[turnID], len(res.ToolEvents))
							}
						}
					}
					res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
							res.ToolEvents = append(res.ToolEvents, withEffort(evt))
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
			res.ToolEvents = append(res.ToolEvents, withEffort(evt))

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
			// Net-cumulative input (= gross_input - cached_input) is
			// what we want to delta-track per session. Subtracting
			// cached BEFORE deltaing matches the cost engine's
			// Anthropic-shape contract (TokenBundle.Input is NET
			// non-cached). Otherwise the cached portion gets billed at
			// BOTH the full input rate AND the cache_read rate — see
			// internal/intelligence/cost/engine.go TokenBundle docs and
			// the cost-engine audit 2026-05-24.
			netCum := tk.InputTokens - tk.Cached
			if netCum < 0 {
				netCum = 0
			}
			prev, hasPrev := lastNetInputByID[ctxState.SessionID]
			var netIn int64
			switch {
			case !hasPrev && fromOffset == 0:
				// Fresh parse from start of file — first cumulative IS
				// the delta.
				netIn = netCum
			case !hasPrev && fromOffset > 0:
				// Resume — baseline this event's cumulative net as prev
				// so subsequent events compute correct deltas.
				netIn = 0
			case netCum >= prev:
				netIn = netCum - prev
			default:
				// Negative delta — session reset or upstream resequencing.
				netIn = netCum
			}
			lastNetInputByID[ctxState.SessionID] = netCum
			// Keep the legacy gross tracker live as well so it doesn't
			// drift if any future code path reads it; cheap to update.
			lastInputByID[ctxState.SessionID] = tk.InputTokens

			projectRoot := a.resolveProjectRoot(ctxState.Cwd, rootCache)
			turnID := assistantTurnID("")
			model := firstNonEmpty(tk.Model, modelForTurn(turnID))
			// v1.7.24+ (migration 032): legacy codex token_count is
			// already delta'd at line 1668-1673 (netCum - prev). The
			// per-event MessageID + turn-grouping TurnID match the
			// modern path's contract — one row per token_count event,
			// each row a per-inference delta. See the modern-path
			// comment at the `tk` emission site for the full rationale.
			perEventID := fmt.Sprintf("tk:%s:L%d", filepath.Base(path), lineNum)
			evt := models.TokenEvent{
				SourceFile:        path,
				SourceEventID:     perEventID,
				SessionID:         ctxState.SessionID,
				ProjectRoot:       projectRoot,
				GitBranch:         ctxState.GitBranch,
				Timestamp:         ts,
				Tool:              models.ToolCodex,
				Model:             model,
				InputTokens:       netIn,
				OutputTokens:      tk.OutputTokens,
				CacheReadTokens:   tk.Cached,
				ReasoningTokens:   tk.Reasoning,
				WebSearchRequests: runningWebSearchCount,
				Source:            models.TokenSourceJSONL,
				Reliability:       models.ReliabilityApproximate,
				MessageID:         perEventID,
				TurnID:            turnID,
			}
			runningWebSearchCount = 0
			res.TokenEvents = append(res.TokenEvents, evt)
			// CACHETRACK §15.3 (was: §14.3 scaffold-only). Legacy
			// codex transcripts emit cumulative input_tokens that the
			// adapter pre-deltas at the netCum subtraction above
			// (line ~1668-1673); tk.Cached is fed verbatim per
			// codex's audit doc rationale. See modern-path emission
			// site above for the full ImplicitCache wiring rationale.
			if evt.SessionID != "" && evt.Model != "" {
				res.CacheObservations = append(res.CacheObservations, models.CacheTurnObservation{
					SourceFile:    path,
					SourceEventID: perEventID,
					SessionID:     evt.SessionID,
					MessageID:     perEventID,
					Timestamp:     ts,
					Model:         evt.Model,
					Fast:          false,
					BlockHashes:   nil,
					Usage: models.CacheUsage{
						NetInputTokens:        evt.InputTokens,
						OutputTokens:          evt.OutputTokens,
						CacheReadTokens:       evt.CacheReadTokens,
						CacheCreationTokens:   0,
						CacheCreation1hTokens: 0,
					},
					CompactionSeen: false,
					ImplicitCache:  true,
				})
			}
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
func mergeExecIntoPending(row *models.ToolEvent, a *Adapter, ex execCommandEnd, endTS time.Time) {
	command := commandString(ex.Command)
	output := firstNonEmpty(ex.AggregatedOutput, ex.Stdout+ex.Stderr)
	scrubbedOutput := a.scrubber.String(output)
	success := ex.Status != "failed" && ex.ExitCode == 0
	row.ActionType = models.ActionRunCommand
	row.Target = truncate(a.scrubber.String(command), 200)
	row.Success = success
	row.ErrorMessage = errorIfFailed(success, scrubbedOutput)
	row.DurationMs = ex.Duration.Secs*1000 + ex.Duration.Nanos/1_000_000
	// When exec_command_end omits its structured duration (some Codex builds
	// emit a zero Duration), fall back to the begin(function_call)→end gap, the
	// same fallback the modern function_call_output path uses (§3.2). row's
	// Timestamp is the call's begin time; endTS is this exec_command_end's.
	if row.DurationMs == 0 && !row.Timestamp.IsZero() && !endTS.IsZero() {
		if d := endTS.Sub(row.Timestamp).Milliseconds(); d > 0 {
			row.DurationMs = d
		}
	}
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
	if raw := webSearchRawInput(ws); raw != "" {
		row.RawToolInput = raw
	}
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

// parseExecFooter extracts the command's true wall time (in ms) and numeric
// exit code from an exec_command output footer, which Codex appends as plain
// text lines: "Wall time: 0.6788 seconds" and "Process exited with code 0".
// ok is false when neither line is present (a non-exec output, or a build that
// omits the footer) — the caller then falls back to the call->output gap. The
// scan is tolerant of surrounding output and case.
func parseExecFooter(output string) (wallMs int64, exitCode int, ok bool) {
	if output == "" {
		return 0, 0, false
	}
	for _, ln := range strings.Split(output, "\n") {
		ln = strings.TrimSpace(ln)
		lower := strings.ToLower(ln)
		switch {
		case strings.HasPrefix(lower, "wall time:"):
			f := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ln[len("wall time:"):]), "seconds"))
			f = strings.TrimSpace(strings.TrimSuffix(f, "s"))
			if secs, err := strconv.ParseFloat(f, 64); err == nil && secs >= 0 {
				wallMs = int64(secs * 1000)
				ok = true
			}
		case strings.HasPrefix(lower, "process exited with code"):
			fields := strings.Fields(ln)
			if n := len(fields); n > 0 {
				if code, err := strconv.Atoi(strings.TrimRight(fields[n-1], ".")); err == nil {
					exitCode = code
					ok = true
				}
			}
		}
	}
	return wallMs, exitCode, ok
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

// buildAgentMessageEvent emits a standalone assistant-text row for each
// `event_msg`/`agent_message` line in the rollout. Codex can emit multiple
// agent_messages per turn, so MessageID is content-hash-distinguished within
// the turn (turn_id alone collides on multi-message turns). SourceEventID
// uses the `:L<lineNum>:` format that's stable across re-parses (invariant
// 42). No token/cost fields are set — these rows are observability-only,
// not pricing inputs. Mirrors the Antigravity precedent at
// internal/adapter/antigravity/structured.go:443-461.
// codexServiceTier returns the operator's requested OpenAI processing tier
// ("priority" = Codex Fast mode, "flex" = slow/discount, "default"/"auto"
// = standard) read from the ~/.codex/config.toml that owns the given
// rollout file. The served tier is never recorded in the rollout JSONL
// (confirmed 2026-06-08: service_tier appears only as a tool-schema
// definition there, never a value), so config.toml is the sole node-local
// source on the watcher path; the proxy captures the authoritative served
// tier from the OpenAI response separately. Returns "" when the .codex
// root can't be located or no top-level service_tier is set. Cheap enough
// to call once per rollout file (config is a few KB).
func codexServiceTier(rolloutPath string) string {
	root := codexRootFromRollout(rolloutPath)
	if root == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(root, "config.toml"))
	if err != nil {
		return ""
	}
	return topLevelTomlString(string(data), "service_tier")
}

// codexRootFromRollout walks up from a rollout path
// (<root>/sessions/YYYY/MM/DD/rollout-*.jsonl) to the directory that holds
// the sessions/ tree — i.e. the .codex root that also holds config.toml.
// Returns "" if no sessions ancestor is found.
func codexRootFromRollout(rolloutPath string) string {
	dir := filepath.Dir(rolloutPath)
	for {
		if filepath.Base(dir) == "sessions" {
			return filepath.Dir(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "" // reached the filesystem root without finding sessions/
		}
		dir = parent
	}
}

// topLevelTomlString extracts a top-level (pre-first-table) string key from
// a TOML document without pulling in a full parser. TOML requires bare root
// keys to appear before any [table] header, so the scan stops at the first
// '['. Handles a trailing inline comment (" #...") and single/double
// quotes. Returns "" when the key is absent at the top level.
func topLevelTomlString(doc, key string) string {
	sc := bufio.NewScanner(strings.NewReader(doc))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break // entered a table; root keys are done
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		v = strings.TrimSpace(v)
		if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		return strings.Trim(v, `"'`)
	}
	return ""
}

func (a *Adapter) buildAgentMessageEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, lineNum int, message string) models.ToolEvent {
	turnID := sess.TurnID
	preview := truncate(a.scrubber.String(message), 200)
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("agent:%s:L%d:%s", filepath.Base(sourceFile), lineNum, shortHash(message)),
		SessionID:          sess.SessionID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		GitBranch:          sess.GitBranch,
		Model:              sess.Model,
		Tool:               models.ToolCodex,
		ActionType:         models.ActionTaskComplete,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "codex.assistant_text",
		ToolOutput:         a.scrubber.String(contentcap.Cap(message, contentcap.DefaultMaxBytes)),
		MessageID:          "codex:agent:" + turnID + ":" + shortHash(message),
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

// buildCodexRateLimitEvent emits an ActionRateLimit ToolEvent from a
// Codex token_count.rate_limits envelope, reusing the generic
// schema the cowork adapter introduced (RateLimitStatus / Type /
// ResetsAt / OverageStatus on ActionMetadata). Codex emits two
// windows (primary / secondary) per snapshot — primary's resets_at
// goes onto the dedicated metadata field; the full envelope is
// preserved verbatim in RawToolInput so the dashboard can render
// the dual-window state without losing the secondary window.
//
// Stable source_event_id uses the filename + line number so
// re-parses are idempotent and the unique index dedups across
// scans without us having to track in-memory seenRateLimits state.
func buildCodexRateLimitEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, rl *codexRateLimits, lineNum int) models.ToolEvent {
	status := "ok"
	if rl.RateLimitReachedType != nil && *rl.RateLimitReachedType != "" {
		status = *rl.RateLimitReachedType
	}
	var primaryResetsAt int64
	if rl.Primary != nil {
		primaryResetsAt = rl.Primary.ResetsAt
	}
	meta := &models.ActionMetadata{
		RateLimitStatus:        status,
		RateLimitType:          rl.LimitID,
		RateLimitResetsAt:      primaryResetsAt,
		RateLimitOverageStatus: rl.PlanType,
	}
	rawJSON, _ := json.Marshal(rl)
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("ratelimit:%s:L%d", filepath.Base(sourceFile), lineNum),
		SessionID:     sess.SessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     sess.GitBranch,
		Model:         sess.Model,
		Tool:          models.ToolCodex,
		ActionType:    models.ActionRateLimit,
		Target:        rl.LimitID,
		Success:       rl.RateLimitReachedType == nil || *rl.RateLimitReachedType == "",
		RawToolName:   status,
		RawToolInput:  string(rawJSON),
		MessageID:     fmt.Sprintf("ratelimit:%s:L%d", filepath.Base(sourceFile), lineNum),
		Metadata:      meta,
	}
}

// buildCodexReasoningEvent emits a ToolEvent for each
// response_item.reasoning block. When summary text is present the
// row carries that text in Target/ToolOutput; encrypted-only items
// emit an opaque placeholder noting the encrypted byte count so the
// reasoning's position-in-turn is visible in the action stream.
// Stable source_event_id keys on filename + line so re-parses are
// idempotent under the store's unique index.
func (a *Adapter) buildCodexReasoningEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, rr responseItemReasoning, text string, lineNum int) models.ToolEvent {
	var preview, output string
	if text != "" {
		preview = truncate(text, 200)
		output = a.scrubber.String(contentcap.Cap(text, contentcap.DefaultMaxBytes))
	} else if n := len(rr.EncryptedContent); n > 0 {
		preview = fmt.Sprintf("(encrypted reasoning, %d bytes)", n)
		output = preview
	} else {
		preview = "(reasoning)"
		output = preview
	}
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: fmt.Sprintf("reasoning:%s:L%d", filepath.Base(sourceFile), lineNum),
		SessionID:     sess.SessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     sess.GitBranch,
		Model:         sess.Model,
		Tool:          models.ToolCodex,
		ActionType:    models.ActionTaskComplete,
		Target:        preview,
		Success:       true,
		RawToolName:   "codex.reasoning",
		ToolOutput:    output,
		MessageID:     fmt.Sprintf("reasoning:%s:L%d", filepath.Base(sourceFile), lineNum),
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
		RawToolInput:       a.scrubber.String(webSearchRawInput(ws)),
		MessageID:          firstNonEmpty(ws.TurnID, sess.TurnID),
	}
}

// webSearchRawInput serializes the full web_search_end action payload
// so the dashboard's RawToolInput render shows the multi-query
// fan-out — Codex's web_search tool issues 3-4 sub-queries per
// model-facing call (action.queries[]) and historically only the
// top-level Query string was preserved. Emit JSON when a fan-out
// exists; fall back to the bare query string otherwise so legacy
// renders stay readable for single-query calls.
func webSearchRawInput(ws webSearchEnd) string {
	if len(ws.Action.Queries) > 1 {
		payload := struct {
			Query   string   `json:"query"`
			Queries []string `json:"queries"`
		}{
			Query:   firstNonEmpty(ws.Query, ws.Action.Query),
			Queries: ws.Action.Queries,
		}
		if b, err := json.Marshal(payload); err == nil {
			return string(b)
		}
	}
	return firstNonEmpty(ws.Query, ws.Action.Query, strings.Join(ws.Action.Queries, "; "))
}

func (a *Adapter) buildTaskCompleteEvent(sourceFile string, sess sessionContext, projectRoot string, ts time.Time, done taskComplete, lineNum int) models.ToolEvent {
	if done.CompletedAt > 0 {
		ts = time.Unix(done.CompletedAt, 0).UTC()
	}
	evt := models.ToolEvent{
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
	// time_to_first_token_ms — codex 0.130+ only. Attaches to the
	// task_complete row's metadata so the dashboard's per-action
	// detail view + downstream queries can read it without a schema
	// change. Skipped when zero (older sessions / non-Desktop runs).
	if done.TimeToFirstTokenMS > 0 {
		if evt.Metadata == nil {
			evt.Metadata = &models.ActionMetadata{}
		}
		evt.Metadata.TimeToFirstTokenMS = done.TimeToFirstTokenMS
	}
	return evt
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
	// Run-command-class tools (shell / shell_command / exec_command / powershell
	// / pwsh / cmd.exe / …) carry the command in {"command": [...]|"..."} or
	// {"cmd":"..."}. Branch on the MAPPED ACTION rather than enumerating each
	// tool name, so a new shell alias in actionMap is covered automatically.
	// (exec_command — the modern Codex Desktop / CLI shell tool — was missing
	// from the old name list, leaving Target empty and breaking the
	// process→action message mapping that CorrelateActions keys on.)
	if actionMap[toolName] == models.ActionRunCommand {
		// Codex shell inputs: {"command": ["bash", "-lc", "..."]} or
		// {"command": "..."} (shell/shell_command) or {"cmd": "..."} (exec_command).
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
	}
	switch toolName {
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

// parseModernRateLimits extracts the rate_limits envelope from a
// token_count event_msg payload. Returns nil when the field is
// absent. Independent of the token-usage path because Codex emits
// the startup token_count with `info: null` but rate_limits already
// populated — we want to capture that snapshot too.
func parseModernRateLimits(raw json.RawMessage) *codexRateLimits {
	var probe struct {
		RateLimits *codexRateLimits `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil
	}
	return probe.RateLimits
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

// prefetchSessionContext scans the file's leading bytes (up to `until`)
// for session_meta / session_configured / turn_context lines and returns
// the most recent context fields seen before the resume offset, plus
// the count of lines that preceded the offset. Used by ParseSessionFile
// when fromOffset > 0 so resumed parses inherit:
//
//  1. The SessionID, Cwd, Model, GitBranch, and EffortLevel from the
//     leading session_meta / turn_context — without them every emitted
//     event would be dropped by store.Ingest (empty ProjectRoot is a
//     hard skip) and effort_level metadata would never populate on
//     resumed cycles.
//  2. The absolute line count up to the resume offset. SourceEventIDs
//     that embed `:L<linenum>:` need this to be stable across re-parses
//     — without it, a chunk-relative line number drifts from the
//     absolute one and `observer scan --force` creates duplicate rows.
//
// The function leaves the file cursor positioned wherever Seek-by-caller
// chooses; ParseSessionFile re-seeks to fromOffset right after this
// returns. Returns ok=false only if the file cannot be re-read from
// the start; in that case the caller falls back to the filename-derived
// SessionID, empty cwd, and lineNum starting at 0 (the pre-fix
// behavior — accepting the duplication risk).
func prefetchSessionContext(f *os.File, until int64) (sessionContext, int, bool) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return sessionContext{}, 0, false
	}
	scanner := bufio.NewScanner(f)
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)
	var (
		bytesRead int64
		lineNum   int
		out       sessionContext
	)
	for scanner.Scan() {
		raw := scanner.Bytes()
		bytesRead += int64(len(raw)) + 1 // approximate; trailing newline accounting matches ParseSessionFile
		if bytesRead > until {
			// Don't apply context from a line that straddles the resume
			// offset — the resumed parse will see (and apply) it itself.
			// Also don't bump lineNum for the straddling line — the
			// resumed parse will increment its own counter for it.
			break
		}
		// Count every line that fits before `until`, including empty
		// lines, to mirror ParseSessionFile's lineNum semantics
		// (it increments before the empty-line skip).
		lineNum++
		var line rawLine
		if err := json.Unmarshal(raw, &line); err != nil {
			continue
		}
		switch line.Type {
		case "session_meta":
			var meta sessionMetaPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				out = mergeSessionContext(out, meta.sessionContext)
			}
		case "session_configured", "session_start", "turn_context":
			var meta turnContextPayload
			if err := json.Unmarshal(line.Payload, &meta); err == nil {
				// Lift effort from the nested envelope BEFORE merge —
				// EffortLevel is tagged `json:"-"` so it never populates
				// from the unmarshal directly. Mirrors the live-parse
				// path at line 779-791. Without this, every watcher
				// resume past a `reasoning_effort: "medium"` turn_context
				// dropped effort_level on subsequent events (verified
				// 2026-05-11 on session 019e1743 — empty effort even
				// though the JSONL had medium set).
				sc := meta.sessionContext
				if effort := meta.EffortFromPayload(); effort != "" {
					sc.EffortLevel = effort
				}
				out = mergeSessionContext(out, sc)
			}
		}
	}
	return out, lineNum, true
}

// mergeSessionContext copies non-empty fields from `from` over `into`
// using the same precedence rules ParseSessionFile's applyContext
// closure follows for the parsing pass. Pure value semantics — no
// side effects on pending model maps or queued tool events (those are
// only valid during the live parse).
func mergeSessionContext(into, from sessionContext) sessionContext {
	// Mirror ParseSessionFile's file-ownership rule: first non-empty
	// SessionID seen in the file wins. Required for watcher resumes,
	// where prefetchSessionContext may see both the child session_meta
	// and a replayed parent session_meta before the resumed parse
	// starts emitting rows.
	if into.SessionID == "" {
		if from.ID != "" {
			into.SessionID = from.ID
		}
		if into.SessionID == "" && from.SessionID != "" {
			into.SessionID = from.SessionID
		}
	}
	if from.TurnID != "" {
		into.TurnID = from.TurnID
	}
	if from.Model != "" {
		into.Model = from.Model
	}
	if from.Cwd != "" {
		into.Cwd = from.Cwd
	}
	if from.GitBranch != "" {
		into.GitBranch = from.GitBranch
	}
	// EffortLevel follows the same sticky rule as applyContext — a
	// later non-empty value wins, empty does NOT wipe a prior value.
	// Required for watcher-cycle continuity: when the leading
	// turn_context with `reasoning_effort: "medium"` lives before the
	// resume offset, this is the only path that propagates it into
	// the resumed parse's ctxState.
	if from.EffortLevel != "" {
		into.EffortLevel = from.EffortLevel
	}
	return into
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
