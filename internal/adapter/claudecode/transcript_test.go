package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func writeTranscriptFixture(t *testing.T, root, sessionID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(root, "-home-u-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

var ccFixtureLines = []string{
	`{"type":"user","timestamp":"2026-07-03T09:59:59Z","message":{"role":"user","content":"<local-command-caveat>Caveat: the messages below were generated while running local commands.</local-command-caveat>"}}`,
	`{"type":"user","timestamp":"2026-07-03T09:59:59.5Z","message":{"role":"user","content":"<task-notification>background task 42 finished</task-notification>"}}`,
	`{"type":"user","timestamp":"2026-07-03T10:00:00Z","message":{"role":"user","content":"build the feature"}}`,
	`{"type":"assistant","timestamp":"2026-07-03T10:00:10Z","message":{"role":"assistant","model":"opus-4-8","content":[{"type":"thinking","thinking":"secret plan"},{"type":"text","text":"On it."},{"type":"tool_use","id":"tu1","name":"Edit","input":{"file":"a.go"}}]}}`,
	`{"type":"user","timestamp":"2026-07-03T10:00:20Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"edit ok"}]}]}}`,
	`{"type":"assistant","timestamp":"2026-07-03T10:00:30Z","message":{"role":"assistant","model":"opus-4-8","content":[{"type":"text","text":"Done."}]}}`,
	`{"type":"assistant","timestamp":"2026-07-03T10:00:31Z","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"subagent chatter"}]}}`,
	`{"type":"user","timestamp":"2026-07-03T10:01:00Z","message":{"role":"user","content":"now part two"}}`,
	`{"type":"assistant","timestamp":"2026-07-03T10:01:10Z","message":{"role":"assistant","model":"opus-4-8","content":[{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"go test"}}]}}`,
}

func TestReadTranscript_NormalizesExchanges(t *testing.T) {
	root := t.TempDir()
	writeTranscriptFixture(t, root, "cc-sess-1", ccFixtureLines)
	a := NewWithOptions(nil, root)

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "cc-sess-1"}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	// user / assistant(merged: text+tool, then "Done.") / user / assistant(dangling)
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != models.TranscriptUser || msgs[0].Text != "build the feature" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	as := msgs[1]
	if as.Role != models.TranscriptAssistant || as.Model != "opus-4-8" {
		t.Errorf("msg1 role/model = %s/%s", as.Role, as.Model)
	}
	if strings.Contains(as.Text, "secret plan") {
		t.Error("thinking must be dropped")
	}
	if !strings.Contains(as.Text, "On it.") || !strings.Contains(as.Text, "Done.") {
		t.Errorf("consecutive assistant records must merge: %q", as.Text)
	}
	if len(as.ToolCalls) != 1 || !as.ToolCalls[0].Resolved || as.ToolCalls[0].ResultExcerpt != "edit ok" {
		t.Errorf("tool call must resolve from the user-carrier record: %+v", as.ToolCalls)
	}
	if strings.Contains(as.Text, "subagent chatter") {
		t.Error("sidechain records must be skipped")
	}
	// The exchange time anchors on its LAST constituent record.
	if got := as.Time.Format("15:04:05"); got != "10:00:30" {
		t.Errorf("assistant exchange time = %s, want 10:00:30", got)
	}
	last := msgs[3]
	if len(last.ToolCalls) != 1 || last.ToolCalls[0].Resolved {
		t.Errorf("trailing call must stay unresolved: %+v", last.ToolCalls)
	}
}

// TestReadTranscript_DerivesPathFromSessionID pins D-P0.1: hook-fed
// sessions carry no usable source path — derivation by session id is the
// primary lookup.
func TestReadTranscript_DerivesPathFromSessionID(t *testing.T) {
	root := t.TempDir()
	writeTranscriptFixture(t, root, "cc-sess-2", ccFixtureLines)
	a := NewWithOptions(nil, root)
	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "cc-sess-2"}, []string{"claude-code:hook"})
	if err != nil || len(msgs) == 0 {
		t.Fatalf("derivation failed: %v (%d msgs)", err, len(msgs))
	}
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "missing"}, nil); err == nil {
		t.Fatal("missing transcript must error")
	}
}

func TestReadTranscript_PrefersRealHint(t *testing.T) {
	root := t.TempDir()
	path := writeTranscriptFixture(t, root, "cc-sess-3", ccFixtureLines)
	// Adapter pointed at an empty root: only the hint can resolve.
	a := NewWithOptions(nil, t.TempDir())
	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "cc-sess-3"}, []string{path})
	if err != nil || len(msgs) != 4 {
		t.Fatalf("hint path failed: %v (%d msgs)", err, len(msgs))
	}
}
