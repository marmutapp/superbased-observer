package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// parseModern parses a modern Copilot Chat session file (chatSessions/<id>.jsonl
// or globalStorage/emptyWindowChatSessions/<id>.jsonl), produced by VS Code
// Copilot Chat ≥0.45.
//
// Format: each line is one JSON object with a "kind" discriminator.
//
//   - kind=0: a full session snapshot (`v` is the entire session state).
//     A file may contain multiple snapshots — VS Code rewrites the snapshot
//     when reopening a session. We only ever parse the LAST snapshot; earlier
//     ones are subsumed.
//   - kind=1: an incremental patch. `k` is a JSON-pointer-style array of
//     keys/indices into the session; `v` is the new value at that path. We
//     apply only patches whose path begins with "requests" — patches under
//     "inputState" carry transient UI state (selections, attachments, etc.)
//     and may contain multi-MB inline image payloads we never want to ingest.
//
// fromOffset is intentionally ignored: the snapshot+patches model has no
// meaningful resumption point, so we always reparse the whole file. The
// store layer's (source_file, source_event_id) UNIQUE constraint dedupes
// re-emitted turns; the watcher's stat-only short-circuit (v1.4.26) keeps
// steady-state cost bounded.
func (a *Adapter) parseModern(ctx context.Context, path string) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("copilot.parseModern: open %s: %w", path, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("copilot.parseModern: stat: %w", err)
	}

	res := adapter.ParseResult{NewOffset: stat.Size()}

	scanner := bufio.NewScanner(f)
	const maxLine = 16 * 1024 * 1024
	scanner.Buffer(make([]byte, 64*1024), maxLine)

	var session map[string]any
	// pendingPatch buffers a kind=1 (replace) or kind=2 (insert/append)
	// patch until we've finished scanning the file — earlier patches are
	// thrown away if a later kind=0 snapshot subsumes them.
	type pendingPatch struct {
		kind int
		k    []any
		i    int
		hasI bool
		v    json.RawMessage
	}
	var patches []pendingPatch

	lineNum := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		raw := scanner.Bytes()
		lineNum++
		if len(raw) == 0 {
			continue
		}

		var head struct {
			Kind int             `json:"kind"`
			K    json.RawMessage `json:"k"`
			V    json.RawMessage `json:"v"`
			I    *int            `json:"i"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
			continue
		}

		switch head.Kind {
		case 0:
			// New snapshot — discard any patches accumulated against the
			// previous snapshot; they are subsumed by this one.
			var v map[string]any
			if err := json.Unmarshal(head.V, &v); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: snapshot decode: %v", lineNum, err))
				continue
			}
			session = v
			patches = patches[:0]
		case 1, 2:
			// kind=1 replaces the value at path k; kind=2 inserts the
			// elements of v[*] into the array at k, at index i (append
			// when i is omitted). In both cases we filter on k[0] so
			// inputState/selections/attachments patches drop without
			// materializing v (the 14.8 MB attachment line is one of
			// these).
			var k []any
			if err := json.Unmarshal(head.K, &k); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: patch path decode: %v", lineNum, err))
				continue
			}
			if len(k) == 0 {
				continue
			}
			head0, _ := k[0].(string)
			if head0 != "requests" {
				continue
			}
			pp := pendingPatch{kind: head.Kind, k: k, v: head.V}
			if head.I != nil {
				pp.i = *head.I
				pp.hasI = true
			}
			patches = append(patches, pp)
		default:
			// Forward-compat: unknown kind values are ignored.
		}
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("copilot.parseModern: scan: %w", err)
	}

	if session == nil {
		return res, nil
	}

	// Apply queued patches against the last snapshot.
	for _, p := range patches {
		var v any
		if err := json.Unmarshal(p.v, &v); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("patch %v: value decode: %v", p.k, err))
			continue
		}
		switch p.kind {
		case 1:
			if err := applyPatch(session, p.k, v); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("patch %v: %v", p.k, err))
			}
		case 2:
			elems, ok := v.([]any)
			if !ok {
				res.Warnings = append(res.Warnings, fmt.Sprintf("patch %v: kind=2 v is %T, not array", p.k, v))
				continue
			}
			if err := applyInsert(session, p.k, p.i, p.hasI, elems); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("patch %v: %v", p.k, err))
			}
		}
	}

	state := sessionContext{
		SessionID:   sessionIDFromPath(path),
		ProjectRoot: projectRootFromPath(path),
	}
	a.emitModernEvents(path, session, &state, &res)
	return res, nil
}

// applyPatch writes v into session at the location named by the JSON-pointer
// array k. Numeric indices in k may arrive as float64 (json.Unmarshal default)
// or string; both are handled. Returns an error when the path can't be
// followed; callers log and continue.
func applyPatch(session map[string]any, k []any, v any) error {
	if len(k) == 0 {
		return fmt.Errorf("empty path")
	}
	if len(k) == 1 {
		key, ok := k[0].(string)
		if !ok {
			return fmt.Errorf("top-level key must be string, got %T", k[0])
		}
		session[key] = v
		return nil
	}
	cur := any(session)
	for i := 0; i < len(k)-1; i++ {
		next, ok := descend(cur, k[i])
		if !ok {
			return fmt.Errorf("path miss at index %d (%v)", i, k[i])
		}
		cur = next
	}
	return assign(cur, k[len(k)-1], v)
}

// applyInsert handles kind=2 patches: insert elems[*] into the array at
// session[k...] starting at index i. When hasI is false (the JSON omitted
// `i`), the elements are appended.
func applyInsert(session map[string]any, k []any, i int, hasI bool, elems []any) error {
	if len(k) == 0 {
		return fmt.Errorf("empty path")
	}
	// Walk to the parent of the array we want to mutate, then merge at
	// the leaf. We need parent + final key (rather than the array value
	// itself) because slice header reassignment after a grow only sticks
	// when written back through the parent.
	parent := any(session)
	for j := 0; j < len(k)-1; j++ {
		next, ok := descend(parent, k[j])
		if !ok {
			return fmt.Errorf("path miss at index %d (%v)", j, k[j])
		}
		parent = next
	}
	last := k[len(k)-1]
	switch p := parent.(type) {
	case map[string]any:
		s, ok := last.(string)
		if !ok {
			return fmt.Errorf("string key required, got %T", last)
		}
		existing, _ := p[s].([]any)
		p[s] = mergeInsert(existing, i, hasI, elems)
		return nil
	case []any:
		idx, ok := toIndex(last)
		if !ok {
			return fmt.Errorf("integer index required, got %T", last)
		}
		if idx < 0 || idx >= len(p) {
			return fmt.Errorf("index %d out of range %d", idx, len(p))
		}
		existing, _ := p[idx].([]any)
		p[idx] = mergeInsert(existing, i, hasI, elems)
		return nil
	}
	return fmt.Errorf("cannot insert into %T", parent)
}

func mergeInsert(existing []any, i int, hasI bool, elems []any) []any {
	if !hasI || i < 0 || i > len(existing) {
		return append(append([]any{}, existing...), elems...)
	}
	merged := make([]any, 0, len(existing)+len(elems))
	merged = append(merged, existing[:i]...)
	merged = append(merged, elems...)
	merged = append(merged, existing[i:]...)
	return merged
}

func descend(x any, key any) (any, bool) {
	switch m := x.(type) {
	case map[string]any:
		s, ok := key.(string)
		if !ok {
			return nil, false
		}
		val, exists := m[s]
		return val, exists
	case []any:
		idx, ok := toIndex(key)
		if !ok || idx < 0 || idx >= len(m) {
			return nil, false
		}
		return m[idx], true
	}
	return nil, false
}

func assign(x any, key any, v any) error {
	switch m := x.(type) {
	case map[string]any:
		s, ok := key.(string)
		if !ok {
			return fmt.Errorf("string key required, got %T", key)
		}
		m[s] = v
		return nil
	case []any:
		idx, ok := toIndex(key)
		if !ok {
			return fmt.Errorf("integer index required, got %T", key)
		}
		if idx < 0 || idx >= len(m) {
			return fmt.Errorf("index %d out of range %d", idx, len(m))
		}
		m[idx] = v
		return nil
	}
	return fmt.Errorf("cannot assign into %T", x)
}

func toIndex(key any) (int, bool) {
	switch v := key.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case string:
		// rare but tolerated
		var i int
		if _, err := fmt.Sscanf(v, "%d", &i); err == nil {
			return i, true
		}
	}
	return 0, false
}

// emitModernEvents walks the assembled snapshot's requests[] and emits one
// user_prompt + N tool-call ToolEvents + 1 task_complete + 1 TokenEvent per
// turn. Turn idempotence is keyed on requestId, which is stable across
// snapshot rewrites within a file.
func (a *Adapter) emitModernEvents(sourceFile string, session map[string]any, state *sessionContext, res *adapter.ParseResult) {
	if model := topLevelModel(session); model != "" {
		state.Model = model
	}

	requests, _ := session["requests"].([]any)
	for _, ri := range requests {
		req, ok := ri.(map[string]any)
		if !ok {
			continue
		}
		a.emitModernTurn(sourceFile, req, state, res)
	}
}

// topLevelModel returns the session's selectedModel.identifier from
// inputState, when present. Per-turn modelId on a request takes priority,
// but this fallback covers turns that don't echo it.
func topLevelModel(session map[string]any) string {
	is, _ := session["inputState"].(map[string]any)
	if is == nil {
		return ""
	}
	sm, _ := is["selectedModel"].(map[string]any)
	if sm == nil {
		return ""
	}
	id, _ := sm["identifier"].(string)
	return id
}

func (a *Adapter) emitModernTurn(sourceFile string, req map[string]any, state *sessionContext, res *adapter.ParseResult) {
	requestID, _ := req["requestId"].(string)
	if requestID == "" {
		// Without a stable id we can't dedupe; skip rather than emit
		// rows that re-duplicate every reparse.
		res.Warnings = append(res.Warnings, "modern turn missing requestId")
		return
	}
	ts := millisToTime(asInt64(req["timestamp"]))
	// Model resolution: prefer result.metadata.resolvedModel (the actual
	// upstream model that processed the turn — e.g. claude-haiku-4-5-20251001
	// or grok-code-fast-1), fall back to requests[].modelId (the user-facing
	// routing pick — typically copilot/auto when the user hasn't picked a
	// specific model). resolvedModel is what cost analytics actually wants.
	if rm := resolvedModel(req); rm != "" {
		state.Model = rm
	} else if model, _ := req["modelId"].(string); model != "" {
		state.Model = model
	}
	agent, _ := req["agent"].(map[string]any)
	agentID, _ := agent["id"].(string)
	messageID := "assistant:" + requestID

	// User prompt event.
	if msg, _ := req["message"].(map[string]any); msg != nil {
		if text, _ := msg["text"].(string); strings.TrimSpace(text) != "" {
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:         sourceFile,
				SourceEventID:      requestID + ":user",
				SessionID:          state.SessionID,
				MessageID:          "user:" + requestID,
				ProjectRoot:        state.ProjectRoot,
				Timestamp:          ts,
				Model:              state.Model,
				Tool:               models.ToolCopilot,
				ActionType:         models.ActionUserPrompt,
				Target:             truncate(text, 200),
				Success:            true,
				PrecedingReasoning: truncate(text, 200),
				RawToolName:        "user_message",
				RawToolInput:       a.scrubber.String(text),
			})
		}
	}

	// Tool calls — taken from result.metadata.toolCallRounds[].toolCalls[].
	// This is the canonical source; the parallel response[] array has only
	// references to tool invocations, not arguments.
	rounds := pickRounds(req)
	results := pickResults(req)
	for ri, round := range rounds {
		toolCalls, _ := round["toolCalls"].([]any)
		roundTS := millisToTime(asInt64(round["timestamp"]))
		if roundTS.IsZero() {
			roundTS = ts
		}
		for ci, ti := range toolCalls {
			tc, _ := ti.(map[string]any)
			if tc == nil {
				continue
			}
			name, _ := tc["name"].(string)
			args, _ := tc["arguments"].(string)
			callID, _ := tc["id"].(string)
			target, rawInput := parseToolArgs(args, name)
			eventID := fmt.Sprintf("%s:tool:%s", requestID, firstNonEmpty(callID, fmt.Sprintf("r%d-c%d", ri, ci)))
			ev := models.ToolEvent{
				SourceFile:    sourceFile,
				SourceEventID: eventID,
				SessionID:     state.SessionID,
				MessageID:     messageID,
				ProjectRoot:   state.ProjectRoot,
				Timestamp:     roundTS,
				Model:         state.Model,
				Tool:          models.ToolCopilot,
				ActionType:    mapToolName(name),
				Target:        truncate(target, 200),
				Success:       true,
				RawToolName:   name,
				RawToolInput:  a.scrubber.String(rawInput),
			}
			if outExcerpt := toolResultExcerpt(results, callID); outExcerpt != "" {
				ev.ToolOutput = a.scrubber.String(outExcerpt)
			}
			res.ToolEvents = append(res.ToolEvents, ev)
		}
	}

	// Final assistant response (task_complete).
	respText := assistantResponseText(req)
	complete, _ := req["isComplete"].(bool)
	res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
		SourceFile:         sourceFile,
		SourceEventID:      requestID + ":complete",
		SessionID:          state.SessionID,
		MessageID:          messageID,
		ProjectRoot:        state.ProjectRoot,
		Timestamp:          ts,
		Model:              state.Model,
		Tool:               models.ToolCopilot,
		ActionType:         models.ActionTaskComplete,
		Target:             "agent_response",
		Success:            complete,
		DurationMs:         asInt64(req["elapsedMs"]),
		PrecedingReasoning: truncate(agentID, 200),
		RawToolName:        "agent_response",
		ToolOutput:         a.scrubber.String(respText),
	})

	// Token usage. Modern Copilot exposes:
	//   - input:  result.metadata.promptTokens
	//   - output: completionTokens (preferred) / result.metadata.outputTokens
	//   - reasoning: sum of result.metadata.toolCallRounds[].thinking.tokens
	// Cache token counts are NOT exposed (only cacheType:"ephemeral" markers
	// on rendered context blocks, no count). See Adapter doc-comment.
	in, out := turnTokens(req)
	reasoning := turnReasoningTokens(req)
	if in > 0 || out > 0 || reasoning > 0 {
		res.TokenEvents = append(res.TokenEvents, models.TokenEvent{
			SourceFile:      sourceFile,
			SourceEventID:   requestID + ":usage",
			SessionID:       state.SessionID,
			MessageID:       messageID,
			ProjectRoot:     state.ProjectRoot,
			Timestamp:       ts,
			Tool:            models.ToolCopilot,
			Model:           state.Model,
			InputTokens:     in,
			OutputTokens:    out,
			ReasoningTokens: reasoning,
			Source:          models.TokenSourceJSONL,
			Reliability:     models.ReliabilityApproximate,
		})
	}
}

// resolvedModel returns the actual upstream model that processed this turn,
// from result.metadata.resolvedModel. Empty when missing (e.g. the snapshot
// was written before the result patch landed).
func resolvedModel(req map[string]any) string {
	result, _ := req["result"].(map[string]any)
	meta, _ := result["metadata"].(map[string]any)
	rm, _ := meta["resolvedModel"].(string)
	return rm
}

// turnReasoningTokens sums result.metadata.toolCallRounds[*].thinking.tokens
// across all rounds for one turn. Modern Copilot has the field but writes
// 0 in observed data; the field exists for forward-compat with thinking-
// model variants.
func turnReasoningTokens(req map[string]any) int64 {
	result, _ := req["result"].(map[string]any)
	meta, _ := result["metadata"].(map[string]any)
	rounds, _ := meta["toolCallRounds"].([]any)
	var total int64
	for _, r := range rounds {
		rd, _ := r.(map[string]any)
		thinking, _ := rd["thinking"].(map[string]any)
		total += asInt64(thinking["tokens"])
	}
	return total
}

func pickRounds(req map[string]any) []map[string]any {
	result, _ := req["result"].(map[string]any)
	meta, _ := result["metadata"].(map[string]any)
	rawRounds, _ := meta["toolCallRounds"].([]any)
	out := make([]map[string]any, 0, len(rawRounds))
	for _, r := range rawRounds {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func pickResults(req map[string]any) map[string]any {
	result, _ := req["result"].(map[string]any)
	meta, _ := result["metadata"].(map[string]any)
	tcr, _ := meta["toolCallResults"].(map[string]any)
	return tcr
}

// toolResultExcerpt returns a short, scrubbable string describing the tool's
// output. It NEVER returns inline image bytes or large value blobs — only
// structural metadata or a truncated text excerpt.
func toolResultExcerpt(results map[string]any, callID string) string {
	if results == nil || callID == "" {
		return ""
	}
	r, _ := results[callID].(map[string]any)
	if r == nil {
		return ""
	}
	content, _ := r["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	// Prefer text content. Image/binary content is summarized as a marker.
	for _, ci := range content {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if mt, _ := c["mimeType"].(string); strings.HasPrefix(mt, "image/") {
			return "<image:" + mt + ">"
		}
		if v, ok := c["value"].(map[string]any); ok {
			if t, _ := v["text"].(string); t != "" {
				return truncate(t, 400)
			}
		}
		if t, _ := c["text"].(string); t != "" {
			return truncate(t, 400)
		}
	}
	return fmt.Sprintf("<%d content items>", len(content))
}

// assistantResponseText pulls the model's textual reply out of req.response[].
// Modern Copilot interleaves tool-invocation envelopes (kind=toolInvocationSerialized)
// with the assistant's prose; the prose lives in entries with a `value` field.
func assistantResponseText(req map[string]any) string {
	resp, _ := req["response"].([]any)
	var b strings.Builder
	for _, ri := range resp {
		r, _ := ri.(map[string]any)
		if r == nil {
			continue
		}
		// Tool invocation envelope; skip.
		if _, hasKind := r["kind"].(string); hasKind {
			if _, hasToolID := r["toolId"]; hasToolID {
				continue
			}
		}
		if v, ok := r["value"].(string); ok && v != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(v)
		}
	}
	return strings.TrimSpace(b.String())
}

// turnTokens extracts (input, output) tokens from a request. Output tokens
// prefer the top-level `completionTokens` (canonical) over the echo at
// result.metadata.outputTokens; input always comes from result.metadata.promptTokens.
func turnTokens(req map[string]any) (int64, int64) {
	out := asInt64(req["completionTokens"])
	result, _ := req["result"].(map[string]any)
	meta, _ := result["metadata"].(map[string]any)
	in := asInt64(meta["promptTokens"])
	if out == 0 {
		out = asInt64(meta["outputTokens"])
	}
	return in, out
}

func asInt64(x any) int64 {
	switch v := x.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		i, _ := v.Int64()
		return i
	}
	return 0
}

// dispatchParse is the shared entry point that routes between the legacy
// debug-log scanner and the modern snapshot+patches parser.
func (a *Adapter) dispatchParse(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	if isModernSessionPath(path) {
		return a.parseModern(ctx, path)
	}
	return a.parseLegacy(ctx, path, fromOffset)
}

// parseLegacy preserves the existing forward-only debug-log scanner as a
// named entrypoint so the dispatcher can call it explicitly.
func (a *Adapter) parseLegacy(ctx context.Context, path string, fromOffset int64) (adapter.ParseResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return adapter.ParseResult{}, fmt.Errorf("copilot.parseLegacy: open %s: %w", path, err)
	}
	defer f.Close()
	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, io.SeekStart); err != nil {
			return adapter.ParseResult{}, fmt.Errorf("copilot.parseLegacy: seek: %w", err)
		}
	}
	res := adapter.ParseResult{NewOffset: fromOffset}
	state := sessionContext{
		SessionID:   sessionIDFromPath(path),
		ProjectRoot: projectRootFromPath(path),
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
		bytesRead += int64(len(raw) + 1)
		lineNum++
		if len(raw) == 0 {
			continue
		}
		var line rawLine
		if err := json.Unmarshal(raw, &line); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: malformed JSON: %v", lineNum, err))
			res.NewOffset = bytesRead
			continue
		}
		res.NewOffset = bytesRead
		a.parseLine(path, line, lineNum, &state, &res)
	}
	if err := scanner.Err(); err != nil {
		return res, fmt.Errorf("copilot.parseLegacy: scan: %w", err)
	}
	return res, nil
}

// guard against accidental imports being elided.
var _ = time.Time{}
