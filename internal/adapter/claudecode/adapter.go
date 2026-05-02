package claudecode

import (
	"bufio"
	"context"
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

// Adapter parses Claude Code's JSONL session logs under
// ~/.claude/projects/<encoded-path>/<session-id>.jsonl. See spec §4.1.
type Adapter struct {
	scrubber *scrub.Scrubber
	// watchRoot is the directory scanned for session files. Defaults to
	// ~/.claude/projects when empty.
	watchRoot string
}

// New returns a Claude Code adapter with the default scrubber and
// watch root (~/.claude/projects).
func New() *Adapter {
	return &Adapter{scrubber: scrub.New()}
}

// NewWithOptions returns an adapter with customized scrubber and/or watch
// root. Either argument may be zero value to use defaults.
func NewWithOptions(s *scrub.Scrubber, watchRoot string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	return &Adapter{scrubber: s, watchRoot: watchRoot}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolClaudeCode }

// WatchPaths implements adapter.Adapter.
//
// Returns ".claude/projects" under every cross-mount-resolved $HOME so
// observer running in WSL2 picks up Claude Code sessions from a
// /mnt/c/Users/<u>/.claude/projects tree (and vice-versa from a
// Windows host with WSL distros mounted via \\wsl.localhost\). The
// subpath is identical across OSes — Claude Code uses ~/.claude on
// Linux, macOS, and Windows alike — so no per-home OS branching is
// needed here.
func (a *Adapter) WatchPaths() []string {
	if a.watchRoot != "" {
		return []string{a.watchRoot}
	}
	var roots []string
	for _, h := range crossmount.AllHomes() {
		roots = append(roots, filepath.Join(h.Path, ".claude", "projects"))
	}
	return roots
}

// IsSessionFile implements adapter.Adapter. Claude Code session files end in
// .jsonl under the projects tree.
func (*Adapter) IsSessionFile(path string) bool {
	return filepath.Ext(path) == ".jsonl"
}

// nativeTools is the set of Claude Code tool names that map to native actions
// (as opposed to Bash shell invocations).
var nativeTools = map[string]struct{}{
	"Read":            {},
	"Write":           {},
	"Edit":            {},
	"Grep":            {},
	"Glob":            {},
	"WebSearch":       {},
	"WebFetch":        {},
	"Agent":           {},
	"TaskCreate":      {},
	"TaskUpdate":      {},
	"TaskList":        {},
	"TaskGet":         {},
	"TaskOutput":      {},
	"TaskStop":        {},
	"AskUserQuestion": {},
}

// actionMap translates Claude Code tool names to the normalized taxonomy.
var actionMap = map[string]string{
	"Read":      models.ActionReadFile,
	"Write":     models.ActionWriteFile,
	"Edit":      models.ActionEditFile,
	"Bash":      models.ActionRunCommand,
	"Grep":      models.ActionSearchText,
	"Glob":      models.ActionSearchFiles,
	"WebSearch": models.ActionWebSearch,
	"WebFetch":  models.ActionWebFetch,
	// Agent is Claude Code's sub-agent launcher. Each Agent call kicks
	// off a sub-agent runtime; that sub-agent's activity is logged
	// inline in the same JSONL session with `isSidechain: true` markers
	// per line — NOT as a separate session_id (a common misconception).
	// Tagging the parent's tool_use as spawn_subagent lets users count
	// fan-out distinctly from regular tool work.
	"Agent": models.ActionSpawnSubagent,
	// TaskCreate / TaskUpdate / TaskList / TaskGet / TaskOutput /
	// TaskStop are the structured-todo-list tools (the local equivalent
	// of an internal task tracker the agent uses to plan its own work).
	// Map all six to ActionTodoUpdate so the Actions tab can filter
	// the agent's planning chatter as a single bucket.
	"TaskCreate": models.ActionTodoUpdate,
	"TaskUpdate": models.ActionTodoUpdate,
	"TaskList":   models.ActionTodoUpdate,
	"TaskGet":    models.ActionTodoUpdate,
	"TaskOutput": models.ActionTodoUpdate,
	"TaskStop":   models.ActionTodoUpdate,
	// AskUserQuestion is Claude Code's interactive prompt tool — maps
	// to the existing ActionAskUser constant.
	"AskUserQuestion": models.ActionAskUser,
}

// rawLine is the shape of a single JSONL record we care about. Claude Code's
// actual format is richer; we decode only the fields we need and let extras
// be ignored.
type rawLine struct {
	SessionID string          `json:"sessionId"`
	GitBranch string          `json:"gitBranch"`
	Cwd       string          `json:"cwd"`
	UUID      string          `json:"uuid"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Message   json.RawMessage `json:"message"`
	// Error is populated on type="system", subtype="api_error" records
	// that Claude Code writes when the upstream API rejects a request
	// (content-policy block, rate limit, invalid request, etc.).
	// Captured into ActionAPIError rows so failures show up on the
	// dashboard alongside the tool calls — pre-v1.4.20 these were
	// silently dropped because the adapter skipped records without a
	// `message` field.
	Error json.RawMessage `json:"error"`
	// IsSidechain marks lines emitted inside a sub-agent runtime
	// (spawned via the parent's `Agent` tool). The sub-agent shares
	// the parent's session_id but every line inside its execution
	// gets this flag set true. Used to segment cross-thread
	// redundancy on the Discovery tab and surface sub-agent volume
	// on the Sessions tab.
	IsSidechain bool `json:"isSidechain"`
}

type rawMessage struct {
	// ID is the Anthropic msg_* identifier when this is an assistant
	// message produced by an API call. One API call can produce N JSONL
	// records (1 per content block: text + tool_use × N), all sharing the
	// same ID and echoing the same accumulating usage envelope. Used as
	// the dedup key for token events.
	ID    string `json:"id"`
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content is either a JSON array of rawContentBlock or a plain string
	// (for short text-only messages). decodeContent handles both.
	Content json.RawMessage `json:"content"`
	Usage   *rawUsage       `json:"usage"`
}

// decodeContent returns the content blocks regardless of whether the source
// encoded content as an array or as a bare string.
func decodeContent(raw json.RawMessage) []rawContentBlock {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var blocks []rawContentBlock
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil
		}
		return blocks
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil
		}
		return []rawContentBlock{{Type: "text", Text: s}}
	}
	return nil
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

type rawContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	// CacheCreation is the per-tier breakdown Anthropic emits when the
	// caller opts into 1h ephemeral caching. The JSONL stream mirrors
	// the API response, so the adapter sees the same shape the proxy
	// does. Older sessions don't include this object — fields stay zero.
	CacheCreation struct {
		Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// ParseSessionFile implements adapter.Adapter.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("claudecode.ParseSessionFile: open %s: %w", path, err)
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("claudecode.ParseSessionFile: seek: %w", err)
		}
	}

	res := adapter.ParseResult{NewOffset: fromOffset}
	// Index of toolu_id → position in res.ToolEvents so we can update
	// success/error when the paired tool_result appears later.
	pending := map[string]int{}
	// Index of message.id → position in res.TokenEvents. One API call writes
	// N JSONL records (one per content block) with the same msg.id and a
	// progressing cumulative usage envelope. Last write wins (highest
	// output_tokens), so collapse same-msg.id events into a single
	// TokenEvent rather than emitting N rows that the cost engine would
	// then sum up.
	msgIDToIdx := map[string]int{}
	// Cache of project root per cwd.
	rootCache := map[string]string{}
	reasoningByTurn := []string{}

	scanner := bufio.NewScanner(f)
	// Claude Code lines can be very long (large tool outputs). Bump buffer.
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	var bytesRead int64 = fromOffset
	lineNum := 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw := scanner.Bytes()
		// Each scanned token excludes its trailing newline. Account for it
		// when advancing the offset; assume \n (Claude Code writes LF).
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
		// Update the offset only after successfully *handling* the line so
		// a malformed line doesn't stall progress.
		res.NewOffset = bytesRead

		// API error envelopes — type=system, subtype=api_error. These
		// records have no `message` field (just `error`) so the
		// len(Message)==0 short-circuit below would drop them. Decode
		// the nested error and emit an ActionAPIError row so
		// content-policy blocks, rate limits, and the rest are visible
		// on the Actions / Sessions tabs.
		if line.Type == "system" && line.Subtype == "api_error" && len(line.Error) > 0 {
			ts := parseTimestamp(line.Timestamp)
			projectRoot := a.resolveProjectRoot(line.Cwd, rootCache)
			ev := buildAPIErrorEvent(path, line, ts, projectRoot)
			if ev != nil {
				res.ToolEvents = append(res.ToolEvents, *ev)
			}
			continue
		}

		if len(line.Message) == 0 {
			continue
		}
		var msg rawMessage
		if err := json.Unmarshal(line.Message, &msg); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed message: %v", lineNum, err))
			continue
		}

		ts := parseTimestamp(line.Timestamp)
		projectRoot := a.resolveProjectRoot(line.Cwd, rootCache)

		if msg.Usage != nil {
			// Drop Claude Code's synthetic placeholder rows. The CLI emits
			// `model: "<synthetic>"` lines for compaction/subagent stitching
			// events that don't correspond to a real API call; in the live
			// install all such rows carry zero usage anyway, but filtering
			// at the adapter keeps them out of "unknown model" reports
			// and per-model breakdowns. Audit item C4.
			if msg.Model == "<synthetic>" {
				continue
			}
			// Prefer message.id as the dedup key — one API call shares it
			// across N content-block records — and fall back to the
			// per-record UUID when the JSONL line predates the id field
			// or is a non-API-call assistant entry.
			eventID := msg.ID
			if eventID == "" {
				eventID = line.UUID
			}
			cacheCreation := msg.Usage.CacheCreationInputTokens
			if cacheCreation == 0 {
				cacheCreation = msg.Usage.CacheCreation.Ephemeral5mInputTokens +
					msg.Usage.CacheCreation.Ephemeral1hInputTokens
			}
			ev := models.TokenEvent{
				SourceFile:            path,
				SourceEventID:         eventID,
				SessionID:             line.SessionID,
				ProjectRoot:           projectRoot,
				GitBranch:             line.GitBranch,
				Timestamp:             ts,
				Tool:                  models.ToolClaudeCode,
				Model:                 msg.Model,
				InputTokens:           msg.Usage.InputTokens,
				OutputTokens:          msg.Usage.OutputTokens,
				CacheReadTokens:       msg.Usage.CacheReadInputTokens,
				CacheCreationTokens:   cacheCreation,
				CacheCreation1hTokens: msg.Usage.CacheCreation.Ephemeral1hInputTokens,
				Source:                models.TokenSourceJSONL,
				Reliability:           models.ReliabilityUnreliable,
				MessageID:             msg.ID,
			}
			if msg.ID != "" {
				if idx, ok := msgIDToIdx[msg.ID]; ok {
					// Streaming usage progresses monotonically — keep the
					// later record (largest cumulative output_tokens). Don't
					// `continue` the outer loop — content blocks for this
					// JSONL line are still distinct from prior lines'
					// blocks and must be processed below.
					if ev.OutputTokens >= res.TokenEvents[idx].OutputTokens {
						res.TokenEvents[idx] = ev
					}
				} else {
					msgIDToIdx[msg.ID] = len(res.TokenEvents)
					res.TokenEvents = append(res.TokenEvents, ev)
				}
			} else {
				res.TokenEvents = append(res.TokenEvents, ev)
			}
		}

		blocks := decodeContent(msg.Content)

		// Emit a user_prompt action for user-role lines that carry text
		// content. Mirrors what every other adapter produces so the
		// per-message timeline shows a "user:<id>" row separating turns.
		// Tool-result-only user messages (programmatic responses to the
		// model) don't trigger this — their content is tool_result blocks,
		// not text — so the existing block loop below handles them
		// unchanged.
		if msg.Role == "user" {
			if text := userPromptText(blocks); text != "" {
				truncated := text
				if len(truncated) > 200 {
					truncated = truncated[:200]
				}
				res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
					SourceFile:         path,
					SourceEventID:      line.UUID,
					SessionID:          line.SessionID,
					ProjectRoot:        projectRoot,
					Timestamp:          ts,
					GitBranch:          line.GitBranch,
					Tool:               models.ToolClaudeCode,
					ActionType:         models.ActionUserPrompt,
					Target:             truncated,
					Success:            true,
					PrecedingReasoning: truncated,
					RawToolName:        "user_message",
					RawToolInput:       a.scrubber.String(text),
					IsSidechain:        line.IsSidechain,
					MessageID:          "user:" + line.UUID,
				})
			}
		}

		for _, block := range blocks {
			switch block.Type {
			case "text":
				if msg.Role == "assistant" && strings.TrimSpace(block.Text) != "" {
					reasoningByTurn = appendCapped(reasoningByTurn, block.Text, 20)
				}
			case "tool_use":
				evt := a.toolUseEvent(path, line, block, projectRoot, ts, msg.ID)
				evt.PrecedingReasoning = truncateReasoning(lastReasoning(reasoningByTurn))
				idx := len(res.ToolEvents)
				res.ToolEvents = append(res.ToolEvents, evt)
				if block.ID != "" {
					pending[block.ID] = idx
				}
			case "tool_result":
				if idx, ok := pending[block.ToolUseID]; ok {
					body := decodeResultContent(block.Content)
					scrubbed := a.scrubber.String(body)
					res.ToolEvents[idx].ToolOutput = scrubbed
					if block.IsError {
						res.ToolEvents[idx].Success = false
						res.ToolEvents[idx].ErrorMessage = truncateResult(scrubbed)
					}
					// Wall-clock duration: gap between the tool_use's
					// assistant-message timestamp and the tool_result's
					// user-message timestamp. Anthropic's JSONL doesn't
					// emit a structured per-tool elapsed field, so the
					// successor-timestamp delta is the only signal we
					// have. Skip when either timestamp is zero (legacy
					// rows) or the gap is negative (clock skew).
					call := &res.ToolEvents[idx]
					if call.DurationMs == 0 && !call.Timestamp.IsZero() && !ts.IsZero() {
						if d := ts.Sub(call.Timestamp).Milliseconds(); d > 0 {
							call.DurationMs = d
						}
					}
					delete(pending, block.ToolUseID)
				}
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return res, fmt.Errorf("claudecode.ParseSessionFile: scan: %w", err)
	}

	return res, nil
}

// toolUseEvent builds a ToolEvent from a tool_use content block. The
// messageID parameter is the parent assistant message's Anthropic id
// (msg_xxx) — same upstream message that produced this tool call.
// Empty when the block has no parent message id (legacy JSONL).
func (a *Adapter) toolUseEvent(
	sourceFile string,
	line rawLine,
	block rawContentBlock,
	projectRoot string,
	ts time.Time,
	messageID string,
) models.ToolEvent {
	rawInput := string(block.Input)
	scrubbedInput := a.scrubber.RawJSON(block.Input)

	actionType, ok := actionMap[block.Name]
	if !ok {
		actionType = models.ActionUnknown
	}

	target := a.extractTarget(block.Name, block.Input, projectRoot)
	return models.ToolEvent{
		SourceFile:    sourceFile,
		SourceEventID: block.ID,
		SessionID:     line.SessionID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		GitBranch:     line.GitBranch,
		Tool:          models.ToolClaudeCode,
		ActionType:    actionType,
		Target:        target,
		Success:       true,
		RawToolName:   block.Name,
		RawToolInput:  firstNonEmpty(scrubbedInput, scrub.Truncate(rawInput)),
		IsSidechain:   line.IsSidechain,
		MessageID:     messageID,
	}
}

// IsNativeTool reports whether a Claude Code tool name is one of the native
// (non-Bash) tools. Used by the store layer to set actions.is_native_tool.
func IsNativeTool(name string) bool {
	_, ok := nativeTools[name]
	return ok
}

func (a *Adapter) extractTarget(toolName string, rawInput []byte, projectRoot string) string {
	var input map[string]any
	if len(rawInput) == 0 {
		return ""
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}

	switch toolName {
	case "Read", "Write", "Edit":
		if fp := pick("file_path"); fp != "" {
			if projectRoot != "" {
				return git.RelativePath(projectRoot, fp)
			}
			return fp
		}
	case "Bash":
		// Bash targets are scrubbed commands — always run them through the
		// scrubber even though raw_tool_input was already scrubbed.
		return a.scrubber.String(pick("command"))
	case "Grep":
		return pick("pattern")
	case "Glob":
		return pick("pattern")
	case "WebSearch":
		return pick("query")
	case "WebFetch":
		return pick("url")
	}
	return ""
}

// resolveProjectRoot is cached per cwd because git.Resolve walks the
// filesystem — Claude Code sessions often contain hundreds of events sharing
// one cwd.
func (a *Adapter) resolveProjectRoot(cwd string, cache map[string]string) string {
	if cwd == "" {
		return ""
	}
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

// buildAPIErrorEvent decodes a Claude Code system/api_error JSONL
// record into an ActionAPIError tool event. The actual upstream
// payload lives at line.Error and has the shape:
//
//	{
//	  "status": 400,
//	  "headers": {...},
//	  "requestID": "req_011...",
//	  "error": {
//	    "type": "invalid_request_error",
//	    "message": "Output blocked by content filtering policy",
//	    "details": null
//	  },
//	  "type": "..."
//	}
//
// We map fields onto Action columns:
//
//   - Target → upstream request_id (joinable to api_turns.request_id
//     when both proxy + JSONL saw the same call)
//   - RawToolName → upstream error class
//     (invalid_request_error / rate_limit_error / overloaded_error)
//   - ErrorMessage → human-readable message after secrets scrubbing
//   - Success → false
//
// Returns nil when the line lacks the minimum fields the row needs
// (request id + non-empty message); these are recorded as a warning
// so silent ingest gaps surface in `observer status`.
func buildAPIErrorEvent(path string, line rawLine, ts time.Time, projectRoot string) *models.ToolEvent {
	var env struct {
		Status    int             `json:"status"`
		RequestID string          `json:"requestID"`
		Type      string          `json:"type"`  // outer type — sometimes the specific class
		Error     json.RawMessage `json:"error"` // nested error envelope (1–2 levels deep in live JSONL)
	}
	if err := json.Unmarshal(line.Error, &env); err != nil {
		return nil
	}
	// Walk the nested error chain; in live Claude Code logs the leaf
	// carries both the specific class (overloaded_error /
	// invalid_request_error / rate_limit_error) and the human message.
	// The outer envelope's `type` is sometimes the same specific class
	// but other times just the generic "error" string — prefer leaf,
	// fall back to outer.
	errType, message := findInnermostAPIError(env.Error)
	if errType == "" || errType == "error" {
		errType = env.Type
	}
	if errType == "" || errType == "error" {
		errType = "api_error"
	}
	if env.RequestID == "" && message == "" {
		return nil
	}
	eventID := line.UUID
	if eventID == "" {
		eventID = env.RequestID
	}
	return &models.ToolEvent{
		SourceFile:    path,
		SourceEventID: eventID,
		SessionID:     line.SessionID,
		ProjectRoot:   projectRoot,
		GitBranch:     line.GitBranch,
		Timestamp:     ts,
		Tool:          models.ToolClaudeCode,
		ActionType:    models.ActionAPIError,
		Target:        env.RequestID,
		RawToolName:   errType,
		Success:       false,
		ErrorMessage:  truncateResult(message),
		IsSidechain:   line.IsSidechain,
		MessageID:     env.RequestID,
	}
}

// findInnermostAPIError recursively walks Anthropic's nested error
// envelope and returns the deepest (type, message) pair where the
// message is non-empty. Live Claude Code logs nest the same shape
// `{type, message, error: {…}}` 1–2 levels deep; the leaf carries the
// most-specific information. Returns empty strings when no message
// is present anywhere in the chain.
func findInnermostAPIError(raw json.RawMessage) (errType, message string) {
	if len(raw) == 0 {
		return "", ""
	}
	var node struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return "", ""
	}
	if t, m := findInnermostAPIError(node.Error); m != "" {
		return t, m
	}
	return node.Type, node.Message
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

func truncateReasoning(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func truncateResult(s string) string {
	const max = 2048
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// decodeResultContent extracts the text body from a tool_result content
// payload. Claude Code encodes it as either a bare string or an array of
// {type:"text", text:"..."} blocks — both are handled.
func decodeResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := bytesTrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return ""
		}
		var b strings.Builder
		for i, block := range blocks {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(block.Text)
		}
		return b.String()
	}
	return ""
}

// userPromptText concatenates the text content of a user-role message's
// content blocks. Returns the trimmed result; empty when the message
// carries only tool_result blocks (programmatic responses) or no text.
func userPromptText(blocks []rawContentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		t := strings.TrimSpace(block.Text)
		if t == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(t)
	}
	return strings.TrimSpace(b.String())
}

// appendCapped appends v to xs, keeping at most n elements (oldest dropped).
func appendCapped(xs []string, v string, n int) []string {
	xs = append(xs, v)
	if len(xs) > n {
		xs = xs[len(xs)-n:]
	}
	return xs
}

func lastReasoning(xs []string) string {
	if len(xs) == 0 {
		return ""
	}
	return xs[len(xs)-1]
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
