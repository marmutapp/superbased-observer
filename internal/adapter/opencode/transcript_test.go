package opencode

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestOpenCodeReadTranscript exercises the reader against the message +
// part fixture (text/tool/reasoning/step/subtask parts): reasoning and
// step markers are dropped, tool parts carry call+result in one row.
func TestOpenCodeReadTranscript(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithVariant(t, dbPath)
	a := NewWithOptions(nil, []string{root})

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "ses_1"}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2 (u a): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != models.TranscriptUser || msgs[0].Text != "Do work" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	a1 := msgs[1]
	if a1.Role != models.TranscriptAssistant || a1.Model != "gpt-5.4-nano" {
		t.Errorf("msg1 role/model = %s/%s", a1.Role, a1.Model)
	}
	if a1.Text != "Working on it." {
		t.Errorf("msg1 text = %q (reasoning/step parts must be dropped)", a1.Text)
	}
	if len(a1.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want 1", a1.ToolCalls)
	}
	c := a1.ToolCalls[0]
	if c.Name != "bash" || !c.Resolved || c.ResultExcerpt != "ok" || c.ID != "c1" {
		t.Errorf("call = %+v", c)
	}
	if a1.Time.IsZero() || !a1.Time.After(msgs[0].Time) {
		t.Errorf("times = %v / %v — assistant must use completed time", msgs[0].Time, a1.Time)
	}
}

func TestOpenCodeReadTranscript_MissingErrors(t *testing.T) {
	a := NewWithOptions(nil, []string{t.TempDir()})
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "x"}, nil); err == nil {
		t.Fatal("want error when no opencode.db exists")
	}
}
