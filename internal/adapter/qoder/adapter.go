package qoder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/git"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Adapter parses qoder CLI sessions. See the package doc for the record
// shapes, the token/model gaps, and the project-root resolution model.
type Adapter struct {
	scrubber *scrub.Scrubber
	roots    []string
}

// New returns an adapter with the default scrubber and platform-default
// cross-mount watch roots (~/.qoder/projects and ~/.qoder/logs/sessions
// under every resolved $HOME).
func New() *Adapter {
	return &Adapter{scrubber: scrub.New(), roots: defaultRoots()}
}

// NewWithOptions customizes the scrubber and/or watch roots for tests. A
// nil scrubber falls back to scrub.New(); no roots falls back to the
// platform defaults.
func NewWithOptions(s *scrub.Scrubber, roots ...string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	return &Adapter{scrubber: s, roots: roots}
}

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return models.ToolQoder }

// WatchPaths implements adapter.Adapter. Two roots per resolved $HOME: the
// transcript store (projects/) and the run-log store (logs/sessions/). The
// layout is identical across Linux / macOS / Windows — qoder mirrors
// $HOME/.qoder everywhere — so a WSL2 observer picks up Windows-side
// sessions on /mnt/c/Users/<u>/.qoder and vice versa.
func (a *Adapter) WatchPaths() []string { return a.roots }

// defaultRoots returns the transcript + run-log roots under every
// cross-mount-resolved $HOME.
func defaultRoots() []string {
	var roots []string
	for _, h := range crossmount.AllHomes() {
		roots = append(
			roots,
			filepath.Join(h.Path, ".qoder", "projects"),
			filepath.Join(h.Path, ".qoder", "logs", "sessions"),
		)
	}
	return roots
}

// IsSessionFile implements adapter.Adapter. A path qualifies only when it
// is BOTH under one of this adapter's watch roots AND matches one of the
// two qoder JSONL shapes: the transcript
// `.qoder/projects/<slug>/<uuid>.jsonl` or the run-log segment
// `.qoder/logs/sessions/<slug>/<sid>/segments/*.jsonl`. The encrypted
// `<uuid>/state.json` and `compression-v2/state.json` siblings are
// rejected (not .jsonl).
func (a *Adapter) IsSessionFile(path string) bool {
	if !matchesShape(path) {
		return false
	}
	return adapter.UnderAnyWatchRoot(path, a.WatchPaths())
}

