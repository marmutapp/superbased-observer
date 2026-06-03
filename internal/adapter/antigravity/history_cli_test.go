package antigravity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

const fakeHistoryJSONL = `{"display":"Just say ok","timestamp":1779561384748,"workspace":"C:\\some\\workspace"}
{"display":"hey","timestamp":1779562038857,"workspace":"C:\\some\\workspace","conversationId":"aaaa1111-2222-3333-4444-555555555555"}
{"display":"my name is bash 1","timestamp":1779562080510,"workspace":"C:\\some\\workspace","conversationId":"bbbb1111-2222-3333-4444-555555555555"}
{"display":"my name is bash 2","timestamp":1779562091484,"workspace":"C:\\some\\workspace","conversationId":"aaaa1111-2222-3333-4444-555555555555"}
{"display":"my name is bash 22","timestamp":1779564809690,"workspace":"C:\\some\\workspace","conversationId":"aaaa1111-2222-3333-4444-555555555555"}
`

// setupHistoryFixture writes the fake history.jsonl into a tmp CLI
// layout and returns the .pb path the adapter would parse.
func setupHistoryFixture(t *testing.T, conversationID string) (pbPath string) {
	t.Helper()
	homeDir := t.TempDir()
	cliRoot := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	convDir := filepath.Join(cliRoot, "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cliRoot, "history.jsonl"), []byte(fakeHistoryJSONL), 0o644); err != nil {
		t.Fatalf("write history: %v", err)
	}
	pbPath = filepath.Join(convDir, conversationID+".pb")
	if err := os.WriteFile(pbPath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("write pb: %v", err)
	}
	return pbPath
}

// TestReadCLIHistoryEntries pins JSONL parsing including the skip
// rules for entries missing display / timestamp / conversationId.
func TestReadCLIHistoryEntries(t *testing.T) {
	pbPath := setupHistoryFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLIHistoryEntries(cliRoot)
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5: %+v", len(entries), entries)
	}
	// First entry has no conversationId (matches real-host shape
	// for the very first message of a new agy session).
	if entries[0].ConversationID != "" {
		t.Errorf("first entry conversationId = %q, want empty", entries[0].ConversationID)
	}
	if entries[0].Display != "Just say ok" {
		t.Errorf("first entry display = %q, want 'Just say ok'", entries[0].Display)
	}
}

// TestSynthesizeHistoryUserPromptsFiltersByConv pins that only
// matching-conversation entries are emitted as ToolEvents.
func TestSynthesizeHistoryUserPromptsFiltersByConv(t *testing.T) {
	pbPath := setupHistoryFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLIHistoryEntries(cliRoot)

	out := synthesizeHistoryUserPrompts(
		pbPath,
		"aaaa1111-2222-3333-4444-555555555555",
		"/home/u/code/proj-a",
		"aaaa1111-2222-3333-4444-555555555555",
		scrub.New(),
		entries,
		nil,
	)
	if len(out) != 3 {
		t.Fatalf("got %d events for conv aaaa, want 3 (hey, bash 2, bash 22): %+v", len(out), out)
	}
	for _, ev := range out {
		if ev.ActionType != models.ActionUserPrompt {
			t.Errorf("synthesized event must be ActionUserPrompt, got %v", ev.ActionType)
		}
		if ev.Tool != models.ToolAntigravity {
			t.Errorf("Tool = %v, want antigravity", ev.Tool)
		}
		if ev.SessionID != "aaaa1111-2222-3333-4444-555555555555" {
			t.Errorf("SessionID = %q, want aaaa1111-...", ev.SessionID)
		}
		if !strings.HasPrefix(ev.SourceEventID, "antigravity-cli-history:") {
			t.Errorf("SourceEventID = %q, want antigravity-cli-history: prefix", ev.SourceEventID)
		}
	}
}

// TestSynthesizeHistoryUserPromptsDedupesAgainstExisting pins that
// history entries whose text appears in `existing` are skipped —
// avoids double-emitting user_prompt rows the bridge already
// surfaced.
func TestSynthesizeHistoryUserPromptsDedupesAgainstExisting(t *testing.T) {
	pbPath := setupHistoryFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLIHistoryEntries(cliRoot)

	// Simulate the bridge having already surfaced "hey".
	existing := []models.ToolEvent{
		{ActionType: models.ActionUserPrompt, Target: "hey"},
	}

	out := synthesizeHistoryUserPrompts(
		pbPath,
		"aaaa1111-2222-3333-4444-555555555555",
		"/home/u/code/proj-a",
		"aaaa1111-2222-3333-4444-555555555555",
		scrub.New(),
		entries,
		existing,
	)
	for _, ev := range out {
		if ev.Target == "hey" {
			t.Errorf("'hey' was already in existing — should have been filtered, but got: %+v", ev)
		}
	}
	if len(out) != 2 {
		t.Errorf("got %d events, want 2 (bash 2 + bash 22; hey deduped): %+v", len(out), out)
	}
}

// TestSourceEventIDsAreIdempotent pins that the synthesized event ID
// is deterministic across re-parses — the store-level UNIQUE
// constraint relies on this for idempotent re-ingestion of the same
// .pb file.
func TestSourceEventIDsAreIdempotent(t *testing.T) {
	pbPath := setupHistoryFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLIHistoryEntries(cliRoot)

	out1 := synthesizeHistoryUserPrompts(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "aaaa1111-2222-3333-4444-555555555555", scrub.New(), entries, nil)
	out2 := synthesizeHistoryUserPrompts(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "aaaa1111-2222-3333-4444-555555555555", scrub.New(), entries, nil)
	if len(out1) != len(out2) {
		t.Fatalf("non-deterministic event count: %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i].SourceEventID != out2[i].SourceEventID {
			t.Errorf("event %d SourceEventID drifted: %q vs %q", i, out1[i].SourceEventID, out2[i].SourceEventID)
		}
	}
}
