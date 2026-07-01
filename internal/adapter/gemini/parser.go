package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// rawLegacy is the legacy single-object JSON shape gemini-cli writes today.
type rawLegacy struct {
	SessionID   string         `json:"sessionId"`
	ProjectHash string         `json:"projectHash"`
	StartTime   string         `json:"startTime"`
	Messages    []rawLegacyMsg `json:"messages"`
}

type rawLegacyMsg struct {
	ID        string         `json:"id"`
	Role      string         `json:"role"`
	Timestamp string         `json:"timestamp"`
	Cwd       string         `json:"cwd"`
	Model     string         `json:"model"`
	Content   contentParts   `json:"content"`
	Tokens    *legacyTokens  `json:"tokens,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// contentParts holds a Gemini message's content, which the live CLI writes
// in TWO shapes depending on role: user/tool messages carry an ARRAY of
// parts ([{"text":"…"}, {"functionCall":…}]), while a `gemini`
// (assistant) message carries a bare STRING ("hi there"). Without a custom
// unmarshaler the string shape fails to decode into []legacyPart and the
// WHOLE assistant line is dropped as malformed — so the dashboard shows only
// the user prompt (the live-capture bug, 2026-06-27). UnmarshalJSON
// normalizes both into a []legacyPart: a string becomes a single text part.
type contentParts []legacyPart

func (c *contentParts) UnmarshalJSON(b []byte) error {
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" || trimmed == "null" {
		*c = nil
		return nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*c = contentParts{{Type: "text", Text: s}}
		return nil
	case '[':
		var parts []legacyPart
		if err := json.Unmarshal(b, &parts); err != nil {
			return err
		}
		*c = contentParts(parts)
		return nil
	default:
		// A single object part (defensive — not observed live).
		var p legacyPart
		if err := json.Unmarshal(b, &p); err != nil {
			return err
		}
		*c = contentParts{p}
		return nil
	}
}

type legacyPart struct {
	Type             string         `json:"type"`
	Text             string         `json:"text"`
	Thought          string         `json:"thought"`
	FunctionCall     *legacyFnCall  `json:"functionCall"`
	FunctionResponse *legacyFnResp  `json:"functionResponse"`
	InlineData       map[string]any `json:"inlineData"`
}

type legacyFnCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type legacyFnResp struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type legacyTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cacheRead"`
	Cached    int64 `json:"cached"`
	// Reasoning ("thinking") tokens. The LIVE Gemini CLI emits "thoughts"
	// (verified 2026-06-26); the proposed event-record shape (issue #15292,
	// used by the testdata fixtures) emits "thoughtsTokenCount". Capture
	// both and take the max in tokenEventFor — with only the
	// thoughtsTokenCount tag, real reasoning tokens deserialized to 0 and
	// were silently dropped (under-billing, since reasoning bills at the
	// output rate; [[feedback_reasoning_tokens_billed]]).
	ThoughtsTokens int64 `json:"thoughtsTokenCount"`
	Thoughts       int64 `json:"thoughts"`
	Total          int64 `json:"total"`
}

// rawJSONL is the proposed event-record shape from issue #15292.
// Both `messages` rows from legacy and `event-record` lines from JSONL
// get normalized to this shape internally before emission.
type rawJSONL struct {
	Type        string         `json:"type"`
	SessionID   string         `json:"sessionId,omitempty"`
	ProjectHash string         `json:"projectHash,omitempty"`
	StartTime   string         `json:"startTime,omitempty"`
	ID          string         `json:"id,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	Model       string         `json:"model,omitempty"`
	Content     contentParts   `json:"content,omitempty"`
	Tokens      *legacyTokens  `json:"tokens,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type sessionState struct {
	SessionID   string
	ProjectHash string
	ProjectRoot string
	Model       string
	StartTime   time.Time
}

