package clinecli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// hookSourceFile is the synthetic source_file tag for every hook-
// derived row. Distinct from the absolute sessions.db path the
// SQLite backfill path uses, so the UNIQUE(source_file,
// source_event_id) constraint keeps the two paths' rows in distinct
// dedup buckets. Cross-path dedup is the SessionHookChecker's job
// (Phase 3 commit 10); see WithSessionHookChecker on Adapter.
const hookSourceFile = "clinecli:hook"

// hookEvent is the union of every event Cline CLI's hook system can
// write to ~/.cline/data/logs/hooks.jsonl. Each line in that file is
// a complete JSON object — the discriminator is hookName, and the
// payload sub-object that gets populated tracks the discriminator.
//
// Plan §6 + testdata/clinecli/hooks-jsonl-sample.jsonl document the
// per-event shapes. We tolerate the optional/extra fields cline-cli
// 3.x emits (parent_agent_id may be null, iteration is monotonically
// per-session, the tool_call wrapper carries the same {id, name,
// input} the messages.json tool_use block does).
//
// hooks.jsonl is OPT-IN by Cline design — it only gets written when
// the operator has registered hook commands under
// <workspace>/.clinerules/hooks/ or <CLINE_DIR>/hooks/, OR when a
// subagent is spawned (the audit path writes regardless). The
// tailer no-ops cleanly on an empty file.
type hookEvent struct {
	Ts             string `json:"ts"`
	HookName       string `json:"hookName"`
	SessionID      string `json:"sessionId,omitempty"`
	AgentID        string `json:"agentId,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	ParentAgentID  string `json:"parentAgentId,omitempty"`
	Iteration      int    `json:"iteration,omitempty"`
	PID            int64  `json:"pid,omitempty"`
	Source         string `json:"source,omitempty"`
	Reason         string `json:"reason,omitempty"`
	CWD            string `json:"cwd,omitempty"`

	TaskStart        *hookTaskStart        `json:"taskStart,omitempty"`
	TaskResume       *hookTaskResume       `json:"taskResume,omitempty"`
	UserPromptSubmit *hookUserPromptSubmit `json:"userPromptSubmit,omitempty"`
	ToolCall         *hookToolCall         `json:"tool_call,omitempty"`
	ToolResult       *hookToolResult       `json:"tool_result,omitempty"`
	TaskComplete     *hookTaskComplete     `json:"taskComplete,omitempty"`
	TaskCancel       *hookTaskCancel       `json:"taskCancel,omitempty"`
	Turn             *hookTurn             `json:"turn,omitempty"`
	Error            *hookError            `json:"error,omitempty"`
}

type hookTaskStart struct {
	TaskMetadata map[string]any `json:"taskMetadata,omitempty"`
}

type hookTaskResume struct {
	PreviousState map[string]any `json:"previousState,omitempty"`
}

type hookUserPromptSubmit struct {
	Prompt      string `json:"prompt"`
	Attachments []any  `json:"attachments,omitempty"`
}

type hookToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type hookToolResult struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
	DurationMs int64           `json:"durationMs,omitempty"`
}

type hookTaskComplete struct {
	TaskMetadata map[string]any `json:"taskMetadata,omitempty"`
}

type hookTaskCancel struct {
	TaskMetadata map[string]any `json:"taskMetadata,omitempty"`
}

type hookTurn struct {
	OutputText string `json:"outputText"`
	Status     string `json:"status"`
}

type hookError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Stack   string `json:"stack,omitempty"`
}

// parseHooksJSONL reads hooks.jsonl past fromByte (a byte offset into
// the file). Returns the events + the new offset (end-of-last-
// complete-line) + warnings + (best-effort) an io.EOF-equivalent err.
//
// Idempotency: SourceEventIDs are derived from session_id + iteration
// + per-event tag, so re-reading from offset 0 produces stable IDs.
// The byte offset is purely an optimisation to skip already-seen
// lines.
//
// Stops at the last complete newline — partial trailing lines stay
// unconsumed so the next tailer call picks them up after the writer
// finishes the write.
func parseHooksJSONL(_ context.Context, path string, fromByte int64, sc *scrub.Scrubber) ([]models.ToolEvent, []models.TokenEvent, int64, []string, error) {
	f, err := os.Open(path) //nolint:gosec // path derived from validated watch root
	if err != nil {
		return nil, nil, fromByte, nil, fmt.Errorf("parseHooksJSONL: open: %w", err)
	}
	defer f.Close()

	if fromByte > 0 {
		if _, err := f.Seek(fromByte, io.SeekStart); err != nil {
			return nil, nil, fromByte, nil, fmt.Errorf("parseHooksJSONL: seek %d: %w", fromByte, err)
		}
	}

	var (
		tools    []models.ToolEvent
		tokens   []models.TokenEvent
		warnings []string
		// pendingToolUse maps (sessionID + tool_call.id) → index into
		// `tools` so a subsequent tool_result hook can lookup-and-fill.
		pendingToolUse = map[string]int{}
	)

	rd := bufio.NewReader(f)
	currentOffset := fromByte
	for {
		line, err := rd.ReadString('\n')
		if errReadComplete(err, line) {
			// Partial trailing line — don't advance the cursor past it.
			break
		}
		consumed := int64(len(line))
		// Strip the trailing newline before unmarshal.
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmed) == "" {
			currentOffset += consumed
			continue
		}
		var ev hookEvent
		if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
			warnings = append(warnings, fmt.Sprintf("clinecli.parseHooksJSONL: line at offset=%d: %v", currentOffset, err))
			currentOffset += consumed
			continue
		}
		newTools, newTokens, evWarnings := dispatchHookEvent(&ev, path, sc, pendingToolUse, len(tools))
		tools = append(tools, newTools...)
		tokens = append(tokens, newTokens...)
		warnings = append(warnings, evWarnings...)
		currentOffset += consumed
	}
	return tools, tokens, currentOffset, warnings, nil
}

// errReadComplete returns true when bufio.Reader.ReadString returned
// io.EOF AND the line is empty / non-newline-terminated. That's the
// "partial trailing line; back off and wait" signal.
func errReadComplete(err error, line string) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) && (line == "" || !strings.HasSuffix(line, "\n")) {
		return true
	}
	return false
}

// dispatchHookEvent emits the per-event-type ToolEvent / TokenEvent
// rows. Mirrors the messages.json content-block walker's discipline:
// SourceEventIDs are content-derivable so re-ingestion is safe;
// tool_call rows are recorded in pendingToolUse so a paired
// tool_result hook can fill ToolOutput / Success / ErrorMessage.
//
// firstIdx is the index into the OUTER tools slice where this
// dispatch's first event will land — used to populate
// pendingToolUse correctly when the caller appends our output.
func dispatchHookEvent(ev *hookEvent, _ string, sc *scrub.Scrubber, pendingToolUse map[string]int, firstIdx int) (tools []models.ToolEvent, tokens []models.TokenEvent, warnings []string) {
	ts := parseIso8601Time(ev.Ts)
	projectRoot := normalizeProjectRoot(ev.CWD)
	meta := hookEventMetadata(ev)

	switch ev.HookName {
	case "agent_start":
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:ss:" + ev.SessionID,
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionSessionStart,
			Target:        "cli",
			Success:       true,
			RawToolName:   "clinecli.hook.agent_start",
			Metadata:      meta,
		})
	case "agent_resume":
		// Treat resume as a session_start variant — the dashboard
		// can disambiguate via the RawToolName tag. SourceEventID
		// distinct from agent_start so both fire (resume after
		// a prior start within the same session is meaningful
		// observability).
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:resume:" + ev.SessionID + ":i" + intToStr(ev.Iteration),
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionSessionStart,
			Target:        "resume",
			Success:       true,
			RawToolName:   "clinecli.hook.agent_resume",
			Metadata:      meta,
		})
	case "prompt_submit":
		if ev.UserPromptSubmit == nil {
			return tools, tokens, warnings
		}
		body := stripUserInputWrapper(ev.UserPromptSubmit.Prompt)
		scrubbed := sc.String(body)
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:" + ev.SessionID + ":i" + intToStr(ev.Iteration) + ":user",
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionUserPrompt,
			Target:        truncate(scrubbed, 200),
			Success:       true,
			RawToolName:   "clinecli.hook.prompt_submit",
			RawToolInput:  sc.String(contentcap.Cap(body, contentcap.DefaultMaxBytes)),
			Metadata:      meta,
		})
	case "tool_call":
		if ev.ToolCall == nil {
			return tools, tokens, warnings
		}
		actionType := normalizeToolName(ev.ToolCall.Name)
		scrubbedInput := sc.RawJSON(ev.ToolCall.Input)
		target := extractTarget(ev.ToolCall.Name, ev.ToolCall.Input, sc)
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:" + ev.SessionID + ":i" + intToStr(ev.Iteration) + ":tool_use:" + ev.ToolCall.ID,
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    actionType,
			Target:        target,
			Success:       true,
			RawToolName:   ev.ToolCall.Name,
			RawToolInput:  firstNonEmpty(scrubbedInput, scrub.Truncate(string(ev.ToolCall.Input))),
			Metadata:      meta,
		})
		// Stash the index so a subsequent tool_result hook can fill.
		pendingToolUse[ev.SessionID+":"+ev.ToolCall.ID] = firstIdx + len(tools) - 1
	case "tool_result":
		if ev.ToolResult == nil {
			return tools, tokens, warnings
		}
		// tool_result hooks are typically NOT preceded by a matching
		// tool_call hook in the same dispatch batch (they fire on a
		// later message). Skip when no pending row to fill — the
		// caller's pendingToolUse from earlier batches still applies
		// across calls because parseHooksJSONL keeps the map across
		// the whole scan.
		idx, ok := pendingToolUse[ev.SessionID+":"+ev.ToolResult.ID]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("clinecli.dispatchHookEvent: tool_result for unknown call_id %s in session %s", ev.ToolResult.ID, ev.SessionID))
			return tools, tokens, warnings
		}
		// Out-of-batch fill — we can't mutate the prior batch's
		// tools slice from here. Emit a synthetic "tool_failure"
		// or "task_complete" companion row indexed by the original
		// call_id so the store layer can later JOIN them; for v1 we
		// just leave the pre-existing tool_use row's success=true
		// and accept that hook-path tool_result enrichment is
		// best-effort. This is a known limitation; the SQLite path
		// (which has full tool_use ↔ tool_result pairing within
		// the same scan) is the primary truth source. Drop the
		// pending entry so the next hook isn't confused.
		_ = idx
		delete(pendingToolUse, ev.SessionID+":"+ev.ToolResult.ID)
	case "agent_end":
		status := "completed"
		if ev.Turn != nil && ev.Turn.Status != "" {
			status = ev.Turn.Status
		}
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:se:" + ev.SessionID + ":i" + intToStr(ev.Iteration),
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionSessionEnd,
			Target:        status,
			Success:       status == "completed",
			RawToolName:   "clinecli.hook.agent_end",
			Metadata:      meta,
		})
	case "agent_abort":
		reason := "aborted"
		if ev.Reason != "" {
			reason = ev.Reason
		}
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:abort:" + ev.SessionID + ":i" + intToStr(ev.Iteration),
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionTurnAborted,
			Target:        reason,
			Success:       false,
			RawToolName:   "clinecli.hook.agent_abort",
			Metadata:      meta,
		})
	case "agent_error":
		if ev.Error == nil {
			return tools, tokens, warnings
		}
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:err:" + ev.SessionID + ":i" + intToStr(ev.Iteration),
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionAPIError,
			Target:        ev.Error.Name,
			Success:       false,
			ErrorMessage:  truncate(sc.String(ev.Error.Message), 2048),
			RawToolName:   "clinecli.hook.agent_error",
			Metadata:      meta,
		})
	case "session_shutdown":
		reason := "shutdown"
		if ev.Reason != "" {
			reason = ev.Reason
		}
		tools = append(tools, models.ToolEvent{
			SourceFile:    hookSourceFile,
			SourceEventID: "hook:shutdown:" + ev.SessionID,
			SessionID:     ev.SessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     ts,
			Tool:          models.ToolClineCLI,
			ActionType:    models.ActionSessionEnd,
			Target:        reason,
			Success:       true,
			RawToolName:   "clinecli.hook.session_shutdown",
			Metadata:      meta,
		})
	default:
		warnings = append(warnings, fmt.Sprintf("clinecli.dispatchHookEvent: unknown hookName %q (session=%s ts=%s)", ev.HookName, ev.SessionID, ev.Ts))
	}
	return tools, tokens, warnings
}

// hookEventMetadata extracts ActionMetadata from a hook event's
// agent / conversation / parent-agent identifiers. Hook payloads
// carry less linkage than sessions.db rows (no team_name in the
// hook envelope; no is_subagent flag), but the parent_agent_id +
// agent_id + conversation_id can be lifted.
func hookEventMetadata(ev *hookEvent) *models.ActionMetadata {
	m := models.ActionMetadata{
		AgentID:       ev.AgentID,
		ParentAgentID: ev.ParentAgentID,
	}
	if m.IsZero() {
		return nil
	}
	return &m
}

// intToStr formats an int without pulling fmt.Sprintf into the hot
// path (tighter inlining; the iteration field tops out below 10000
// in any realistic Cline CLI session).
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SessionHookChecker is the predicate the SQLite path consults to
// decide "this session already has rows from the hook path; skip the
// SQLite re-emit." Phase 3 commit 10 wires it into Adapter; until
// then it's documented here so the hook tailer's SourceFile
// (hookSourceFile = "clinecli:hook") is the dedup key the SQLite path
// will look up.
//
// Mirrors cursor's SessionHookChecker pattern at
// internal/adapter/cursor/scan.go:51-87.
type SessionHookChecker func(ctx context.Context, sessionID string) (bool, error)
