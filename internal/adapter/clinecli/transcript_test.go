package clinecli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestClineCLIReadTranscript exercises the reader against the real
// testdata/clinecli/sample-session-messages.json live capture (schema v1:
// `<user_input>` envelopes, modelInfo on assistant rows, structured
// tool_result content lists).
func TestClineCLIReadTranscript(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "clinecli", "sample-session-messages.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	const sid = "1780701711502_0v8d2"
	dir := filepath.Join(root, "data", "sessions", sid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".messages.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewWithOptions(nil, root)

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: sid}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("messages = %d, want ≥ 2", len(msgs))
	}
	if msgs[0].Role != models.TranscriptUser || msgs[0].Text != "Hi" {
		t.Errorf("msg0 = %+v (user_input envelope must be stripped)", msgs[0])
	}
	a1 := msgs[1]
	if a1.Role != models.TranscriptAssistant || a1.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("msg1 role/model = %s/%s", a1.Role, a1.Model)
	}
	if strings.Contains(a1.Text, "session protocol in the AGENTS.md") && strings.Contains(a1.Text, "greeting me") {
		t.Error("thinking must be dropped")
	}
	var calls, resolved int
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			calls++
			if c.Resolved {
				resolved++
			}
		}
	}
	if calls == 0 || resolved == 0 {
		t.Errorf("calls=%d resolved=%d — tool pairing broken", calls, resolved)
	}
	if msgs[0].Time.IsZero() {
		t.Error("ts millis must map onto message times")
	}
}

func TestClineCLIReadTranscript_MissingErrors(t *testing.T) {
	a := NewWithOptions(nil, t.TempDir())
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "absent"}, nil); err == nil {
		t.Fatal("want error for missing messages.json")
	}
}
