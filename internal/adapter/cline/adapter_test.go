package cline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// copyFixture duplicates the cline fixture under a synthetic task directory
// so sessionIDFromPath resolves to a task-ID-shaped name.
func copyFixture(t *testing.T, taskID string) string {
	t.Helper()
	src := filepath.Join("..", "..", "..", "testdata", "cline", "api_conversation_history.json")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "tasks", taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "api_conversation_history.json")
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestParseClineTask(t *testing.T) {
	t.Parallel()
	path := copyFixture(t, "abc123")
	a := New()

	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 3 {
		t.Fatalf("tool events: %d want 3", len(res.ToolEvents))
	}

	// event 1: read_file
	e1 := res.ToolEvents[0]
	if e1.ActionType != models.ActionReadFile {
		t.Errorf("e1: %s", e1.ActionType)
	}
	if e1.SessionID != "abc123" {
		t.Errorf("e1 session: %q", e1.SessionID)
	}
	if e1.Tool != models.ToolCline {
		t.Errorf("e1 tool: %q", e1.Tool)
	}
	if !strings.Contains(e1.Target, "auth.go") {
		t.Errorf("e1 target: %q", e1.Target)
	}
	if !strings.Contains(e1.ToolOutput, "package auth") {
		t.Errorf("e1 tool_output: %q", e1.ToolOutput)
	}

	// event 2: replace_in_file → edit_file
	if res.ToolEvents[1].ActionType != models.ActionEditFile {
		t.Errorf("e2: %s", res.ToolEvents[1].ActionType)
	}

	// event 3: execute_command → run_command, failed
	e3 := res.ToolEvents[2]
	if e3.ActionType != models.ActionRunCommand {
		t.Errorf("e3 action: %s", e3.ActionType)
	}
	if e3.Success {
		t.Error("e3 should be failure")
	}
	if !strings.Contains(e3.ErrorMessage, "FAIL") {
		t.Errorf("e3 error_message: %q", e3.ErrorMessage)
	}
	if !strings.Contains(e3.Target, "go test") {
		t.Errorf("e3 target: %q", e3.Target)
	}

	// Token event
	if len(res.TokenEvents) != 1 {
		t.Fatalf("token events: %d want 1", len(res.TokenEvents))
	}
	tk := res.TokenEvents[0]
	if tk.CacheReadTokens != 1500 {
		t.Errorf("cache read: %d", tk.CacheReadTokens)
	}
	if tk.Reliability != models.ReliabilityApproximate {
		t.Errorf("reliability: %q", tk.Reliability)
	}

	// NewOffset should equal file size.
	fi, _ := os.Stat(path)
	if res.NewOffset != fi.Size() {
		t.Errorf("offset: got %d want %d", res.NewOffset, fi.Size())
	}
}

func TestIncrementalSkipsUnchanged(t *testing.T) {
	t.Parallel()
	path := copyFixture(t, "abc123")
	a := New()

	res1, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Second call with offset = prior size should short-circuit.
	res2, err := a.ParseSessionFile(context.Background(), path, res1.NewOffset)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.ToolEvents) != 0 {
		t.Errorf("expected zero events when file unchanged, got %d", len(res2.ToolEvents))
	}
}

func TestToolInferredFromPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want string
	}{
		{"/x/saoudrizwan.claude-dev/tasks/abc/api_conversation_history.json", models.ToolCline},
		{"/x/rooveterinaryinc.roo-cline/tasks/abc/api_conversation_history.json", models.ToolRooCode},
		{"/x/other/tasks/abc/api_conversation_history.json", models.ToolCline},
	}
	for _, tc := range cases {
		if got := toolFromPath(tc.path); got != tc.want {
			t.Errorf("toolFromPath(%q) = %q want %q", tc.path, got, tc.want)
		}
	}
}

func TestIsSessionFile(t *testing.T) {
	t.Parallel()
	a := New()
	if !a.IsSessionFile("/x/tasks/abc/api_conversation_history.json") {
		t.Error("api_conversation_history.json should match")
	}
	if a.IsSessionFile("/x/tasks/abc/ui_messages.json") {
		t.Error("ui_messages.json should NOT match")
	}
}
