package grok

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

const (
	fxEncDir = "%2Fhome%2Fdev%2Fdemo"
	fxSID    = "019f0000-0000-7000-8000-000000000001"
)

// fixturePaths returns the absolute grok-home, sessions root, logs root,
// updates.jsonl and unified.jsonl of the committed fixture bundle.
func fixturePaths(t *testing.T) (home, sessions, logs, updates, unified string) {
	t.Helper()
	home, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "grok", "home", ".grok"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	sessions = filepath.Join(home, "sessions")
	logs = filepath.Join(home, "logs")
	updates = filepath.Join(sessions, fxEncDir, fxSID, "updates.jsonl")
	unified = filepath.Join(logs, "unified.jsonl")
	return
}

func newFixtureAdapter(t *testing.T) *Adapter {
	t.Helper()
	_, sessions, logs, _, _ := fixturePaths(t)
	return NewWithOptions(nil, sessions, logs)
}

func TestName(t *testing.T) {
	if got := New().Name(); got != models.ToolGrok {
		t.Fatalf("Name() = %q, want %q", got, models.ToolGrok)
	}
}

func TestIsSessionFile(t *testing.T) {
	// A synthetic-root adapter so the shape+root predicate is exercised
	// independent of the on-disk fixtures.
	a := NewWithOptions(nil, "/home/u/.grok/sessions", "/home/u/.grok/logs")
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"updates under sessions", "/home/u/.grok/sessions/%2Fhome%2Fu%2Fp/uuid/updates.jsonl", true},
		{"unified under logs", "/home/u/.grok/logs/unified.jsonl", true},
		{"windows separators", `C:\Users\u\.grok\sessions\enc\uuid\updates.jsonl`, false}, // not under the linux root
		{"chat_history not claimed", "/home/u/.grok/sessions/enc/uuid/chat_history.jsonl", false},
		{"events not claimed", "/home/u/.grok/sessions/enc/uuid/events.jsonl", false},
		{"foreign updates outside root", "/tmp/foreign/.grok/sessions/enc/uuid/updates.jsonl", false},
		{"unified outside logs root", "/tmp/foreign/.grok/logs/unified.jsonl", false},
		{"unrelated jsonl", "/home/u/.grok/sessions/enc/uuid/rewind_points.jsonl", false},
		{"session_search decoy", "/home/u/.grok/sessions/session_search.sqlite", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.IsSessionFile(tc.path); got != tc.want {
				t.Fatalf("IsSessionFile(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// findEvents returns the ToolEvents of a given action type.
func findEvents(evs []models.ToolEvent, action string) []models.ToolEvent {
	var out []models.ToolEvent
	for _, e := range evs {
		if e.ActionType == action {
			out = append(out, e)
		}
	}
	return out
}

func TestParseUpdates(t *testing.T) {
	a := newFixtureAdapter(t)
	_, _, _, updates, _ := fixturePaths(t)

	res, err := a.ParseSessionFile(context.Background(), updates, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.TokenEvents) != 0 {
		t.Fatalf("updates.jsonl must yield no TokenEvents, got %d", len(res.TokenEvents))
	}

	// Session start marker.
	if starts := findEvents(res.ToolEvents, models.ActionSessionStart); len(starts) != 1 {
		t.Fatalf("want 1 session_start, got %d", len(starts))
	}

	// User prompt: scrubbed, project root + model + branch resolved.
	prompts := findEvents(res.ToolEvents, models.ActionUserPrompt)
	if len(prompts) != 1 {
		t.Fatalf("want 1 user_prompt, got %d", len(prompts))
	}
	p := prompts[0]
	if strings.Contains(p.RawToolInput, "sk-") {
		t.Errorf("user prompt not scrubbed: %q", p.RawToolInput)
	}
	if !strings.Contains(p.RawToolInput, scrub.Redacted) {
		t.Errorf("expected %s in scrubbed prompt, got %q", scrub.Redacted, p.RawToolInput)
	}
	if p.ProjectRoot != "/home/dev/demo" {
		t.Errorf("ProjectRoot = %q, want /home/dev/demo", p.ProjectRoot)
	}
	if p.GitBranch != "main" {
		t.Errorf("GitBranch = %q, want main", p.GitBranch)
	}
	if p.Model != "grok-4.5" {
		t.Errorf("Model = %q, want grok-4.5", p.Model)
	}
	if p.Tool != models.ToolGrok {
		t.Errorf("Tool = %q, want %q", p.Tool, models.ToolGrok)
	}
	if p.SourceEventID != fxSID+"-2" {
		t.Errorf("SourceEventID = %q, want %s-2", p.SourceEventID, fxSID)
	}

	// Assistant message carries the preceding agent_thought as reasoning.
	msgs := findEvents(res.ToolEvents, models.ActionAssistantMessage)
	if len(msgs) != 1 {
		t.Fatalf("want 1 assistant_message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].PrecedingReasoning, "README file first") {
		t.Errorf("assistant PrecedingReasoning missing thought: %q", msgs[0].PrecedingReasoning)
	}

	// read_file tool call succeeded with the target and output.
	reads := findEvents(res.ToolEvents, models.ActionReadFile)
	if len(reads) != 1 {
		t.Fatalf("want 1 read_file, got %d", len(reads))
	}
	if !reads[0].Success {
		t.Errorf("read_file should be Success after completed status")
	}
	if reads[0].Target != "/home/dev/demo/README.md" {
		t.Errorf("read_file Target = %q", reads[0].Target)
	}
	if !strings.Contains(reads[0].ToolOutput, "Hello world") {
		t.Errorf("read_file ToolOutput missing content: %q", reads[0].ToolOutput)
	}
	if reads[0].RawToolName != "read_file" {
		t.Errorf("RawToolName = %q, want read_file", reads[0].RawToolName)
	}

	// run_terminal_command failed.
	runs := findEvents(res.ToolEvents, models.ActionRunCommand)
	if len(runs) != 1 {
		t.Fatalf("want 1 run_command, got %d", len(runs))
	}
	if runs[0].Success {
		t.Errorf("run_command should be failed")
	}
	if !strings.Contains(runs[0].ErrorMessage, "permission denied") {
		t.Errorf("run_command ErrorMessage = %q", runs[0].ErrorMessage)
	}
}

func TestParseUpdatesOffsetResume(t *testing.T) {
	a := newFixtureAdapter(t)
	_, _, _, updates, _ := fixturePaths(t)

	first, err := a.ParseSessionFile(context.Background(), updates, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	if first.NewOffset == 0 {
		t.Fatalf("offset did not advance")
	}
	second, err := a.ParseSessionFile(context.Background(), updates, first.NewOffset)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if len(second.ToolEvents) != 0 {
		t.Errorf("resume from EOF yielded %d events, want 0", len(second.ToolEvents))
	}
	if second.NewOffset != first.NewOffset {
		t.Errorf("NewOffset drifted: %d -> %d", first.NewOffset, second.NewOffset)
	}
	// A fresh parse (offset 0) must NOT emit a second session_start when
	// resuming; but from 0 it does, so the guard is per-call. Re-parse from
	// mid-file must not re-emit session_start.
	mid, err := a.ParseSessionFile(context.Background(), updates, 1)
	if err != nil {
		t.Fatalf("mid parse: %v", err)
	}
	if len(findEvents(mid.ToolEvents, models.ActionSessionStart)) != 0 {
		t.Errorf("session_start emitted on non-zero-offset parse")
	}
}

// TestParseUpdatesArrayContent covers the 2026-07-09 live shapes: a
// tool_call_update whose `content` is an ARRAY of blocks (before the
// shape-tolerant seam this aborted the whole line's unmarshal), the
// `search_replace` edit tool (previously the session's lone `unknown`
// action), and a delete performed via run_terminal_command.
func TestParseUpdatesArrayContent(t *testing.T) {
	const (
		encDir = "%2Fhome%2Fdev%2Fdemo2"
		sid    = "019f0000-0000-7000-8000-000000000002"
	)
	home, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "grok", "home", ".grok"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	sessions := filepath.Join(home, "sessions")
	logs := filepath.Join(home, "logs")
	updates := filepath.Join(sessions, encDir, sid, "updates.jsonl")
	a := NewWithOptions(nil, sessions, logs)

	res, err := a.ParseSessionFile(context.Background(), updates, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// Gap 1: array-shaped content must NOT produce malformed-JSON warnings.
	for _, w := range res.Warnings {
		if strings.Contains(w, "malformed JSON") {
			t.Errorf("array content produced malformed-JSON warning: %q", w)
		}
	}

	// Gap 2: no action normalizes to `unknown`.
	if u := findEvents(res.ToolEvents, models.ActionUnknown); len(u) != 0 {
		names := make([]string, len(u))
		for i, e := range u {
			names[i] = e.RawToolName
		}
		t.Errorf("unexpected unknown actions from tools %v", names)
	}

	// Single-object content on the user message chunk still parses.
	if prompts := findEvents(res.ToolEvents, models.ActionUserPrompt); len(prompts) != 1 {
		t.Fatalf("want 1 user_prompt (single-object content), got %d", len(prompts))
	}

	// write → write_file, completed via array-diff content.
	writes := findEvents(res.ToolEvents, models.ActionWriteFile)
	if len(writes) != 1 {
		t.Fatalf("want 1 write_file, got %d", len(writes))
	}
	if !writes[0].Success {
		t.Errorf("write_file should be Success after completed status")
	}
	if writes[0].Target != "/home/dev/demo2/hello.py" {
		t.Errorf("write_file Target = %q", writes[0].Target)
	}

	// search_replace → edit_file (the previously-unknown action).
	edits := findEvents(res.ToolEvents, models.ActionEditFile)
	if len(edits) != 1 {
		t.Fatalf("want 1 edit_file (search_replace), got %d", len(edits))
	}
	if edits[0].RawToolName != "search_replace" {
		t.Errorf("edit RawToolName = %q, want search_replace", edits[0].RawToolName)
	}
	if !edits[0].Success {
		t.Errorf("edit_file should be Success after completed status")
	}
	if edits[0].Target != "/home/dev/demo2/hello.py" {
		t.Errorf("edit_file Target = %q", edits[0].Target)
	}

	// The delete performed via run_terminal_command → run_command; its
	// output text comes from the array `content` block (not the byte-array
	// rawOutput), proving the content-block output path is preferred.
	runs := findEvents(res.ToolEvents, models.ActionRunCommand)
	if len(runs) != 1 {
		t.Fatalf("want 1 run_command (delete), got %d", len(runs))
	}
	if !runs[0].Success {
		t.Errorf("run_command should be Success")
	}
	if !strings.Contains(runs[0].ToolOutput, "Confirmed: file does not exist") {
		t.Errorf("run_command ToolOutput missing array-content text: %q", runs[0].ToolOutput)
	}
	if strings.Contains(runs[0].ToolOutput, "108") {
		t.Errorf("run_command ToolOutput leaked byte-array rawOutput: %q", runs[0].ToolOutput)
	}

	// list_dir → search_files.
	if lists := findEvents(res.ToolEvents, models.ActionSearchFiles); len(lists) != 1 {
		t.Fatalf("want 1 search_files (list_dir), got %d", len(lists))
	}
}

// TestContentFieldShapes unit-tests the polymorphic content decoder across
// the single-object, array, bare-string, and null shapes.
func TestContentFieldShapes(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantText string
	}{
		{"single text object", `{"type":"text","text":"hi"}`, "hi"},
		{"array with nested content wrapper", `[{"type":"content","content":{"type":"text","text":"out"}}]`, "out"},
		{"array with diff block only", `[{"type":"diff","path":"/x","oldText":"a","newText":"b"}]`, ""},
		{"bare string", `"raw"`, "raw"},
		{"null", `null`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var f acpContentField
			if err := f.UnmarshalJSON([]byte(tc.json)); err != nil {
				t.Fatalf("UnmarshalJSON(%s): %v", tc.json, err)
			}
			if got := f.firstText(); got != tc.wantText {
				t.Fatalf("firstText() = %q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestParseUnified(t *testing.T) {
	a := newFixtureAdapter(t)
	_, _, _, _, unified := fixturePaths(t)

	res, err := a.ParseSessionFile(context.Background(), unified, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile(unified): %v", err)
	}
	if len(res.ToolEvents) != 0 {
		t.Fatalf("unified.jsonl must yield no ToolEvents, got %d", len(res.ToolEvents))
	}
	if len(res.TokenEvents) != 2 {
		t.Fatalf("want 2 TokenEvents (inference_done), got %d", len(res.TokenEvents))
	}

	// loop 1: prompt 12438 gross, cached 11136 → net 1302.
	e0 := res.TokenEvents[0]
	if e0.SessionID != fxSID {
		t.Errorf("SessionID = %q, want %s", e0.SessionID, fxSID)
	}
	if e0.InputTokens != 1302 {
		t.Errorf("net input = %d, want 1302 (12438-11136)", e0.InputTokens)
	}
	if e0.CacheReadTokens != 11136 {
		t.Errorf("cache read = %d, want 11136", e0.CacheReadTokens)
	}
	if e0.OutputTokens != 96 {
		t.Errorf("output = %d, want 96", e0.OutputTokens)
	}
	if e0.ReasoningTokens != 28 {
		t.Errorf("reasoning = %d, want 28", e0.ReasoningTokens)
	}
	if e0.Model != "grok-4.5" {
		t.Errorf("Model = %q, want grok-4.5 (from summary.json glob)", e0.Model)
	}
	if e0.ProjectRoot != "/home/dev/demo" {
		t.Errorf("ProjectRoot = %q, want /home/dev/demo", e0.ProjectRoot)
	}
	if e0.Source != models.TokenSourceJSONL || e0.Reliability != models.ReliabilityApproximate {
		t.Errorf("source/reliability = %q/%q", e0.Source, e0.Reliability)
	}
	if e0.SourceEventID == res.TokenEvents[1].SourceEventID {
		t.Errorf("token SourceEventIDs collide: %q", e0.SourceEventID)
	}

	// loop 2: 26346-25984 = 362.
	if res.TokenEvents[1].InputTokens != 362 {
		t.Errorf("net input[1] = %d, want 362", res.TokenEvents[1].InputTokens)
	}
}

func TestReadTranscript(t *testing.T) {
	a := newFixtureAdapter(t)
	_, _, _, updates, _ := fixturePaths(t)

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: fxSID, Tool: models.ToolGrok}, []string{updates})
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 transcript messages (user + assistant exchange), got %d", len(msgs))
	}
	if msgs[0].Role != models.TranscriptUser {
		t.Errorf("msg0 role = %v, want user", msgs[0].Role)
	}
	if strings.Contains(msgs[0].Text, "system-reminder") {
		t.Errorf("synthetic user record not skipped: %q", msgs[0].Text)
	}
	if msgs[1].Role != models.TranscriptAssistant {
		t.Errorf("msg1 role = %v, want assistant", msgs[1].Role)
	}
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("want 1 tool call on assistant exchange, got %d", len(msgs[1].ToolCalls))
	}
	if msgs[1].Model != "grok-4.5" {
		t.Errorf("assistant Model = %q, want grok-4.5", msgs[1].Model)
	}
	// Reasoning records are dropped at read time.
	for _, m := range msgs {
		if strings.Contains(m.Text, "read the README file first") {
			t.Errorf("reasoning leaked into transcript: %q", m.Text)
		}
	}
}

func TestReadTranscriptFull(t *testing.T) {
	a := newFixtureAdapter(t)
	msgs, err := a.ReadTranscriptFull(context.Background(), models.Session{ID: fxSID, Tool: models.ToolGrok}, nil)
	if err != nil {
		t.Fatalf("ReadTranscriptFull (glob path): %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages via glob, got %d", len(msgs))
	}
}

func TestReadTranscriptMissing(t *testing.T) {
	a := newFixtureAdapter(t)
	_, err := a.ReadTranscript(context.Background(), models.Session{ID: "no-such-session"}, nil)
	if err == nil {
		t.Fatal("expected error for missing session, got nil")
	}
}

func TestMapToolName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"read_file", models.ActionReadFile},
		{"View", models.ActionReadFile},
		{"write", models.ActionWriteFile},
		{"str_replace", models.ActionEditFile},
		{"search_replace", models.ActionEditFile}, // grok's live edit tool (was unknown)
		{"delete_file", models.ActionEditFile},    // file deletion → edit_file (copilot precedent)
		{"run_terminal_command", models.ActionRunCommand},
		{"grep", models.ActionSearchText},
		{"list_dir", models.ActionSearchFiles},
		{"web_search", models.ActionWebSearch},
		{"web_fetch", models.ActionWebFetch},
		{"task", models.ActionSpawnSubagent},
		{"todo_write", models.ActionTodoUpdate},
		{"mcp__server__tool", models.ActionMCPCall},
		{"some_future_tool", models.ActionUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := mapToolName(tc.in); got != tc.want {
				t.Fatalf("mapToolName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTokenBundleNet(t *testing.T) {
	tests := []struct {
		name                                  string
		ctx                                   unifiedCtx
		wantInput, wantCache, wantOut, wantRe int64
	}{
		{"cached subset of gross", unifiedCtx{PromptTokens: 26346, CachedPromptTokens: 25984, CompletionTokens: 154, ReasoningTokens: 53}, 362, 25984, 154, 53},
		{"no cache", unifiedCtx{PromptTokens: 12438, CachedPromptTokens: 0, CompletionTokens: 96, ReasoningTokens: 28}, 12438, 0, 96, 28},
		{"clamp negative", unifiedCtx{PromptTokens: 100, CachedPromptTokens: 200}, 0, 200, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tokenBundle(tc.ctx)
			if got.inputNet != tc.wantInput || got.cacheRead != tc.wantCache ||
				got.output != tc.wantOut || got.reasoning != tc.wantRe {
				t.Fatalf("tokenBundle = %+v, want input=%d cache=%d out=%d re=%d",
					got, tc.wantInput, tc.wantCache, tc.wantOut, tc.wantRe)
			}
		})
	}
}

func TestWatchPaths(t *testing.T) {
	a := New()
	if len(a.WatchPaths()) == 0 {
		t.Fatal("WatchPaths empty")
	}
	var sessions, logs bool
	for _, p := range a.WatchPaths() {
		s := filepath.ToSlash(p)
		if strings.HasSuffix(s, "/.grok/sessions") {
			sessions = true
		}
		if strings.HasSuffix(s, "/.grok/logs") {
			logs = true
		}
	}
	if !sessions || !logs {
		t.Errorf("WatchPaths missing sessions/logs roots: %v", a.WatchPaths())
	}
}