// parseLegacy handles a single-object JSON session file. Re-reads the
// file in full on every call (idempotent via dedup); returns the
// current file size as the cursor so the watcher's MAX-monotonic
// guard advances naturally on each turn append.
func (a *Adapter) parseLegacy(ctx context.Context, path string, fi os.FileInfo, fromOffset int64) (adapter.ParseResult, error) {
	res := adapter.ParseResult{NewOffset: fi.Size()}
	if fi.Size() == 0 {
		return res, nil
	}
	if fi.Size() == fromOffset {
		// Watcher woke us with a non-content event (mtime touch, etc.).
		// File size unchanged → no work.
		return res, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("gemini.parseLegacy: read: %w", err)
	}
	var legacy rawLegacy
	if err := json.Unmarshal(body, &legacy); err != nil {
		// Truncated mid-write or genuinely malformed. Don't advance the
		// cursor in this case — the next call will retry once the file
		// is fully written.
		return adapter.ParseResult{
			NewOffset: fromOffset,
			Warnings:  []string{fmt.Sprintf("gemini: legacy JSON parse failed (likely mid-write); will retry: %v", err)},
		}, nil
	}

	state := sessionState{
		SessionID:   firstNonEmpty(legacy.SessionID, sessionIDFromPath(path)),
		ProjectHash: legacy.ProjectHash,
		StartTime:   parseTimestamp(legacy.StartTime),
	}
	// First pass: pick a cwd hint from any message that has one, so
	// downstream emission uses a stable project root.
	for _, m := range legacy.Messages {
		if strings.TrimSpace(m.Cwd) != "" {
			state.ProjectRoot = resolveProjectRoot(path, m.Cwd)
			break
		}
	}
	if state.ProjectRoot == "" {
		state.ProjectRoot = resolveProjectRoot(path, "")
	}

	for i, msg := range legacy.Messages {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		a.emitMessage(path, i, msg, &state, &res)
	}
	return res, nil
}

