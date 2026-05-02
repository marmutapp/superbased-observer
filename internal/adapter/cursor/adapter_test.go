package cursor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

func TestBuildEvent_BeforeShellExecution(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "beforeShellExecution",
		"conversation_id": "conv-1",
		"generation_id": "gen-7",
		"workspace_roots": ["/home/me/repo"],
		"model": "claude-sonnet-4-5",
		"command": "go test ./..."
	}`)
	ev, ok, err := BuildEvent(EventBeforeShellCommand, body, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildEvent: %v ok=%v", err, ok)
	}
	if ev.SessionID != "conv-1" {
		t.Errorf("session: %s", ev.SessionID)
	}
	if ev.MessageID != "gen-7" {
		t.Errorf("message id: %s", ev.MessageID)
	}
	if ev.ProjectRoot != "/home/me/repo" {
		t.Errorf("project_root: %s", ev.ProjectRoot)
	}
	if ev.Model != "claude-sonnet-4-5" {
		t.Errorf("model: %q", ev.Model)
	}
	if ev.Tool != models.ToolCursor {
		t.Errorf("tool: %s", ev.Tool)
	}
	if ev.ActionType != models.ActionRunCommand {
		t.Errorf("action: %s", ev.ActionType)
	}
	if ev.Target != "go test ./..." {
		t.Errorf("target: %s", ev.Target)
	}
	if ev.SourceEventID == "" || !strings.HasPrefix(ev.SourceEventID, "gen-7:") {
		t.Errorf("event id: %s", ev.SourceEventID)
	}
}

// TestBuildEvent_PopulatesModelAcrossEvents guards parity with the matching
// stop token event: every Cursor hook payload carries `model`, so action
// rows for the same generation_id should share that model. Before this
// fix BuildEvent decoded raw.Model but never assigned it, leaving the
// actions table with empty model strings while token rows had the real
// value.
func TestBuildEvent_PopulatesModelAcrossEvents(t *testing.T) {
	cases := []struct {
		name string
		evt  string
		body string
	}{
		{
			name: "afterFileEdit",
			evt:  EventAfterFileEdit,
			body: `{"hook_event_name":"afterFileEdit","conversation_id":"c1","generation_id":"g1","workspace_roots":["/repo"],"model":"claude-opus-4-5","file_path":"x.go"}`,
		},
		{
			name: "beforeSubmitPrompt",
			evt:  EventBeforeSubmitPrompt,
			body: `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"c1","generation_id":"g1","workspace_roots":["/repo"],"model":"gpt-5","prompt":"hello"}`,
		},
		{
			name: "beforeMCPExecution",
			evt:  EventBeforeMCPExecution,
			body: `{"hook_event_name":"beforeMCPExecution","conversation_id":"c1","generation_id":"g1","workspace_roots":["/repo"],"model":"gemini-2.5-pro","server_name":"s","tool_name":"t","input":{}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok, err := BuildEvent(tc.evt, []byte(tc.body), scrub.New())
			if err != nil || !ok {
				t.Fatalf("BuildEvent: %v ok=%v", err, ok)
			}
			if ev.Model == "" {
				t.Fatalf("expected ev.Model populated from raw.Model, got empty")
			}
		})
	}
}