// matchesShape reports whether path has a qoder transcript or segment
// shape, independent of watch roots. Comparison is on a slash-normalized,
// lower-cased copy so Windows separators and case-insensitive mounts
// match too.
func matchesShape(path string) bool {
	lower := strings.ReplaceAll(strings.ToLower(path), `\`, "/")
	if !strings.HasSuffix(lower, ".jsonl") {
		return false
	}
	if isSegmentPath(lower) {
		return true
	}
	// Transcript: under .qoder/projects/, and the <uuid>.jsonl sits
	// directly in the slug dir (NOT under the <uuid>/ state subdir).
	if !strings.Contains(lower, "/.qoder/projects/") {
		return false
	}
	return true
}

// isSegmentPath reports whether a slash-normalized lower-cased path is a
// run-log segment file.
func isSegmentPath(lower string) bool {
	return strings.Contains(lower, "/.qoder/logs/sessions/") &&
		strings.Contains(lower, "/segments/")
}

// ParseSessionFile implements adapter.Adapter. It streams the JSONL from
// fromOffset to EOF. A transcript file emits ToolEvents (user prompts,
// assistant text, tool calls + their results); a run-log segment file
// emits TokenEvents from model.response.completed records (skipped while
// all-zero, which is the only shape seen in live capture). Malformed lines
// are skipped with a warning; the byte cursor advances past every fully
// terminated line so repeated calls make progress.
func (a *Adapter) ParseSessionFile(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("qoder.ParseSessionFile: open %s: %w", path, err)
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("qoder.ParseSessionFile: seek: %w", err)
		}
	}

	segment := isSegmentPath(strings.ReplaceAll(strings.ToLower(path), `\`, "/"))
	res := adapter.ParseResult{NewOffset: fromOffset}
	st := &parseState{
		adapter:     a,
		path:        path,
		rootCache:   map[string]string{},
		pendingCall: map[string]int{},
		firstOffset: fromOffset,
	}
	if segment {
		st.sessionID = sessionIDFromSegmentPath(path)
	}

	// bufio.Reader.ReadString (not Scanner) so the byte cursor advances by
	// the exact terminator length including CRLF, and long tool-output
	// lines aren't capped by a token-size limit.
	reader := bufio.NewReaderSize(f, 64*1024)
	bytesRead := fromOffset
	lineNum := 0
	for {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		lineStr, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return res, fmt.Errorf("qoder.ParseSessionFile: read: %w", readErr)
		}
		hasNewline := strings.HasSuffix(lineStr, "\n")
		// A partial trailing line (no '\n' at EOF) is a still-being-written
		// record: defer it, do NOT advance the cursor past it.
		if !hasNewline && readErr == io.EOF {
			break
		}
		bytesRead += int64(len(lineStr))
		lineNum++
		// Commit the offset for every terminated line, even empty/malformed
		// ones, so the poll loop can't spin on a bad line.
		res.NewOffset = bytesRead

		raw := strings.TrimRight(lineStr, "\r\n")
		if raw == "" {
			if readErr == io.EOF {
				break
			}
			continue
		}
		if segment {
			st.handleSegment([]byte(raw), lineNum, &res)
		} else {
			st.handleTranscript([]byte(raw), lineNum, &res)
		}
		if readErr == io.EOF {
			break
		}
	}
	st.flagPendingOutcomes(&res)
	return res, nil
}

// flagPendingOutcomes marks every tool call still waiting for its result
// at EOF. Its Success=true is a placeholder, so the store holds
// failure-context bookkeeping for the row until the matching
// ActionOutcomeUpdate reports the real outcome.
func (st *parseState) flagPendingOutcomes(res *adapter.ParseResult) {
	for _, idx := range st.pendingCall {
		if idx < len(res.ToolEvents) {
			res.ToolEvents[idx].OutcomePending = true
		}
	}
}

// parseState carries the per-call mutable bookkeeping shared across lines.
type parseState struct {
	adapter *Adapter
	path    string
	// rootCache memoizes cwd → resolved project root.
	rootCache map[string]string
	// pendingCall maps a tool_use id to the index of its ToolEvent, so the
	// later tool_result block can stamp success/output onto it.
	pendingCall map[string]int
	// firstOffset is the fromOffset the parse started at; the session-start
	// marker is only emitted when parsing from the very top of the file.
	firstOffset int64
	// sessionStarted guards against emitting more than one session-start
	// marker per parse call.
	sessionStarted bool
	// lastCwd / lastBranch carry the most recent envelope context.
	lastCwd    string
	lastBranch string
	// sessionID + projectRoot are recovered for segment run-logs (whose
	// records carry neither directly on the token line).
	sessionID   string
	segProjRoot string
}

// handleTranscript decodes and dispatches one transcript JSONL line.
func (st *parseState) handleTranscript(raw []byte, lineNum int, res *adapter.ParseResult) {
	var rec rawRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
		return
	}
	if rec.Cwd != "" {
		st.lastCwd = rec.Cwd
	}
	if rec.GitBranch != "" {
		st.lastBranch = rec.GitBranch
	}
	switch rec.Type {
	case "user":
		st.emitUser(&rec, res)
	case "assistant":
		st.emitAssistant(&rec, res)
	}
	// runtime-config / file-history-snapshot / last-prompt are
	// informational and intentionally skipped.
}

// projectRoot resolves a cwd (translating a foreign-OS path before
// git.Resolve) and memoizes the result. An empty cwd yields "".
func (st *parseState) projectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	cwd = crossmount.TranslateForeignPath(cwd)
	if root, ok := st.rootCache[cwd]; ok {
		return root
	}
	info, err := git.Resolve(cwd)
	if err != nil {
		st.rootCache[cwd] = cwd
		return cwd
	}
	st.rootCache[cwd] = info.Root
	return info.Root
}

// emitUser records a user prompt (message.content is a bare string) plus a
// one-shot session-start marker, or applies tool results (content is an
// array of tool_result blocks).
func (st *parseState) emitUser(rec *rawRecord, res *adapter.ParseResult) {
	root := st.projectRoot(st.lastCwd)
	if text := rec.Message.contentString(); text != "" {
		if st.firstOffset == 0 && !st.sessionStarted && (rec.ParentUUID == nil || *rec.ParentUUID == "") {
			st.sessionStarted = true
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    st.path,
				SourceEventID: "session_start:" + rec.SessionID,
				SessionID:     rec.SessionID,
				ProjectRoot:   root,
				Timestamp:     parseTimestamp(rec.Timestamp),
				GitBranch:     st.lastBranch,
				Tool:          models.ToolQoder,
				ActionType:    models.ActionSessionStart,
				Target:        "startup",
				RawToolName:   "qoder.session_start",
				IsSidechain:   rec.IsSidechain,
				Success:       true,
			})
		}
		scrubbed := st.adapter.scrubber.String(text)
		res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
			SourceFile:    st.path,
			SourceEventID: "prompt:" + rec.UUID,
			SessionID:     rec.SessionID,
			ProjectRoot:   root,
			Timestamp:     parseTimestamp(rec.Timestamp),
			GitBranch:     st.lastBranch,
			Tool:          models.ToolQoder,
			ActionType:    models.ActionUserPrompt,
			Target:        truncate(scrubbed, 200),
			RawToolName:   "qoder.user_prompt",
			RawToolInput:  scrubbed,
			IsSidechain:   rec.IsSidechain,
			Success:       true,
		})
		return
	}
	for _, block := range rec.Message.contentBlocks() {
		if block.Type != "tool_result" {
			continue
		}
		st.applyToolResult(block, res)
	}
}

// emitAssistant records the assistant turn's text blocks as
// assistant-message events and its tool_use blocks as tool-call events.
func (st *parseState) emitAssistant(rec *rawRecord, res *adapter.ParseResult) {
	if rec.Message == nil {
		return
	}
	root := st.projectRoot(st.lastCwd)
	ts := parseTimestamp(rec.Timestamp)
	msgID := rec.Message.ID
	model := rec.Message.Model // empty in live capture; never fabricated
	for _, block := range rec.Message.contentBlocks() {
		switch block.Type {
		case "tool_use":
			action := mapToolName(block.Name)
			scrubbedInput := st.adapter.scrubber.RawJSON(block.Input)
			target := st.adapter.scrubber.String(targetFromInput(block.Input, block.Name))
			idx := len(res.ToolEvents)
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    st.path,
				SourceEventID: "tool:" + toolEventID(block.ID, rec.UUID, idx),
				SessionID:     rec.SessionID,
				ProjectRoot:   root,
				Timestamp:     ts,
				GitBranch:     st.lastBranch,
				Model:         model,
				Tool:          models.ToolQoder,
				ActionType:    action,
				Target:        target,
				RawToolName:   block.Name,
				RawToolInput:  scrubbedInput,
				ContentBytes:  authoredBytes(action, block.Input),
				MessageID:     msgID,
				IsSidechain:   rec.IsSidechain,
				Success:       true, // optimistic; corrected by the paired tool_result
			})
			if block.ID != "" {
				st.pendingCall[block.ID] = idx
			}
		case "text":
			if strings.TrimSpace(block.Text) == "" {
				continue
			}
			scrubbed := st.adapter.scrubber.String(block.Text)
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    st.path,
				SourceEventID: "assistant:" + rec.UUID,
				SessionID:     rec.SessionID,
				ProjectRoot:   root,
				Timestamp:     ts,
				GitBranch:     st.lastBranch,
				Model:         model,
				Tool:          models.ToolQoder,
				ActionType:    models.ActionAssistantMessage,
				Target:        truncate(scrubbed, 200),
				RawToolName:   "qoder.assistant_text",
				ToolOutput:    st.adapter.scrubber.String(contentcap.Cap(block.Text, contentcap.DefaultMaxBytes)),
				MessageID:     msgID,
				IsSidechain:   rec.IsSidechain,
				Success:       true,
			})
		}
	}
}

// applyToolResult stamps success + scrubbed output onto the ToolEvent the
// matching tool_use produced.
//
// When no pending call matches, the tool_use was emitted in an EARLIER
// parse window and its row is already persisted (optimistically
// successful): the outcome goes out as an ActionOutcomeUpdate keyed by
// the same "tool:<ToolUseID>" SourceEventID the emit side built. Only a
// non-empty ToolUseID qualifies — the empty-id fallback id embeds the
// record uuid + position, which the result block doesn't carry.
func (st *parseState) applyToolResult(block rawBlock, res *adapter.ParseResult) {
	var scrubbed string
	if out := toolResultText(block.Content); out != "" {
		scrubbed = st.adapter.scrubber.String(contentcap.Cap(out, contentcap.DefaultMaxBytes))
	}
	idx, ok := st.pendingCall[block.ToolUseID]
	if !ok || idx >= len(res.ToolEvents) {
		if block.ToolUseID == "" {
			return
		}
		// DurationMs stays 0 — the call lived in the prior window and
		// UpdateActionOutcome no-ops a zero.
		up := models.ActionOutcomeUpdate{
			SourceFile:    st.path,
			SourceEventID: "tool:" + block.ToolUseID,
			SuccessKnown:  true, // is_error is always a verdict
			Success:       !block.IsError,
			ToolOutput:    scrubbed,
		}
		if block.IsError && scrubbed != "" {
			up.ErrorMessage = truncate(scrubbed, 500)
		}
		res.OutcomeUpdates = append(res.OutcomeUpdates, up)
		return
	}
	ev := &res.ToolEvents[idx]
	ev.Success = !block.IsError
	if scrubbed != "" {
		ev.ToolOutput = scrubbed
		if block.IsError {
			ev.ErrorMessage = truncate(scrubbed, 500)
		}
	}
	delete(st.pendingCall, block.ToolUseID)
}

// toolEventID builds a deterministic per-call id, preferring the provider
// tool_use id and falling back to the record uuid + position.
func toolEventID(callID, uuid string, pos int) string {
	if callID != "" {
		return callID
	}
	return uuid + ":" + strconv.Itoa(pos)
}

// handleSegment decodes and dispatches one run-log segment JSONL line. Only
// session.config.loaded (project root) and model.response.completed
// (tokens) are consumed; every other event type is skipped silently.
func (st *parseState) handleSegment(raw []byte, lineNum int, res *adapter.ParseResult) {
	var seg rawSegment
	if err := json.Unmarshal(raw, &seg); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
		return
	}
	switch seg.Type {
	case "session.config.loaded":
		var cfg segConfig
		if err := json.Unmarshal(seg.Data, &cfg); err != nil {
			return
		}
		cwd := cfg.ProjectRoot
		if cwd == "" {
			cwd = cfg.TargetDir
		}
		st.segProjRoot = st.projectRoot(cwd)
	case "model.response.completed":
		var r segResponse
		if err := json.Unmarshal(seg.Data, &r); err != nil {
			return
		}
		// Zero-usage guard: qoder resolves usage server-side and writes
		// zeros locally. Emit only when a (future) capture carries real
		// counts, so no phantom rows land today.
		if !r.nonZero() {
			return
		}
		res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
			SourceFile:          st.path,
			SourceEventID:       segTokenID(seg, lineNum),
			SessionID:           st.sessionID,
			ProjectRoot:         st.segProjRoot,
			Timestamp:           parseTimestamp(seg.Ts),
			Tool:                models.ToolQoder,
			Model:               r.Model,
			InputTokens:         r.InputTokens,
			OutputTokens:        r.OutputTokens,
			CacheReadTokens:     r.CacheReadTokens,
			CacheCreationTokens: r.CacheCreationTokens,
			Source:              models.TokenSourceJSONL,
			Reliability:         models.ReliabilityApproximate,
			MessageID:           seg.RequestID,
			TurnID:              seg.TurnID,
		})
	}
}

// segTokenID builds a deterministic token-event id from the segment's
// request id, falling back to session + sequence.
func segTokenID(seg rawSegment, lineNum int) string {
	if seg.RequestID != "" {
		return "tok:" + seg.RequestID
	}
	return "tok:" + seg.TurnID + ":" + strconv.Itoa(lineNum)
}

// sessionIDFromSegmentPath recovers the session uuid from a run-log segment
// path `.../logs/sessions/<slug>/<sid>/segments/<file>.jsonl` — the <sid>
// dir is the parent of the `segments` directory.
func sessionIDFromSegmentPath(path string) string {
	dir := filepath.Dir(path) // …/segments
	if strings.EqualFold(filepath.Base(dir), "segments") {
		return filepath.Base(filepath.Dir(dir)) // …/<sid>
	}
	return ""
}