// parseJSONL handles the proposed event-record JSONL format. Streams
// from fromOffset, returns NewOffset = bytes consumed.
func (a *Adapter) parseJSONL(ctx context.Context, path string, fi os.FileInfo, fromOffset int64) (adapter.ParseResult, error) {
	res := adapter.ParseResult{NewOffset: fromOffset}
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("gemini.parseJSONL: open: %w", err)
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("gemini.parseJSONL: seek: %w", err)
		}
	}

	state := sessionState{SessionID: sessionIDFromPath(path)}
	scanner := bufio.NewScanner(f)
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	bytesRead := fromOffset
	lineNum := 0
	for scanner.Scan() {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		raw := scanner.Bytes()
		bytesRead += int64(len(raw) + 1)
		lineNum++
		if len(raw) == 0 {
			res.NewOffset = bytesRead
			continue
		}
		var line rawJSONL
		if err := json.Unmarshal(raw, &line); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("gemini: line %d malformed: %v", lineNum, err))
			res.NewOffset = bytesRead
			continue
		}
		res.NewOffset = bytesRead

		switch line.Type {
		case "session_metadata":
			if line.SessionID != "" {
				state.SessionID = line.SessionID
			}
			if line.ProjectHash != "" {
				state.ProjectHash = line.ProjectHash
			}
			if line.StartTime != "" {
				state.StartTime = parseTimestamp(line.StartTime)
			}
			if state.ProjectRoot == "" {
				state.ProjectRoot = resolveProjectRoot(path, line.Cwd)
			}
		case "user", "gemini", "model", "tool":
			// Convert event record → legacy-message shape and reuse emitMessage.
			msg := rawLegacyMsg{
				ID:        line.ID,
				Role:      line.Type,
				Timestamp: line.Timestamp,
				Cwd:       line.Cwd,
				Model:     line.Model,
				Content:   line.Content,
				Tokens:    line.Tokens,
				Metadata:  line.Metadata,
			}
			if state.ProjectRoot == "" {
				state.ProjectRoot = resolveProjectRoot(path, msg.Cwd)
			}
			a.emitMessage(path, lineNum, msg, &state, &res)
		case "message_update":
			// Token-row update for a previously emitted gemini message.
			// The store layer's ON CONFLICT DO UPDATE on token_usage will
			// pick up the new counts; we just emit a fresh TokenEvent
			// keyed by the same SourceEventID.
			if line.Tokens == nil || line.ID == "" {
				continue
			}
			ts := parseTimestamp(line.Timestamp)
			if ts.IsZero() {
				ts = state.StartTime
			}
			res.TokenEvents = append(res.TokenEvents, tokenEventFor(path, line.ID, ts, line.Model, &state, *line.Tokens))
		default:
			// The live CLI writes an untyped HEADER line
			// ({sessionId,projectHash,startTime,kind}) plus interleaved
			// {"$set":…} mutation lines. Capture the header as session
			// metadata; skip every other untyped line SILENTLY — a warning
			// per `$set` would flood the watcher log on every session.
			if line.Type == "" {
				// The header carries metadata (projectHash/startTime/cwd) AND a
				// UUID `sessionId`. Capture the metadata but do NOT override
				// state.SessionID: gemini sessions are keyed by the path
				// basename (sessionIDFromPath) everywhere else, and the live
				// header's UUID differs from it — overriding here fragments the
				// session into two rows (user prompt under the path id, the rest
				// under the UUID). The proposed-format `session_metadata` case
				// keeps its own SessionID handling for that (typed) shape.
				if line.SessionID != "" || line.ProjectHash != "" || line.StartTime != "" {
					if line.ProjectHash != "" {
						state.ProjectHash = line.ProjectHash
					}
					if line.StartTime != "" {
						state.StartTime = parseTimestamp(line.StartTime)
					}
					if state.ProjectRoot == "" {
						state.ProjectRoot = resolveProjectRoot(path, line.Cwd)
					}
				}
				continue
			}
			// A genuinely unknown TYPED event — log and skip (forward-compat).
			res.Warnings = append(res.Warnings, fmt.Sprintf("gemini: line %d unknown type %q", lineNum, line.Type))
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("gemini.parseJSONL: scan: %w", err)
	}
	if state.ProjectRoot == "" {
		// Backfill: if no metadata line landed, give every event a root
		// derived from the path. emitMessage already handled per-line
		// resolution but a fully empty file would leave it blank.
		state.ProjectRoot = resolveProjectRoot(path, "")
	}
	return res, nil
}

