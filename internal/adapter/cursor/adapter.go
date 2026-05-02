// Package cursor implements the hook-driven capture path for Cursor.
// Cursor has no structured native session logs — every action is observed
// via a registered hook fired before/after the user's tool calls.
//
// The package exposes a stateless mapper, BuildEvent, that turns a single
// Cursor hook JSON payload into a normalized models.ToolEvent. The hook CLI
// wraps this with config loading, DB insert, and the approval response.
//
// Coverage gap (audit C2): Cursor's public hook surface emits events for
// shell commands, MCP executions, file edits, and prompt submission — but
// there is no event for file reads. As a result, observer's freshness
// tracking and read-redundancy detection systematically undercount Cursor
// activity relative to Claude Code (which captures every Read tool_use).
// Cursor would need to add a beforeFileRead hook in their CLI for this to
// close; in the meantime cross-tool comparisons should treat Cursor as
// having an "edits-only" view of file activity.
//
// See spec §4.3.
package cursor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Hook event names that Cursor passes via `--<event>` flags or the
// `hook_event_name` payload field. Stable strings — used in event IDs.
const (
	EventBeforeShellCommand = "beforeShellExecution"
	EventBeforeMCPExecution = "beforeMCPExecution"
	EventAfterFileEdit      = "afterFileEdit"
	EventBeforeSubmitPrompt = "beforeSubmitPrompt"
	EventStop               = "stop"
)

// rawHookPayload is the union of fields we read out of any Cursor hook
// payload. Unknown fields are tolerated; missing fields surface as zero
// values. workspace_roots can be a list of strings (older builds) or a list
// of objects with .path (newer builds), so we handle both.
type rawHookPayload struct {
	HookEventName  string          `json:"hook_event_name"`
	ConversationID string          `json:"conversation_id"`
	GenerationID   string          `json:"generation_id"`
	WorkspaceRoots json.RawMessage `json:"workspace_roots"`
	Model          string          `json:"model"`
	Status         string          `json:"status"`
	InputTokens    int64           `json:"input_tokens"`
	OutputTokens   int64           `json:"output_tokens"`
	CacheRead      int64           `json:"cache_read_tokens"`
	CacheWrite     int64           `json:"cache_write_tokens"`
	TranscriptPath string          `json:"transcript_path"`

	// Per-event fields:
	Command  string          `json:"command"`     // beforeShellCommand
	FilePath string          `json:"file_path"`   // afterFileEdit
	Prompt   string          `json:"prompt"`      // beforeSubmitPrompt
	ToolName string          `json:"tool_name"`   // beforeMCPExecution
	Server   string          `json:"server_name"` // beforeMCPExecution
	Input    json.RawMessage `json:"input"`       // beforeMCPExecution
}

// BuildEvent maps a Cursor hook payload to a normalized ToolEvent. The
// caller passes the hook event name (from CLI or payload), the raw JSON
// body, and a scrubber. Returns (event, true) when the payload represents
// a recordable action; (zero, false) when there's nothing to record (the
// `stop` event, which is handled separately as token usage).
//
// Errors indicate malformed input — the caller should log and continue
// (spec §17 row 1) since hooks must never break the host tool.
func BuildEvent(eventName string, body []byte, sc *scrub.Scrubber) (models.ToolEvent, bool, error) {
	var raw rawHookPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		return models.ToolEvent{}, false, fmt.Errorf("cursor.BuildEvent: parse: %w", err)
	}
	// CLI-supplied event name wins; some payloads don't include
	// hook_event_name explicitly.
	if eventName == "" {
		eventName = raw.HookEventName
	}
	if eventName == "" {
		return models.ToolEvent{}, false, errors.New("cursor.BuildEvent: event name missing")
	}
	if eventName == EventStop {
		// Nothing to record — Cursor doesn't pass tool detail on stop.
		return models.ToolEvent{}, false, nil
	}

	projectRoot := decodeWorkspaceRoot(raw.WorkspaceRoots)
	if raw.ConversationID == "" {
		return models.ToolEvent{}, false, errors.New("cursor.BuildEvent: conversation_id missing")
	}

	now := time.Now().UTC()
	ev := models.ToolEvent{
		SourceFile:    "cursor:hook",
		SourceEventID: cursorEventID(raw.GenerationID, eventName, raw),
		SessionID:     raw.ConversationID,
		MessageID:     raw.GenerationID,
		ProjectRoot:   projectRoot,
		Timestamp:     now,
		Model:         raw.Model,
		Tool:          models.ToolCursor,
		Success:       true,
		RawToolName:   eventName,
	}

	switch eventName {
	case EventBeforeShellCommand:
		ev.ActionType = models.ActionRunCommand
		ev.Target = raw.Command
		if sc != nil {
			ev.RawToolInput = sc.String(raw.Command)
		} else {
			ev.RawToolInput = raw.Command
		}
	case EventAfterFileEdit:
		ev.ActionType = models.ActionEditFile
		ev.Target = raw.FilePath
		if sc != nil {
			ev.RawToolInput = sc.String(raw.FilePath)
		} else {
			ev.RawToolInput = raw.FilePath
		}
	case EventBeforeSubmitPrompt:
		ev.ActionType = models.ActionUserPrompt
		ev.MessageID = "user:" + raw.GenerationID
		stripped := stripUserQueryWrapper(raw.Prompt)
		preview := stripped
		if len(preview) > 200 {
			preview = preview[:200]
		}
		ev.Target = preview
		ev.PrecedingReasoning = preview
	case EventBeforeMCPExecution:
		ev.ActionType = models.ActionMCPCall
		ev.Target = strings.TrimSpace(raw.Server + ":" + raw.ToolName)
		if sc != nil {
			ev.RawToolInput = sc.RawJSON(raw.Input)
		} else {
			ev.RawToolInput = string(raw.Input)
		}
	default:
		ev.ActionType = models.ActionUnknown
		ev.RawToolInput = string(body)
	}

	return ev, true, nil
}

