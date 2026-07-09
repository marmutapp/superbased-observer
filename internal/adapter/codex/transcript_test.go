package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

var codexFixtureLines = []string{
	`{"timestamp":"2026-07-03T09:00:00Z","type":"event_msg","payload":{"type":"user_message","message":"run the tests"}}`,
	`{"timestamp":"2026-07-03T09:00:05Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"private chain"}]}}`,
	`{"timestamp":"2026-07-03T09:00:06Z","type":"event_msg","payload":{"type":"agent_message","message":"Running them now."}}`,
	`{"timestamp":"2026-07-03T09:00:07Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"go\",\"test\"]}","call_id":"c1"}}`,
	`{"timestamp":"2026-07-03T09:00:09Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"ok  \tpkg\t0.1s"}}`,
	`{"timestamp":"2026-07-03T09:00:10Z","type":"event_msg","payload":{"type":"agent_message","message":"All green."}}`,
	`{"timestamp":"2026-07-03T09:01:00Z","type":"event_msg","payload":{"type":"user_message","message":"now lint"}}`,
	`{"timestamp":"2026-07-03T09:01:02Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"make\",\"lint\"]}","call_id":"c2"}}`,
}

func writeRolloutFixture(t *testing.T, root, sessionID string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "07", "03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-03T09-00-00-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(codexFixtureLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTranscript_Codex(t *testing.T) {
	root := t.TempDir()
	writeRolloutFixture(t, root, "019e-test-uuid")
	a := NewWithOptions(nil, root)

	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "019e-test-uuid"}, nil)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != models.TranscriptUser || msgs[0].Text != "run the tests" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	as := msgs[1]
	if strings.Contains(as.Text, "private chain") {
		t.Error("reasoning records must be dropped")
	}
	if !strings.Contains(as.Text, "Running them now.") || !strings.Contains(as.Text, "All green.") {
		t.Errorf("agent messages must merge into one exchange: %q", as.Text)
	}
	if len(as.ToolCalls) != 1 || !as.ToolCalls[0].Resolved || !strings.Contains(as.ToolCalls[0].ResultExcerpt, "ok") {
		t.Errorf("function_call must pair with its output: %+v", as.ToolCalls)
	}
	last := msgs[3]
	if last.Role != models.TranscriptAssistant || len(last.ToolCalls) != 1 || last.ToolCalls[0].Resolved {
		t.Errorf("trailing call must stay unresolved: %+v", last)
	}
}

func TestReadTranscript_CodexHintAndMissing(t *testing.T) {
	root := t.TempDir()
	path := writeRolloutFixture(t, root, "019e-hint-uuid")
	a := NewWithOptions(nil, t.TempDir())
	msgs, err := a.ReadTranscript(context.Background(), models.Session{ID: "019e-hint-uuid"}, []string{path})
	if err != nil || len(msgs) != 4 {
		t.Fatalf("hint path failed: %v (%d msgs)", err, len(msgs))
	}
	if _, err := a.ReadTranscript(context.Background(), models.Session{ID: "absent"}, nil); err == nil {
		t.Fatal("missing rollout must error")
	}
}