// emitMessage walks one normalized message and appends ToolEvent /
// TokenEvent records to res. Shared between legacy + JSONL paths.
func (a *Adapter) emitMessage(path string, idx int, msg rawLegacyMsg, state *sessionState, res *adapter.ParseResult) {
	if msg.Cwd != "" && state.ProjectRoot == "" {
		state.ProjectRoot = resolveProjectRoot(path, msg.Cwd)
	}
	if msg.Model != "" {
		state.Model = msg.Model
	}
	ts := parseTimestamp(msg.Timestamp)
	if ts.IsZero() {
		ts = state.StartTime
	}

	role := strings.ToLower(strings.TrimSpace(msg.Role))
	switch role {
	case "user":
		text := concatText(msg.Content)
		// Image-only user turns (a pasted/attached image with no text)
		// would otherwise fall through silently — Gemini carries them as
		// `inlineData` content parts (mimeType + base64 data). Surface a
		// marker row (cowork-style) so the multimodal activity shows in
		// the timeline; the image's token cost lands on the next token
		// event's input bucket. Observability-only — no image bytes are
		// read or stored.
		if strings.TrimSpace(text) == "" {
			if n := countInlineImages(msg.Content); n > 0 {
				text = fmt.Sprintf("[user sent %d image attachment(s)]", n)
			} else {
				return
			}
		}
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile:    path,
			SourceEventID: firstNonEmpty(msg.ID, fmt.Sprintf("user:%s:%d", state.SessionID, idx)),
			SessionID:     state.SessionID,
			ProjectRoot:   state.ProjectRoot,
			Timestamp:     ts,
			Model:         state.Model,
			Tool:          models.ToolGeminiCLI,
			ActionType:    models.ActionUserPrompt,
			Target:        truncate(text, 200),
			Success:       true,
			RawToolName:   "message.user",
			RawToolInput:  a.scrubber.String(text),
			MessageID:     "user:" + firstNonEmpty(msg.ID, fmt.Sprintf("L%d", idx)),
		})
	case "gemini", "model", "assistant":
		reasoning := concatThought(msg.Content)
		assistantID := firstNonEmpty(msg.ID, fmt.Sprintf("assistant:%s:%d", state.SessionID, idx))
		// Emit the model's "thought" (chain-of-thought) as a standalone,
		// visible reasoning row (matches cline/kilocode/opencode/codex), not
		// only as downstream PrecedingReasoning on the tool calls below.
		if reasoning != "" {
			res.ToolEvents = append(res.ToolEvents, a.reasoningEvent(path, msg, idx, ts, state, reasoning, assistantID))
		}
		// Emit a standalone assistant-text row per non-empty text part on
		// this assistant message, mirroring the cross-adapter convention
		// (codex.assistant_text, cline.assistant_text, etc.). These rows
		// carry the body in ToolOutput but no token/cost fields — usage
		// flows through the dedicated TokenEvent emitter below.
		for partIdx, part := range msg.Content {
			if part.Type != "text" {
				continue
			}
			body := strings.TrimSpace(part.Text)
			if body == "" {
				continue
			}
			res.ToolEvents = append(res.ToolEvents, a.assistantTextEvent(path, msg, partIdx, idx, ts, state, body, assistantID))
		}
		// One ToolEvent per functionCall part. function responses on
		// later parts/messages get joined onto the call's ToolOutput
		// via call ID.
		for partIdx, part := range msg.Content {
			if part.FunctionCall == nil {
				continue
			}
			ev := a.toolCallEvent(path, msg, partIdx, ts, state, *part.FunctionCall, reasoning, assistantID)
			res.ToolEvents = append(res.ToolEvents, ev)
		}
		// Capture token row whenever present.
		if msg.Tokens != nil && (msg.Tokens.Input > 0 || msg.Tokens.Output > 0 || msg.Tokens.CacheRead > 0 || msg.Tokens.Cached > 0 || msg.Tokens.ThoughtsTokens > 0) {
			res.TokenEvents = append(res.TokenEvents, tokenEventFor(path, assistantID, ts, state.Model, state, *msg.Tokens))
		}
	case "tool", "function", "function_response":
		// Tool/function response messages: join onto the matching tool
		// call by ID. The store layer doesn't update existing rows from
		// re-emit, so we attach output to events already in res via in-
		// memory lookup. Cross-message functionResponse parts (the
		// canonical legacy shape uses one tool message per response)
		// are handled here.
		for _, part := range msg.Content {
			if part.FunctionResponse == nil {
				continue
			}
			joinResponse(res, part.FunctionResponse, a.scrubber)
		}
	}
}

// assistantTextEvent shapes a text content part on an assistant-role
// message into a standalone observability row. SourceEventID embeds the
// session, message position, and part index for re-parse stability.
// MessageID echoes the assistant message id so this row joins to its
// sibling tool/token events. No token/cost fields.
func (a *Adapter) assistantTextEvent(path string, msg rawLegacyMsg, partIdx, msgIdx int, ts time.Time, state *sessionState, body, assistantID string) models.ToolEvent {
	preview := truncate(a.scrubber.String(body), 200)
	return models.ToolEvent{
		SourceFile:         path,
		SourceEventID:      fmt.Sprintf("asst:%s:%s:%d:%d", state.SessionID, firstNonEmpty(msg.ID, fmt.Sprintf("L%d", msgIdx)), msgIdx, partIdx),
		SessionID:          state.SessionID,
		ProjectRoot:        state.ProjectRoot,
		Timestamp:          ts,
		Model:              state.Model,
		Tool:               models.ToolGeminiCLI,
		ActionType:         models.ActionAssistantMessage,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "gemini.assistant_text",
		ToolOutput:         a.scrubber.String(truncate(body, 4000)),
		MessageID:          assistantID,
	}
}