func TestBuildEvent_AfterFileEdit(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "afterFileEdit",
		"conversation_id": "c1",
		"generation_id": "g1",
		"workspace_roots": [{"path": "/repo"}],
		"file_path": "internal/handler.go"
	}`)
	ev, ok, err := BuildEvent(EventAfterFileEdit, body, nil)
	if err != nil || !ok {
		t.Fatalf("BuildEvent: %v ok=%v", err, ok)
	}
	if ev.ActionType != models.ActionEditFile {
		t.Errorf("action: %s", ev.ActionType)
	}
	if ev.Target != "internal/handler.go" {
		t.Errorf("target: %s", ev.Target)
	}
	if ev.ProjectRoot != "/repo" {
		t.Errorf("workspace object form: %s", ev.ProjectRoot)
	}
}

func TestBuildEvent_BeforeMCPExecution(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "beforeMCPExecution",
		"conversation_id": "c1",
		"generation_id": "g1",
		"workspace_roots": ["/repo"],
		"server_name": "github",
		"tool_name": "create_issue",
		"input": {"repo": "owner/x", "title": "bug"}
	}`)
	ev, ok, err := BuildEvent(EventBeforeMCPExecution, body, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildEvent: %v ok=%v", err, ok)
	}
	if ev.ActionType != models.ActionMCPCall {
		t.Errorf("action: %s", ev.ActionType)
	}
	if ev.Target != "github:create_issue" {
		t.Errorf("target: %s", ev.Target)
	}
	if !strings.Contains(ev.RawToolInput, "owner/x") {
		t.Errorf("raw input lost: %s", ev.RawToolInput)
	}
}

func TestBuildEvent_BeforeSubmitPrompt(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "beforeSubmitPrompt",
		"conversation_id": "c1",
		"generation_id": "g1",
		"workspace_roots": ["/repo"],
		"prompt": "fix the failing test in handler_test.go and explain why"
	}`)
	ev, ok, err := BuildEvent(EventBeforeSubmitPrompt, body, nil)
	if err != nil || !ok {
		t.Fatalf("BuildEvent: %v ok=%v", err, ok)
	}
	if ev.ActionType != models.ActionUserPrompt {
		t.Errorf("action: %s", ev.ActionType)
	}
	if ev.MessageID != "user:g1" {
		t.Errorf("message id: %s", ev.MessageID)
	}
	if !strings.Contains(ev.Target, "fix the failing test") {
		t.Errorf("target: %s", ev.Target)
	}
}

func TestBuildEvent_StopIsNotRecorded(t *testing.T) {
	body := []byte(`{"hook_event_name":"stop","conversation_id":"c1","workspace_roots":["/repo"]}`)
	_, ok, err := BuildEvent(EventStop, body, nil)
	if err != nil {
		t.Fatalf("BuildEvent: %v", err)
	}
	if ok {
		t.Errorf("stop should not produce an event")
	}
}

func TestBuildStopTokenEvent(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "stop",
		"conversation_id": "c1",
		"generation_id": "g1",
		"workspace_roots": ["/repo"],
		"model": "default",
		"input_tokens": 54833,
		"output_tokens": 773,
		"cache_read_tokens": 41088,
		"cache_write_tokens": 0
	}`)
	ev, ok, err := BuildStopTokenEvent(body)
	if err != nil || !ok {
		t.Fatalf("BuildStopTokenEvent: %v ok=%v", err, ok)
	}
	if ev.SessionID != "c1" || ev.MessageID != "g1" {
		t.Fatalf("session/message mismatch: %+v", ev)
	}
	if ev.Model != "default" {
		t.Fatalf("model = %q", ev.Model)
	}
	if ev.InputTokens != 54833 || ev.OutputTokens != 773 || ev.CacheReadTokens != 41088 {
		t.Fatalf("usage mismatch: %+v", ev)
	}
	if ev.Source != models.TokenSourceHook || ev.Reliability != models.ReliabilityAccurate {
		t.Fatalf("source/reliability mismatch: %+v", ev)
	}
}

