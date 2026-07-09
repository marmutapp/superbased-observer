package devin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestReadTranscript_ActiveChain(t *testing.T) {
	a := NewWithOptions(nil, []string{filepath.Dir(fixtureDB)})
	msgs, err := a.ReadTranscript(context.Background(),
		models.Session{ID: "cobalt-fruit", Tool: models.ToolDevin},
		[]string{fixtureDB})
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("expected >=2 transcript messages, got %d", len(msgs))
	}
	if msgs[0].Role != models.TranscriptUser {
		t.Errorf("first message role = %q, want user", msgs[0].Role)
	}
	if msgs[0].Text != "Create a file hello.txt containing hi and run ls" {
		t.Errorf("first user text = %q", msgs[0].Text)
	}

	// The dead-branch draft text must not appear anywhere.
	for _, m := range msgs {
		if m.Text == "draft answer" {
			t.Error("dead regeneration branch leaked into the transcript")
		}
	}

	// The assistant exchange should carry the write + exec tool calls,
	// resolved with their outputs.
	var foundWrite bool
	for _, m := range msgs {
		if m.Role != models.TranscriptAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "call_write1" {
				foundWrite = true
				if !tc.Resolved {
					t.Error("write call should be resolved")
				}
				if tc.ResultExcerpt == "" {
					t.Error("write call should carry its result excerpt")
				}
			}
		}
	}
	if !foundWrite {
		t.Error("write tool call missing from transcript")
	}
}

func TestReadTranscript_UnknownSession(t *testing.T) {
	a := NewWithOptions(nil, []string{filepath.Dir(fixtureDB)})
	msgs, err := a.ReadTranscript(context.Background(),
		models.Session{ID: "no-such-session"}, []string{fixtureDB})
	if err != nil {
		t.Fatalf("ReadTranscript on unknown session should not error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("unknown session should yield 0 messages, got %d", len(msgs))
	}
}

func TestStoreDBPath_NoStore(t *testing.T) {
	a := NewWithOptions(nil, []string{t.TempDir()})
	if _, err := a.storeDBPath(nil); err == nil {
		t.Error("expected error when no sessions.db exists")
	}
}