// BuildStopTokenEvent maps Cursor's `stop` hook payload to a normalized
// TokenEvent. Cursor emits per-generation token usage only on stop, so
// this is the forward path that populates model + usage for the dashboard.
func BuildStopTokenEvent(body []byte) (models.TokenEvent, bool, error) {
	var raw rawHookPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		return models.TokenEvent{}, false, fmt.Errorf("cursor.BuildStopTokenEvent: parse: %w", err)
	}
	if raw.ConversationID == "" {
		return models.TokenEvent{}, false, errors.New("cursor.BuildStopTokenEvent: conversation_id missing")
	}
	if raw.GenerationID == "" {
		return models.TokenEvent{}, false, errors.New("cursor.BuildStopTokenEvent: generation_id missing")
	}
	if raw.Model == "" {
		return models.TokenEvent{}, false, errors.New("cursor.BuildStopTokenEvent: model missing")
	}
	if raw.InputTokens == 0 && raw.OutputTokens == 0 && raw.CacheRead == 0 && raw.CacheWrite == 0 {
		return models.TokenEvent{}, false, nil
	}

	return models.TokenEvent{
		SourceFile:          "cursor:hook",
		SourceEventID:       raw.GenerationID + ":" + EventStop,
		SessionID:           raw.ConversationID,
		MessageID:           raw.GenerationID,
		ProjectRoot:         decodeWorkspaceRoot(raw.WorkspaceRoots),
		Timestamp:           time.Now().UTC(),
		Tool:                models.ToolCursor,
		Model:               raw.Model,
		InputTokens:         raw.InputTokens,
		OutputTokens:        raw.OutputTokens,
		CacheReadTokens:     raw.CacheRead,
		CacheCreationTokens: raw.CacheWrite,
		Source:              models.TokenSourceHook,
		Reliability:         models.ReliabilityAccurate,
	}, true, nil
}

type transcriptLine struct {
	Role    string `json:"role"`
	Message struct {
		Content []transcriptPart `json:"content"`
	} `json:"message"`
}

type transcriptPart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type transcriptAssistantLine struct {
	LineNumber int
	Parts      []transcriptPart
}

// transcriptUserLine captures the user-role line that opens a turn.
// LineNumber is the 1-indexed file offset; Text is the concatenation of
// all text parts (cursor user lines exclusively contain text parts in
// observed corpora). Stored here so the backfill path can emit a
// user_prompt action without re-walking the file.
type transcriptUserLine struct {
	LineNumber int
	Text       string
}

type transcriptTurn struct {
	User      transcriptUserLine
	Assistant []transcriptAssistantLine
}

func BuildStopTranscriptEvents(body []byte, sc *scrub.Scrubber, ts time.Time) ([]models.ToolEvent, error) {
	var raw rawHookPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("cursor.BuildStopTranscriptEvents: parse: %w", err)
	}
	if raw.TranscriptPath == "" || raw.ConversationID == "" || raw.GenerationID == "" {
		return nil, nil
	}
	turns, err := parseTranscriptTurns(raw.TranscriptPath)
	if err != nil {
		return nil, fmt.Errorf("cursor.BuildStopTranscriptEvents: parse transcript: %w", err)
	}
	if len(turns) == 0 {
		return nil, nil
	}
	return buildTranscriptToolEvents(
		turns[len(turns)-1],
		raw.ConversationID,
		decodeWorkspaceRoot(raw.WorkspaceRoots),
		raw.GenerationID,
		raw.TranscriptPath,
		ts,
		sc,
	), nil
}