// reasoningEvent shapes the concatenated "thought" parts on an assistant
// message into a standalone, visible reasoning row. RawToolName
// "gemini.reasoning" mirrors the cross-adapter convention; no token fields.
func (a *Adapter) reasoningEvent(path string, msg rawLegacyMsg, msgIdx int, ts time.Time, state *sessionState, reasoning, assistantID string) models.ToolEvent {
	scrubbed := a.scrubber.String(reasoning)
	preview := truncate(scrubbed, 200)
	return models.ToolEvent{
		SourceFile:         path,
		SourceEventID:      fmt.Sprintf("reasoning:%s:%s:%d", state.SessionID, firstNonEmpty(msg.ID, fmt.Sprintf("L%d", msgIdx)), msgIdx),
		SessionID:          state.SessionID,
		ProjectRoot:        state.ProjectRoot,
		Timestamp:          ts,
		Model:              state.Model,
		Tool:               models.ToolGeminiCLI,
		ActionType:         models.ActionTaskComplete,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "gemini.reasoning",
		ToolOutput:         a.scrubber.String(truncate(reasoning, 4000)),
		MessageID:          assistantID,
	}
}

// toolCallEvent shapes a functionCall content part into a ToolEvent.
func (a *Adapter) toolCallEvent(path string, msg rawLegacyMsg, partIdx int, ts time.Time, state *sessionState, fc legacyFnCall, reasoning, assistantID string) models.ToolEvent {
	rawInput, _ := json.Marshal(fc.Args)
	target := targetFromArgs(fc.Args, fc.Name)
	return models.ToolEvent{
		SourceFile:         path,
		SourceEventID:      firstNonEmpty(fc.ID, fmt.Sprintf("tool:%s:%s:%d:%d", state.SessionID, msg.ID, partIdx, len(rawInput))),
		SessionID:          state.SessionID,
		ProjectRoot:        state.ProjectRoot,
		Timestamp:          ts,
		Model:              state.Model,
		Tool:               models.ToolGeminiCLI,
		ActionType:         mapToolName(fc.Name),
		Target:             truncate(target, 200),
		Success:            true,
		PrecedingReasoning: truncate(reasoning, 200),
		RawToolName:        fc.Name,
		RawToolInput:       a.scrubber.RawJSON(rawInput),
		MessageID:          assistantID,
	}
}

func tokenEventFor(path, msgID string, ts time.Time, modelHint string, state *sessionState, t legacyTokens) models.TokenEvent {
	// Gemini's `input` is the GROSS prompt count — promptTokenCount
	// INCLUDES the cached portion (cachedContentTokenCount ⊆ promptTokenCount).
	// The cost engine's TokenBundle.Input contract is NET non-cached (see
	// internal/intelligence/cost/engine.go): cached tokens bill at the
	// discounted cache-read rate via CacheReadTokens, NOT at the full input
	// rate too. Net at emit time — matching codex/cursor — or the cached
	// portion is double-billed (~3.4× over-billing on cached sessions; see
	// [[feedback_openai_input_is_gross]]).
	cacheRead := maxInt64(t.CacheRead, t.Cached)
	netInput := t.Input - cacheRead
	if netInput < 0 {
		netInput = 0
	}
	reasoning := maxInt64(t.ThoughtsTokens, t.Thoughts)
	return models.TokenEvent{
		SourceFile:      path,
		SourceEventID:   "usage:" + msgID,
		SessionID:       state.SessionID,
		ProjectRoot:     state.ProjectRoot,
		Timestamp:       ts,
		Tool:            models.ToolGeminiCLI,
		Model:           firstNonEmpty(modelHint, state.Model),
		InputTokens:     netInput,
		OutputTokens:    t.Output,
		CacheReadTokens: cacheRead,
		ReasoningTokens: reasoning,
		Source:          models.TokenSourceJSONL,
		Reliability:     models.ReliabilityApproximate,
		MessageID:       msgID,
	}
}

