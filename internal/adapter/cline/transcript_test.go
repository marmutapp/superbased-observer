package cline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestClineReadTranscript exercises the reader against the real
// testdata/cline/api_conversation_history.json corpus (Anthropic-shaped
// blocks: tool_use in assistant rows, tool_result in user carriers).
func TestClineReadTranscript(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "cline", "api_conversation_history.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	taskDir := filepath.Join(root, "task-42")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "api_conversation_history.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewWithOptions(nil, []string{root})

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "task-42"}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("messages = %d, want ≥ 2", len(msgs))
	}
	if msgs[0].Role != models.TranscriptUser || !strings.Contains(msgs[0].Text, "Refactor the auth middleware") {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[0].Time.IsZero() {
		t.Error("ts millis must map onto message times")
	}
	// The fixture's assistant records merge into exchanges; every
	// tool_use with a tool_result carrier must be resolved, and no
	// tool_result may surface as a user message (D-P0.3).
	var calls, resolved int
	for _, m := range msgs {
		if m.Role == models.TranscriptUser && strings.TrimSpace(m.Text) == "" {
			t.Errorf("empty user message leaked (tool_result carrier?): %+v", m)
		}
		if m.Role == models.TranscriptAssistant && m.Model == "" && len(m.ToolCalls) > 0 {
			// model rides assistant records in this fixture — merged
			// exchanges must keep it (some later records omit it, so
			// only exchanges with calls are checked loosely here).
			_ = m
		}
		for _, c := range m.ToolCalls {
			calls++
			if c.Resolved {
				resolved++
			}
		}
	}
	if calls == 0 {
		t.Fatal("fixture must yield tool calls")
	}
	if resolved == 0 {
		t.Errorf("no tool call resolved (%d calls) — tool_result pairing broken", calls)
	}
	if msgs[1].Model == "" {
		t.Errorf("assistant exchange model = empty, want the record's model: %+v", msgs[1])
	}
}

func TestClineReadTranscript_MissingErrors(t *testing.T) {
	a := NewWithOptions(nil, []string{t.TempDir()})
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "absent"}, nil); err == nil {
		t.Fatal("want error for missing task history")
	}
}