func ParseTranscriptTurns(path string) ([]transcriptTurn, error) { return parseTranscriptTurns(path) }

func parseTranscriptTurns(path string) ([]transcriptTurn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var turns []transcriptTurn
	var current *transcriptTurn
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var tl transcriptLine
		if err := json.Unmarshal([]byte(line), &tl); err != nil {
			continue
		}
		switch tl.Role {
		case "user":
			var text strings.Builder
			for _, part := range tl.Message.Content {
				if part.Type == "text" && part.Text != "" {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(part.Text)
				}
			}
			turns = append(turns, transcriptTurn{
				User: transcriptUserLine{LineNumber: lineNo, Text: text.String()},
			})
			current = &turns[len(turns)-1]
		case "assistant":
			if current == nil {
				turns = append(turns, transcriptTurn{})
				current = &turns[len(turns)-1]
			}
			current.Assistant = append(current.Assistant, transcriptAssistantLine{
				LineNumber: lineNo,
				Parts:      tl.Message.Content,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return turns, nil
}

func BuildTranscriptToolEvents(
	turn transcriptTurn,
	sessionID, projectRoot, generationID, sourceFile string,
	ts time.Time,
	sc *scrub.Scrubber,
) []models.ToolEvent {
	return buildTranscriptToolEvents(turn, sessionID, projectRoot, generationID, sourceFile, ts, sc)
}

func buildTranscriptToolEvents(
	turn transcriptTurn,
	sessionID, projectRoot, generationID, sourceFile string,
	ts time.Time,
	sc *scrub.Scrubber,
) []models.ToolEvent {
	var out []models.ToolEvent
	for _, line := range turn.Assistant {
		reasoning := ""
		for partIdx, part := range line.Parts {
			switch part.Type {
			case "text":
				txt := strings.TrimSpace(part.Text)
				if txt != "" {
					reasoning = txt
				}
			case "tool_use":
				rawInput := string(part.Input)
				if sc != nil {
					rawInput = sc.RawJSON(part.Input)
				}
				out = append(out, models.ToolEvent{
					SourceFile:         sourceFile,
					SourceEventID:      fmt.Sprintf("%s:transcript:L%d:P%d:%s", generationID, line.LineNumber, partIdx, shortHash(part.Name+":"+string(part.Input))),
					SessionID:          sessionID,
					MessageID:          generationID,
					ProjectRoot:        projectRoot,
					Timestamp:          ts,
					Tool:               models.ToolCursor,
					ActionType:         cursorTranscriptActionType(part.Name),
					Target:             cursorTranscriptTarget(part.Name, part.Input),
					Success:            true,
					PrecedingReasoning: reasoning,
					RawToolName:        part.Name,
					RawToolInput:       rawInput,
				})
			}
		}
	}
	return out
}

// BuildTranscriptUserPromptEvent emits an ActionUserPrompt event for a
// transcript turn's opening user line. Returns (zero, false) when the
// user line carried no text after stripping. The stripUserQueryWrapper
// helper unwraps the `<user_query>...</user_query>` markers Cursor's
// agent runtime injects around the user's prompt so the DB carries the
// user-typed text rather than the wrapped envelope.
//
// generationID is the cursor `generation_id` for this turn (sourced
// from token_usage rows in the backfill path); MessageID on the row
// becomes "user:" + generationID, matching the live hook path's
// MessageID convention so dashboard joins land cleanly.
func BuildTranscriptUserPromptEvent(
	turn transcriptTurn,
	sessionID, projectRoot, generationID, sourceFile string,
	ts time.Time,
	sc *scrub.Scrubber,
) (models.ToolEvent, bool) {
	stripped := stripUserQueryWrapper(turn.User.Text)
	stripped = strings.TrimSpace(stripped)
	if stripped == "" {
		return models.ToolEvent{}, false
	}
	preview := stripped
	if len(preview) > 200 {
		preview = preview[:200]
	}
	rawInput := stripped
	if sc != nil {
		rawInput = sc.String(stripped)
	}
	return models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      fmt.Sprintf("%s:transcript:L%d:user:%s", generationID, turn.User.LineNumber, shortHash(stripped)),
		SessionID:          sessionID,
		MessageID:          "user:" + generationID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		Tool:               models.ToolCursor,
		ActionType:         models.ActionUserPrompt,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "user_message",
		RawToolInput:       rawInput,
	}, true
}

// stripUserQueryWrapper removes a leading `<user_query>` and trailing
// `</user_query>` envelope when both are present (Cursor's agent
// runtime wraps user prompts in this XML before passing them to the
// model). Returns the original string when only one side is present
// or neither — never strips partial wrappers, since that risks
// damaging real user content that happens to mention the tag name.
func stripUserQueryWrapper(s string) string {
	trimmed := strings.TrimSpace(s)
	const open = "<user_query>"
	const close = "</user_query>"
	if !strings.HasPrefix(trimmed, open) || !strings.HasSuffix(trimmed, close) {
		return s
	}
	inner := trimmed[len(open) : len(trimmed)-len(close)]
	return strings.TrimSpace(inner)
}

func cursorTranscriptActionType(name string) string {
	switch strings.ToLower(name) {
	case "glob", "findfiles":
		return models.ActionSearchFiles
	case "grep", "search", "searchfiles":
		return models.ActionSearchText
	case "readfile", "cat", "readlints":
		// readlints reads diagnostic info for a file — semantically a
		// read, not a separate category, so it folds into ActionReadFile.
		return models.ActionReadFile
	case "shell", "bash", "command":
		return models.ActionRunCommand
	case "applypatch", "editfile", "strreplace":
		// strreplace is the cursor in-place string-edit primitive
		// (analogue of claudecode's Edit tool).
		return models.ActionEditFile
	case "writefile", "createfile":
		return models.ActionWriteFile
	case "subagent", "agent":
		return models.ActionSpawnSubagent
	case "call_mcp_tool":
		return models.ActionMCPCall
	case "await":
		// `Await` is a control-flow primitive the agent uses to wait on
		// a long-running tool call. It carries no file/command target,
		// so we don't lift it to a known category — keep as Unknown
		// rather than mis-classifying. Distinguished from genuinely
		// unmapped tools by the RawToolName preserving "Await".
		return models.ActionUnknown
	default:
		return models.ActionUnknown
	}
}

func cursorTranscriptTarget(name string, raw json.RawMessage) string {
	var input map[string]any
	if err := json.Unmarshal(raw, &input); err != nil {
		return ""
	}
	switch strings.ToLower(name) {
	case "glob", "findfiles":
		return firstString(input, "glob_pattern", "pattern", "query")
	case "grep", "search", "searchfiles":
		return firstString(input, "pattern", "query")
	case "readfile", "writefile", "createfile", "editfile", "applypatch", "readlints", "strreplace":
		return firstString(input, "path", "file_path", "target_file")
	case "shell", "bash", "command":
		return firstString(input, "command")
	case "subagent", "agent":
		return firstString(input, "description", "prompt")
	case "call_mcp_tool":
		// MCP calls carry server + tool name in input. Format match
		// what BuildEvent does for live-hook MCP rows.
		server := firstString(input, "server_name", "server")
		tool := firstString(input, "tool_name", "tool")
		switch {
		case server != "" && tool != "":
			return server + ":" + tool
		case tool != "":
			return tool
		default:
			return server
		}
	default:
		return firstString(input, "path", "pattern", "query", "command")
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		v, ok := m[key]
		if !ok {
			continue
		}
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// decodeWorkspaceRoot pulls the first workspace path out of either shape.
// Returns "" when the payload doesn't include any roots — the store layer
// will skip events without a project root (spec §20 fallback to cwd doesn't
// apply here because the hook process isn't in the user's cwd).
func decodeWorkspaceRoot(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try []string first.
	var asStrings []string
	if err := json.Unmarshal(raw, &asStrings); err == nil && len(asStrings) > 0 {
		return asStrings[0]
	}
	// Then []{path string}.
	var asObjects []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &asObjects); err == nil && len(asObjects) > 0 {
		return asObjects[0].Path
	}
	return ""
}

// cursorEventID derives a deterministic event ID for idempotent inserts.
// Cursor sends generation_id per turn, but a single turn can fire multiple
// hooks of the same event type (rare but possible) so we mix in payload
// fields that distinguish duplicates within a turn.
func cursorEventID(generationID, eventName string, raw rawHookPayload) string {
	id := generationID + ":" + eventName
	switch eventName {
	case EventBeforeShellCommand:
		id += ":" + shortHash(raw.Command)
	case EventAfterFileEdit:
		id += ":" + shortHash(raw.FilePath)
	case EventBeforeMCPExecution:
		id += ":" + shortHash(raw.Server+":"+raw.ToolName)
	}
	return id
}