func TestBuildStopTranscriptEvents(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	body := strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>Summarize</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"I'll inspect the repo."},{"type":"tool_use","name":"Glob","input":{"target_directory":"d:\\repo","glob_pattern":"*"}},{"type":"tool_use","name":"ReadFile","input":{"path":"d:\\repo\\package.json"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`,
		"",
	}, "\n")
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stopBody := []byte(`{
		"conversation_id":"c1",
		"generation_id":"g1",
		"workspace_roots":["/repo"],
		"transcript_path":"` + strings.ReplaceAll(transcript, `\`, `\\`) + `"
	}`)
	events, err := BuildStopTranscriptEvents(stopBody, scrub.New(), time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("BuildStopTranscriptEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d want 2", len(events))
	}
	if events[0].MessageID != "g1" || events[0].ActionType != models.ActionSearchFiles {
		t.Fatalf("first event = %+v", events[0])
	}
	if events[1].ActionType != models.ActionReadFile || events[1].Target != "d:\\repo\\package.json" {
		t.Fatalf("second event = %+v", events[1])
	}
}

// TestStripUserQueryWrapper pins the wrapper-strip behavior introduced
// in v1.4.21. Cursor's agent runtime wraps user prompts in
// <user_query>...</user_query> XML before passing them to the model;
// previously this landed verbatim in the DB. Strip when both sides
// are present, leave alone when only one side is — partial-wrapper
// stripping risks damaging real content that mentions the tag.
func TestStripUserQueryWrapper(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"complete wrapper":             {"<user_query>\nbuild a todo app\n</user_query>", "build a todo app"},
		"complete wrapper no newlines": {"<user_query>x</user_query>", "x"},
		"surrounding whitespace":       {"  <user_query>x</user_query>  ", "x"},
		"unwrapped":                    {"plain text", "plain text"},
		"only opening tag":             {"<user_query>oops", "<user_query>oops"},
		"only closing tag":             {"oops</user_query>", "oops</user_query>"},
		"empty":                        {"", ""},
		"empty wrapped":                {"<user_query></user_query>", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := stripUserQueryWrapper(c.in)
			if got != c.want {
				t.Errorf("stripUserQueryWrapper(%q) = %q want %q", c.in, got, c.want)
			}
		})
	}
}

// TestBuildTranscriptUserPromptEvent pins the new transcript-walker
// emission of user_prompt actions. Per-line MessageID format must
// match the live hook path ("user:" + generationID) so dashboards can
// join across the two source paths cleanly.
func TestBuildTranscriptUserPromptEvent(t *testing.T) {
	turn := transcriptTurn{
		User: transcriptUserLine{LineNumber: 7, Text: "<user_query>\nWrite tests\n</user_query>"},
	}
	ev, ok := BuildTranscriptUserPromptEvent(turn, "sess-1", "/repo", "gen-X", "/tmp/x.jsonl", time.Unix(0, 0).UTC(), nil)
	if !ok {
		t.Fatal("expected user_prompt event for non-empty wrapped text")
	}
	if ev.ActionType != models.ActionUserPrompt {
		t.Errorf("action_type: %s", ev.ActionType)
	}
	if ev.Target != "Write tests" {
		t.Errorf("target wrapper not stripped: %q", ev.Target)
	}
	if ev.MessageID != "user:gen-X" {
		t.Errorf("message_id: %q want user:gen-X", ev.MessageID)
	}
	if ev.SessionID != "sess-1" {
		t.Errorf("session_id: %q", ev.SessionID)
	}
	if !strings.HasPrefix(ev.SourceEventID, "gen-X:transcript:L7:user:") {
		t.Errorf("source_event_id prefix: %q", ev.SourceEventID)
	}
	if ev.RawToolName != "user_message" {
		t.Errorf("raw_tool_name: %q", ev.RawToolName)
	}
	// Empty user line returns false.
	emptyTurn := transcriptTurn{User: transcriptUserLine{LineNumber: 1, Text: "<user_query></user_query>"}}
	if _, ok := BuildTranscriptUserPromptEvent(emptyTurn, "s", "/r", "g", "/p", time.Time{}, nil); ok {
		t.Error("expected false for empty wrapper")
	}
}

// TestBuildEvent_StripsUserQueryWrapper pins the live-hook path's
// strip behavior for EventBeforeSubmitPrompt — when Cursor's hook
// payload includes a wrapped prompt, the resulting Target is the
// raw user text only.
func TestBuildEvent_StripsUserQueryWrapper(t *testing.T) {
	body := []byte(`{
		"hook_event_name":"beforeSubmitPrompt",
		"conversation_id":"c1",
		"generation_id":"g1",
		"workspace_roots":["/repo"],
		"prompt":"<user_query>\nWrite tests\n</user_query>"
	}`)
	ev, ok, err := BuildEvent(EventBeforeSubmitPrompt, body, nil)
	if err != nil || !ok {
		t.Fatalf("BuildEvent: ok=%v err=%v", ok, err)
	}
	if ev.Target != "Write tests" {
		t.Errorf("target wrapper not stripped in hook path: %q", ev.Target)
	}
}

// TestCursorTranscriptActionType pins the v1.4.21 normalizer extension
// for tool names observed in real cursor agent transcripts that the
// pre-v1.4.21 classifier silently routed to ActionUnknown. Subagent is
// re-tested in capitalized form to confirm the case-insensitive match.
func TestCursorTranscriptActionType(t *testing.T) {
	cases := map[string]string{
		"ReadLints":     models.ActionReadFile,
		"StrReplace":    models.ActionEditFile,
		"Subagent":      models.ActionSpawnSubagent, // capitalized form lower-cases
		"call_mcp_tool": models.ActionMCPCall,
		"Await":         models.ActionUnknown, // intentional — control-flow primitive
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got := cursorTranscriptActionType(name)
			if got != want {
				t.Errorf("cursorTranscriptActionType(%q) = %s want %s", name, got, want)
			}
		})
	}
}

// TestCursorTranscriptTarget pins target extraction for the same set,
// verifying that MCP calls produce server:tool and edit-shaped tools
// pull the file_path/path/target_file field.
func TestCursorTranscriptTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ReadLints", `{"path":"x.go"}`, "x.go"},
		{"StrReplace", `{"file_path":"y.go"}`, "y.go"},
		{"call_mcp_tool", `{"server_name":"obs","tool_name":"get_session_summary"}`, "obs:get_session_summary"},
		{"call_mcp_tool", `{"tool":"only_tool"}`, "only_tool"},
	}
	for _, c := range cases {
		got := cursorTranscriptTarget(c.name, []byte(c.in))
		if got != c.want {
			t.Errorf("cursorTranscriptTarget(%s, %s) = %q want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestBuildEvent_RejectsMissingFields(t *testing.T) {
	// Missing conversation_id.
	_, _, err := BuildEvent(EventBeforeShellCommand, []byte(`{"command":"ls"}`), nil)
	if err == nil {
		t.Error("expected error when conversation_id missing")
	}
	// Malformed JSON.
	_, _, err = BuildEvent(EventBeforeShellCommand, []byte(`{not json`), nil)
	if err == nil {
		t.Error("expected parse error")
	}
	// Missing event name.
	_, _, err = BuildEvent("", []byte(`{"conversation_id":"c1"}`), nil)
	if err == nil {
		t.Error("expected error when event name missing")
	}
}

func TestBuildEvent_LongPromptTruncated(t *testing.T) {
	long := strings.Repeat("x", 500)
	body := []byte(`{
		"hook_event_name":"beforeSubmitPrompt",
		"conversation_id":"c1",
		"generation_id":"g1",
		"workspace_roots":["/repo"],
		"prompt":"` + long + `"
	}`)
	ev, ok, _ := BuildEvent(EventBeforeSubmitPrompt, body, nil)
	if !ok || len(ev.Target) != 200 {
		t.Errorf("expected target truncated to 200 chars, got %d", len(ev.Target))
	}
}

func TestBuildEvent_DeterministicEventID(t *testing.T) {
	body := []byte(`{
		"hook_event_name": "beforeShellExecution",
		"conversation_id": "c1",
		"generation_id": "gen-A",
		"workspace_roots": ["/repo"],
		"command": "go build"
	}`)
	a, _, _ := BuildEvent(EventBeforeShellCommand, body, nil)
	b, _, _ := BuildEvent(EventBeforeShellCommand, body, nil)
	if a.SourceEventID != b.SourceEventID {
		t.Errorf("event IDs differ across calls: %s vs %s", a.SourceEventID, b.SourceEventID)
	}
}
