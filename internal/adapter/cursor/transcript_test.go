package cursor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// ccWriteCursorTranscript lays out the live-grounded agent-transcript
// shape: `<root>/projects/<slug>/agent-transcripts/<conv>/<conv>.jsonl`.
func writeCursorTranscript(t *testing.T, root, convID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(root, "projects", "home-user-proj", "agent-transcripts", convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, convID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var cursorFixtureLines = []string{
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nBuild the feature\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"text","text":"On it."},{"type":"tool_use","name":"run_terminal_cmd","input":{"command":"go test"}}]}}`,
	`{"type":"turn_ended","status":"success"}`,
	`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nNow part two\n</user_query>"}]}}`,
	`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"edit_file","input":{"path":"a.go"}}]}}`,
}

// TestCursorReadTranscript pins the live-grounded normalization: the
// user_query envelope is stripped, id-less tool calls settle on the
// turn_ended marker (empty excerpts — nothing recorded, nothing
// fabricated), and a trailing call with no marker stays dangling.
func TestCursorReadTranscript(t *testing.T) {
	root := t.TempDir()
	projRoot := filepath.Join(root, "projects")
	writeCursorTranscript(t, root, "conv-1", cursorFixtureLines)
	a := NewWithOptions(nil, projRoot, filepath.Join(root, "chats"))

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "conv-1"}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != models.TranscriptUser || msgs[0].Text != "Build the feature" {
		t.Errorf("msg0 = %+v (user_query envelope must be stripped)", msgs[0])
	}
	a1 := msgs[1]
	if a1.Role != models.TranscriptAssistant || !strings.Contains(a1.Text, "On it.") {
		t.Errorf("msg1 = %+v", a1)
	}
	if len(a1.ToolCalls) != 1 || !a1.ToolCalls[0].Resolved || a1.ToolCalls[0].ResultExcerpt != "" {
		t.Errorf("turn-ended call = %+v, want resolved with empty excerpt", a1.ToolCalls)
	}
	if a1.ToolCalls[0].Name != "run_terminal_cmd" || !strings.Contains(a1.ToolCalls[0].InputExcerpt, "go test") {
		t.Errorf("call = %+v", a1.ToolCalls[0])
	}
	last := msgs[3]
	if len(last.ToolCalls) != 1 || last.ToolCalls[0].Resolved {
		t.Errorf("trailing call = %+v, want dangling (no turn_ended)", last.ToolCalls)
	}
	if !msgs[0].Time.IsZero() {
		t.Error("cursor transcripts carry no timestamps — times must stay zero")
	}
}

func TestCursorReadTranscript_MissingErrors(t *testing.T) {
	a := NewWithOptions(nil, filepath.Join(t.TempDir(), "projects"))
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "nope"}, []string{"cursor:hook"}); err == nil {
		t.Fatal("want error for missing transcript")
	}
}
