package antigravity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// Real-shape sample of transcript.jsonl captured 2026-05-24 from the
// operator's host (conversation 739cbb33). Trimmed to the lines this
// test needs to pin: USER_INPUT + PLANNER_RESPONSE pairs and a
// SYSTEM/CONVERSATION_HISTORY entry that must be ignored. The actual
// content wrappers (<USER_REQUEST>, <ADDITIONAL_METADATA>) are kept
// verbatim so the extractor's parsing is exercised against the live
// format.
const fakeTranscriptJSONL = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-23T18:36:24Z","content":"<USER_REQUEST>\nJust say ok\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-05-24T00:06:24+05:30.\n</ADDITIONAL_METADATA>"}
{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE","created_at":"2026-05-23T18:36:24Z"}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-23T18:36:24Z","content":"ok"}
{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-23T18:48:11Z","content":"<USER_REQUEST>\nmy name is bash 2\n</USER_REQUEST>"}
{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-23T18:48:11Z","content":"Nice to meet you, Bash 2. How can I help you today?"}
{"step_index":6,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-23T19:33:29Z","content":"<USER_REQUEST>\nmy name is bash 22\n</USER_REQUEST>"}
{"step_index":7,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-23T19:33:29Z","content":"Understood, Bash 22. How can I help you today?"}
{"step_index":8,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-23T19:34:00Z","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"git status\""}}]}
`

// setupTranscriptFixture writes a minimal CLI brain/ layout
// containing transcript.jsonl for one conversation. Returns the
// .pb path the adapter would parse.
func setupTranscriptFixture(t *testing.T, conversationID string) (pbPath string) {
	t.Helper()
	homeDir := t.TempDir()
	cliRoot := filepath.Join(homeDir, ".gemini", "antigravity-cli")
	convDir := filepath.Join(cliRoot, "conversations")
	brainDir := filepath.Join(cliRoot, "brain", conversationID, ".system_generated", "logs")
	for _, d := range []string{convDir, brainDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(brainDir, "transcript.jsonl"), []byte(fakeTranscriptJSONL), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	pbPath = filepath.Join(convDir, conversationID+".pb")
	if err := os.WriteFile(pbPath, []byte("dummy"), 0o644); err != nil {
		t.Fatalf("write pb: %v", err)
	}
	return pbPath
}

func TestExtractUserRequestText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "wrapped with USER_REQUEST + ADDITIONAL_METADATA",
			in:   "<USER_REQUEST>\nJust say ok\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nfoo\n</ADDITIONAL_METADATA>",
			want: "Just say ok",
		},
		{
			name: "wrapped with USER_REQUEST only",
			in:   "<USER_REQUEST>\nmy name is bash 2\n</USER_REQUEST>",
			want: "my name is bash 2",
		},
		{
			name: "no wrapper falls back to trim",
			in:   "  plain text  ",
			want: "plain text",
		},
		{
			name: "malformed wrapper falls back to trim",
			in:   "<USER_REQUEST>\nbroken",
			want: "<USER_REQUEST>\nbroken",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractUserRequestText(c.in); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestReadCLITranscriptEntries pins JSONL parsing including skip
// rules for non-DONE entries and the SYSTEM/CONVERSATION_HISTORY
// noop entries.
func TestReadCLITranscriptEntries(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))
	if len(entries) != 8 {
		t.Fatalf("got %d entries, want 8: %+v", len(entries), entries)
	}
}

// TestSynthesizeTranscriptEventsEmitsBothSides pins the core
// guarantee: transcript.jsonl produces user_prompt + task_complete
// events for every completed turn, including the assistant
// responses agy's gRPC ConvertTrajectoryToMarkdown silently drops.
func TestSynthesizeTranscriptEventsEmitsBothSides(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))

	out := synthesizeTranscriptEvents(
		pbPath,
		"aaaa1111-2222-3333-4444-555555555555",
		"/home/u/code/proj-a",
		"aaaa1111-2222-3333-4444-555555555555",
		scrub.New(),
		entries,
		nil,
		nil, nil,
	)
	var userCount, assistantCount int
	wantTargets := map[string]string{
		"Just say ok":       "user",
		"ok":                "assistant",
		"my name is bash 2": "user",
		"Nice to meet you, Bash 2. How can I help you today?": "assistant",
		"my name is bash 22":                             "user",
		"Understood, Bash 22. How can I help you today?": "assistant",
	}
	gotTargets := map[string]string{}
	for _, ev := range out {
		switch ev.ActionType {
		case models.ActionUserPrompt:
			userCount++
			gotTargets[ev.Target] = "user"
		case models.ActionTaskComplete:
			assistantCount++
			gotTargets[ev.Target] = "assistant"
		default:
			t.Errorf("unexpected ActionType: %v", ev.ActionType)
		}
	}
	if userCount != 3 {
		t.Errorf("user_prompt count = %d, want 3 (Just say ok, bash 2, bash 22)", userCount)
	}
	if assistantCount != 3 {
		t.Errorf("task_complete count = %d, want 3 (ok, Nice to meet you Bash 2, Understood Bash 22)", assistantCount)
	}
	for target, role := range wantTargets {
		if gotTargets[target] != role {
			t.Errorf("missing %s event for target %q (got role=%q)", role, target, gotTargets[target])
		}
	}
}

// TestSynthesizeTranscriptDedupesAgainstExisting pins that turns
// already represented in the bridge's events (compared by Target
// text) are skipped — avoids double-emitting the same content.
func TestSynthesizeTranscriptDedupesAgainstExisting(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))

	// Simulate the bridge having already surfaced the first turn pair.
	existing := []models.ToolEvent{
		{ActionType: models.ActionUserPrompt, Target: "Just say ok"},
		{ActionType: models.ActionTaskComplete, Target: "ok"},
	}
	out := synthesizeTranscriptEvents(
		pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "aaaa1111-2222-3333-4444-555555555555",
		scrub.New(), entries, existing,
		nil, nil,
	)
	for _, ev := range out {
		if ev.Target == "Just say ok" || ev.Target == "ok" {
			t.Errorf("event for already-covered target %q was synthesized: %+v", ev.Target, ev)
		}
	}
	// 6 entries minus 2 already covered = 4 expected
	if len(out) != 4 {
		t.Errorf("got %d events, want 4 (6 total - 2 deduped): %+v", len(out), out)
	}
}

// TestSourceEventIDsAreStablePerStep pins that re-parsing the same
// transcript produces the same SourceEventIDs — the
// (source_file, source_event_id) UNIQUE constraint relies on this
// for idempotent re-ingestion.
func TestSourceEventIDsAreStablePerStep(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))

	out1 := synthesizeTranscriptEvents(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "aaaa1111-2222-3333-4444-555555555555", scrub.New(), entries, nil, nil, nil)
	out2 := synthesizeTranscriptEvents(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "aaaa1111-2222-3333-4444-555555555555", scrub.New(), entries, nil, nil, nil)
	if len(out1) != len(out2) {
		t.Fatalf("non-deterministic event count: %d vs %d", len(out1), len(out2))
	}
	for i := range out1 {
		if out1[i].SourceEventID != out2[i].SourceEventID {
			t.Errorf("event %d SourceEventID drifted: %q vs %q", i, out1[i].SourceEventID, out2[i].SourceEventID)
		}
	}
}

// TestSynthesizeTranscriptDedupesAgainstExtraCoverage pins the
// DB-aware dedup path (Phase 1 of the
// antigravity-token-coverage-design-2026-05-24 doc): when prior
// parse cycles already persisted struct.user_prompt / task_complete
// rows for a conversation, a later cycle that reaches synth with an
// EMPTY in-memory `existing` must still dedup against those persisted
// Targets — otherwise it re-emits every transcript entry, landing
// duplicates that the (source_file, source_event_id) UNIQUE
// constraint can't catch (struct + transcript use different SEID
// namespaces).
func TestSynthesizeTranscriptDedupesAgainstExtraCoverage(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))

	// Scenario: a prior parse cycle persisted the first two turn pairs
	// (struct.user_prompt + structured.assistant_text). This cycle's
	// in-memory existing is empty (the historyOnlyResult call path).
	// Extra coverage from the DB must suppress those two transcript
	// pairs.
	extraU := []string{"Just say ok", "my name is bash 2"}
	extraA := []string{"ok", "Nice to meet you, Bash 2. How can I help you today?"}

	out := synthesizeTranscriptEvents(
		pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "aaaa1111-2222-3333-4444-555555555555",
		scrub.New(), entries, nil,
		extraU, extraA,
	)
	for _, ev := range out {
		switch ev.Target {
		case "Just say ok", "ok",
			"my name is bash 2", "Nice to meet you, Bash 2. How can I help you today?":
			t.Errorf("DB-covered target %q leaked into synth output: %+v", ev.Target, ev)
		}
	}
	// 6 transcript entries total minus 4 DB-covered (2 user + 2 asst)
	// = 2 expected (the third turn's user + assistant).
	if len(out) != 2 {
		t.Errorf("got %d events, want 2 (6 total - 4 DB-covered)", len(out))
	}
}

// TestAugmentResultPrefersTranscriptOverHistory pins that when both
// transcript.jsonl AND history.jsonl exist, transcript wins
// (assistant responses are only there). history is only used as a
// safety net when transcript is missing.
func TestAugmentResultPrefersTranscriptOverHistory(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	// Add a history.jsonl that mentions a message NOT in transcript.
	// Transcript-preferred behaviour means this history-only message
	// must NOT appear in the synthesized events.
	if err := os.WriteFile(filepath.Join(cliRoot, "history.jsonl"),
		[]byte(`{"display":"history-only message","timestamp":1779562091484,"conversationId":"aaaa1111-2222-3333-4444-555555555555"}`+"\n"),
		0o644); err != nil {
		t.Fatalf("write history: %v", err)
	}
	a := NewWithOptions(nil, filepath.Dir(pbPath))
	res := adapter.ParseResult{}
	got := a.augmentResultFromHistory(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", &res)
	if got < 6 {
		t.Errorf("expected at least 6 events from transcript, got %d", got)
	}
	for _, ev := range res.ToolEvents {
		if ev.Target == "history-only message" {
			t.Errorf("history-only message leaked into transcript-preferred result: %+v", ev)
		}
	}
}
