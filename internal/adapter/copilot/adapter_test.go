package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func writeFixture(t *testing.T, lines []string) string {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "workspaceStorage", "ws-1")
	dir := filepath.Join(ws, "GitHub.copilot-chat", "debug-logs", "sess-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "workspace.json"), []byte("{\n  \"folder\": \"file:///d%3A/programsx/test-project\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "main.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAdapter_Name(t *testing.T) {
	if New().Name() != models.ToolCopilot {
		t.Fatalf("name: %s", New().Name())
	}
}

func TestAdapter_IsSessionFile(t *testing.T) {
	a := New()
	cases := map[string]bool{
		// legacy debug-log path (Windows + macOS)
		`C:\Users\x\AppData\Roaming\Code\User\workspaceStorage\a\GitHub.copilot-chat\debug-logs\sess\main.jsonl`:           true,
		`/Users/x/Library/Application Support/Code/User/workspaceStorage/a/GitHub.copilot-chat/debug-logs/sess/main.jsonl`: true,
		// modern workspace-bound chatSessions
		`C:\Users\x\AppData\Roaming\Code\User\workspaceStorage\a\chatSessions\sess.jsonl`: true,
		`/home/u/.config/Code/User/workspaceStorage/a/chatSessions/sess.jsonl`:            true,
		// modern empty-window global chat sessions
		`C:\Users\x\AppData\Roaming\Code\User\globalStorage\emptyWindowChatSessions\sess.jsonl`:           true,
		`/Users/x/Library/Application Support/Code/User/globalStorage/emptyWindowChatSessions/sess.jsonl`: true,
		// negatives
		`/tmp/GitHub.copilot-chat/debug-logs/sess/tools_0.json`: false,
		`/tmp/chatSessions/sess.json`:                           false,
		`/tmp/some/path/main.jsonl`:                             false,
	}
	for path, want := range cases {
		if got := a.IsSessionFile(path); got != want {
			t.Errorf("IsSessionFile(%q) = %v want %v", path, got, want)
		}
	}
}

func TestSessionIDFromPath_Modern(t *testing.T) {
	// All paths use the host's native separator because the watcher feeds
	// real on-disk paths into ParseSessionFile (and thus sessionIDFromPath).
	cases := map[string]string{
		filepath.Join("a", "chatSessions", "abc123.jsonl"):                                                   "abc123",
		filepath.Join("globalStorage", "emptyWindowChatSessions", "empty-1.jsonl"):                           "empty-1",
		filepath.Join("workspaceStorage", "ws", "GitHub.copilot-chat", "debug-logs", "sess-1", "main.jsonl"): "sess-1",
	}
	for path, want := range cases {
		if got := sessionIDFromPath(path); got != want {
			t.Errorf("sessionIDFromPath(%q) = %q want %q", path, got, want)
		}
	}
}

func TestParseSessionFile_DebugLogMainJSONL(t *testing.T) {
	lines := []string{
		`{"v":1,"ts":1776928112439,"dur":0,"sid":"sess-1","type":"session_start","name":"session_start","spanId":"session-start","status":"ok","attrs":{"copilotVersion":"0.45.0"}}`,
		`{"ts":1776928112440,"dur":0,"sid":"sess-1","type":"user_message","name":"user_message","spanId":"user-1","status":"ok","attrs":{"content":"hello4"}}`,
		`{"ts":1776928112559,"dur":3,"sid":"sess-1","type":"tool_call","name":"manage_todo_list","spanId":"tool-1","parentSpanId":"user-1","status":"ok","attrs":{"args":"{\"operation\":\"read\",\"chatSessionResource\":{\"scheme\":\"vscode-chat-session\"}}","result":"No todo list found."}}`,
		`{"ts":1776928112610,"dur":42356,"sid":"sess-1","type":"llm_request","name":"chat:oswe-vscode-prime","spanId":"llm-1","parentSpanId":"user-1","status":"ok","attrs":{"model":"oswe-vscode-prime","inputTokens":11136,"outputTokens":56,"ttft":31875}}`,
		`{"ts":1776928154966,"dur":0,"sid":"sess-1","type":"agent_response","name":"agent_response","spanId":"agent-1","parentSpanId":"user-1","status":"ok","attrs":{"response":"[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"Hello! How can I help with your project?\"}]}]","reasoning":"Responding to greetings"}}`,
	}
	path := writeFixture(t, lines)

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 3 {
		t.Fatalf("ToolEvents: got %d want 3", len(res.ToolEvents))
	}
	if len(res.TokenEvents) != 1 {
		t.Fatalf("TokenEvents: got %d want 1", len(res.TokenEvents))
	}

	if res.ToolEvents[0].ActionType != models.ActionUserPrompt || res.ToolEvents[0].Target != "hello4" {
		t.Fatalf("user prompt event mismatch: %#v", res.ToolEvents[0])
	}
	// `decodeFileURI` returns the platform's native separator via
	// `filepath.FromSlash`, so on Linux/macOS this is `d:/programsx/...`
	// and on Windows it's `d:\programsx\...`. Use FromSlash here too so
	// the assertion passes on every host.
	if want := filepath.FromSlash("d:/programsx/test-project"); res.ToolEvents[0].ProjectRoot != want {
		t.Fatalf("project root mismatch: %#v want %q", res.ToolEvents[0], want)
	}
	if res.ToolEvents[0].MessageID != "user:user-1" {
		t.Fatalf("user message_id mismatch: %#v", res.ToolEvents[0])
	}
	if res.ToolEvents[1].ActionType != models.ActionTodoUpdate {
		t.Fatalf("tool event action mismatch: %#v", res.ToolEvents[1])
	}
	if res.ToolEvents[1].MessageID != "assistant:user-1" {
		t.Fatalf("tool message_id mismatch: %#v", res.ToolEvents[1])
	}
	if res.ToolEvents[1].ToolOutput != "No todo list found." {
		t.Fatalf("tool event output mismatch: %#v", res.ToolEvents[1])
	}
	if res.ToolEvents[2].ActionType != models.ActionTaskComplete {
		t.Fatalf("task complete mismatch: %#v", res.ToolEvents[2])
	}
	if res.ToolEvents[2].MessageID != "assistant:user-1" {
		t.Fatalf("task complete message_id mismatch: %#v", res.ToolEvents[2])
	}
	if !strings.Contains(res.ToolEvents[2].ToolOutput, "How can I help") {
		t.Fatalf("task complete output mismatch: %#v", res.ToolEvents[2])
	}

	if res.TokenEvents[0].Model != "oswe-vscode-prime" {
		t.Fatalf("token model mismatch: %#v", res.TokenEvents[0])
	}
	if res.TokenEvents[0].MessageID != "assistant:user-1" {
		t.Fatalf("token message_id mismatch: %#v", res.TokenEvents[0])
	}
	if res.TokenEvents[0].InputTokens != 11136 || res.TokenEvents[0].OutputTokens != 56 {
		t.Fatalf("token counts mismatch: %#v", res.TokenEvents[0])
	}

	stat, _ := os.Stat(path)
	if res.NewOffset != stat.Size() {
		t.Fatalf("NewOffset: got %d want %d", res.NewOffset, stat.Size())
	}
}

func TestDecodeFileURI(t *testing.T) {
	// decodeFileURI normalizes to the host's native separator; the
	// assertion has to do the same so this test passes on every CI
	// runner, not only Windows.
	want := filepath.FromSlash("d:/programsx/test-project")
	if got := decodeFileURI("file:///d%3A/programsx/test-project"); got != want {
		t.Fatalf("decodeFileURI = %q want %q", got, want)
	}
}

func TestParseSessionFile_MalformedLineSkipped(t *testing.T) {
	path := writeFixture(t, []string{
		`{"ts":1,"sid":"sess-1","type":"user_message","spanId":"u1","attrs":{"content":"hello"}}`,
		`{not json}`,
		`{"ts":2,"sid":"sess-1","type":"agent_response","spanId":"a1","attrs":{"response":"[{\"role\":\"assistant\",\"parts\":[{\"type\":\"text\",\"content\":\"done\"}]}]"}}`,
	})

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 2 {
		t.Fatalf("ToolEvents: got %d want 2", len(res.ToolEvents))
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("Warnings: got %d want 1", len(res.Warnings))
	}
}