// joinResponse attaches a functionResponse content part to the
// matching ToolEvent in res by call ID. No-op when no match exists
// (ordering anomaly or partial parse).
func joinResponse(res *adapter.ParseResult, fr *legacyFnResp, scrubber *scrub.Scrubber) {
	if fr == nil || fr.ID == "" {
		return
	}
	body, _ := json.Marshal(fr.Response)
	scrubbed := scrubber.RawJSON(body)
	for i := range res.ToolEvents {
		if !strings.HasPrefix(res.ToolEvents[i].RawToolName, "") {
			continue
		}
		if res.ToolEvents[i].SourceEventID == fr.ID {
			res.ToolEvents[i].ToolOutput = truncate(scrubbed, 4096)
			return
		}
	}
}

// concatText joins all `text` parts in a content array.
func concatText(parts []legacyPart) string {
	var out []string
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			if strings.TrimSpace(p.Text) != "" {
				out = append(out, p.Text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// countInlineImages counts content parts carrying Gemini `inlineData`
// (the multimodal image shape: {mimeType, data}). Used to emit an
// image-attachment marker for image-only user turns. A part is treated
// as an image when it has a non-empty inlineData map or its declared
// type names an image/inline-data part.
func countInlineImages(parts []legacyPart) int {
	n := 0
	for _, p := range parts {
		if len(p.InlineData) > 0 {
			n++
			continue
		}
		t := strings.ToLower(strings.TrimSpace(p.Type))
		if t == "image" || t == "inlinedata" || t == "inline_data" {
			n++
		}
	}
	return n
}

// concatThought joins all `thought` parts (Gemini's CoT-style
// reasoning, separate from `text` parts).
func concatThought(parts []legacyPart) string {
	var out []string
	for _, p := range parts {
		if p.Type == "thought" && strings.TrimSpace(p.Thought) != "" {
			out = append(out, p.Thought)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// mapToolName collapses Gemini CLI's tool vocabulary onto the
// normalized actions in models. Both snake_case (modern) and
// camelCase (older builds) variants are accepted; matching is
// case-insensitive with underscores stripped.
func mapToolName(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	switch key {
	case "readfile", "read", "viewfile", "view":
		return models.ActionReadFile
	case "writefile", "write", "createfile", "create":
		return models.ActionWriteFile
	case "replace", "edit", "editfile", "applypatch", "patch":
		return models.ActionEditFile
	case "runshellcommand", "shell", "bash", "exec", "execute", "runcommand",
		"powershell", "pwsh", "cmd", "cmdexe":
		return models.ActionRunCommand
	case "googlewebsearch", "websearch", "search":
		return models.ActionWebSearch
	case "webfetch", "fetch", "fetchurl", "fetchwebpage":
		return models.ActionWebFetch
	case "grep", "searchtext", "findtext":
		return models.ActionSearchText
	case "glob", "findfiles", "filesearch", "ls", "listfiles":
		return models.ActionSearchFiles
	case "savememory", "memorize":
		return models.ActionMCPCall // closest existing semantic; defer dedicated type
	default:
		// MCP tools land here too — names like
		// `mcp__server_name__tool_name` get mapped to ActionMCPCall.
		if strings.HasPrefix(key, "mcp"+"") || strings.Contains(name, "__") {
			return models.ActionMCPCall
		}
		return models.ActionUnknown
	}
}

// targetFromArgs picks a representative target string from a
// functionCall's args map. Tries common path-shaped keys first;
// falls back to the call name.
func targetFromArgs(args map[string]any, fallback string) string {
	for _, key := range []string{"absolute_path", "absolutePath", "path", "file_path", "filePath", "file", "command", "query", "url", "pattern"} {
		if v, ok := args[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return fallback
}

func sessionIDFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base
}

func parseTimestamp(raw string) time.Time {
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
