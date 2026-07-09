package hermes

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestHermesReadTranscript exercises the reader against the live
// testdata/hermes dumps (schema v14: active=1 stream, tool_calls JSON
// wrappers, role='tool' result rows pairing by tool_call_id, reasoning
// columns dropped).
func TestHermesReadTranscript(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	a := NewWithOptions(nil, filepath.Dir(dbPath))

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "20260605_154029_7b8623"}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) < 4 {
		t.Fatalf("messages = %d, want ≥ 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != models.TranscriptUser || msgs[0].Text != "Hi" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != models.TranscriptAssistant || !strings.Contains(msgs[1].Text, "How can I help") {
		t.Errorf("msg1 = %+v", msgs[1])
	}
	// The hello-world exchange: write_file + terminal calls, each paired
	// with a role='tool' result row.
	var sawWrite, sawTerminal bool
	for _, m := range msgs {
		if strings.Contains(m.Text, "greeting") && strings.Contains(m.Text, "simple") {
			t.Errorf("reasoning leaked into text: %q", m.Text)
		}
		for _, c := range m.ToolCalls {
			switch c.Name {
			case "write_file":
				sawWrite = true
				if !c.Resolved || !strings.Contains(c.ResultExcerpt, "bytes_written") {
					t.Errorf("write_file call = %+v, want resolved with structured result excerpt", c)
				}
			case "terminal":
				sawTerminal = true
				if !c.Resolved {
					t.Errorf("terminal call unresolved: %+v", c)
				}
			}
		}
		if m.Time.IsZero() {
			t.Errorf("REAL unix-seconds timestamps must map onto times: %+v", m)
		}
	}
	if !sawWrite || !sawTerminal {
		t.Errorf("sawWrite=%v sawTerminal=%v — fixture tool calls missing", sawWrite, sawTerminal)
	}
}

func TestHermesReadTranscript_MissingErrors(t *testing.T) {
	t.Parallel()
	a := NewWithOptions(nil, t.TempDir())
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "x"}, []string{"hermes:hook"}); err == nil {
		t.Fatal("want error when no state.db exists")
	}
}
