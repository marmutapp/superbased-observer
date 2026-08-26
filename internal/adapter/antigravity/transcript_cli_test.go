package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		"",
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
		case models.ActionAssistantMessage:
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
		t.Errorf("assistant_message count = %d, want 3 (ok, Nice to meet you Bash 2, Understood Bash 22)", assistantCount)
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
//
// It runs over BOTH assistant action-type spellings. The WP-T6/B2 sweep
// re-typed the structured/transcript assistant-text emit sites from
// task_complete to assistant_message, but a real database is mixed:
// rows ingested before the sweep (and before migration 078 repairs
// them) still carry task_complete, while rows from the same conversation
// ingested after it carry assistant_message — and the genuinely-terminal
// structured.final_summary / markdown.planner_response rows carry
// task_complete forever. The coverage set must therefore close the
// duplicate-assistant-row bug against either spelling; matching one only
// would silently reopen it for half the corpus.
func TestSynthesizeTranscriptDedupesAgainstExisting(t *testing.T) {
	cases := []struct {
		name       string
		assistType string
	}{
		{"post_sweep_assistant_message", models.ActionAssistantMessage},
		{"pre_sweep_task_complete", models.ActionTaskComplete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
			cliRoot, _ := cliRootsFor(pbPath)
			entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))

			// Simulate the bridge having already surfaced the first turn pair.
			existing := []models.ToolEvent{
				{ActionType: models.ActionUserPrompt, Target: "Just say ok"},
				{ActionType: tc.assistType, Target: "ok"},
			}
			out := synthesizeTranscriptEvents(
				pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "", "aaaa1111-2222-3333-4444-555555555555",
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
		})
	}
}

// TestSynthesizeTranscriptDedupesAgainstMixedTypedHistory is the
// mid-upgrade case in one call: a conversation whose persisted rows
// carry BOTH spellings — an older task_complete row for one turn, a
// newer assistant_message row for another. Both must be recognised as
// covered in a single pass.
func TestSynthesizeTranscriptDedupesAgainstMixedTypedHistory(t *testing.T) {
	pbPath := setupTranscriptFixture(t, "aaaa1111-2222-3333-4444-555555555555")
	cliRoot, _ := cliRootsFor(pbPath)
	entries := readCLITranscriptEntries(cliTranscriptPath(cliRoot, "aaaa1111-2222-3333-4444-555555555555"))

	existing := []models.ToolEvent{
		{ActionType: models.ActionUserPrompt, Target: "Just say ok"},
		{ActionType: models.ActionTaskComplete, Target: "ok"},
		{ActionType: models.ActionAssistantMessage, Target: "Nice to meet you, Bash 2. How can I help you today?"},
	}
	out := synthesizeTranscriptEvents(
		pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "", "aaaa1111-2222-3333-4444-555555555555",
		scrub.New(), entries, existing,
		nil, nil,
	)
	covered := map[string]bool{
		"Just say ok": true,
		"ok":          true,
		"Nice to meet you, Bash 2. How can I help you today?": true,
	}
	for _, ev := range out {
		if covered[ev.Target] {
			t.Errorf("event for already-covered target %q was synthesized: %+v", ev.Target, ev)
		}
	}
	// 6 entries minus 3 already covered = 3 expected.
	if len(out) != 3 {
		t.Errorf("got %d events, want 3 (6 total - 3 deduped): %+v", len(out), out)
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

	out1 := synthesizeTranscriptEvents(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "", "aaaa1111-2222-3333-4444-555555555555", scrub.New(), entries, nil, nil, nil)
	out2 := synthesizeTranscriptEvents(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "", "aaaa1111-2222-3333-4444-555555555555", scrub.New(), entries, nil, nil, nil)
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
		pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "", "aaaa1111-2222-3333-4444-555555555555",
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
	got := a.augmentResultFromHistory(pbPath, "aaaa1111-2222-3333-4444-555555555555", "/p", "", &res)
	if got < 6 {
		t.Errorf("expected at least 6 events from transcript, got %d", got)
	}
	for _, ev := range res.ToolEvents {
		if ev.Target == "history-only message" {
			t.Errorf("history-only message leaked into transcript-preferred result: %+v", ev)
		}
	}
}

// toolCallTranscriptJSONL is the real desktop-sidecar shape captured
// 2026-08-01 from conversation b070924e's overview.txt. Step 17
// carries a two-element tool_calls ARRAY whose results the structured
// path emitted as SEPARATE rows at steps 18 and 19 — the fan-out that
// makes any fixed step offset unusable as a join key. Step 18 is the
// typed RESULT entry (content = returned data, not the invocation).
const toolCallTranscriptJSONL = `{"step_index":17,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-06-01T16:48:00Z","tool_calls":[{"name":"view_file","args":{"AbsolutePath":"\"/home/marmutapp/superbased-observer/cmd/observer/main.go\"","toolAction":"\"Reading main.go\""}},{"name":"view_file","args":{"AbsolutePath":"\"/home/marmutapp/superbased-observer/cmd/observer/start.go\"","toolAction":"\"Reading start.go\""}}]}
{"step_index":18,"source":"MODEL","type":"VIEW_FILE","status":"DONE","created_at":"2026-06-01T16:48:01Z","content":"package main\n\nfunc main() {}"}
{"step_index":64,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-06-01T17:00:00Z","tool_calls":[{"name":"write_to_file","args":{"CodeContent":"\"# doc\""}}]}
`

// TestSynthesizeTranscriptEvents_ToolCallsAreNotPromoted pins the
// deliberate deferral documented on cliTranscriptEntry: tool_calls is
// decoded but MUST NOT mint action rows, because the structured /
// bridge path already emits those same invocations under a different
// SourceEventID namespace (see the sibling dedup test for why no
// existing gate can suppress the overlap).
//
// This is a guard, not a feature test. If a future change promotes
// tool_calls, this fails loudly — the correct response is to add the
// missing tool-coverage bucket to TargetCoverageReader, not to relax
// the assertion.
func TestSynthesizeTranscriptEvents_ToolCallsAreNotPromoted(t *testing.T) {
	toolActionTypes := map[string]bool{
		models.ActionReadFile:    true,
		models.ActionEditFile:    true,
		models.ActionRunCommand:  true,
		models.ActionSearchText:  true,
		models.ActionSearchFiles: true,
	}
	cases := []struct {
		name  string
		jsonl string
	}{
		{
			// Real bindings first: the captured shapes that actually
			// carry tool_calls.
			name:  "desktop multi-call array plus typed result entry",
			jsonl: toolCallTranscriptJSONL,
		},
		{
			name:  "cli single run_command invocation",
			jsonl: fakeTranscriptJSONL,
		},
		{
			// Decoys AFTER the real bindings: entries that must keep
			// emitting their normal text rows, so a blanket
			// "emit nothing" regression can't pass this table.
			name:  "text-only turns still emit",
			jsonl: "{\"step_index\":0,\"source\":\"USER_EXPLICIT\",\"type\":\"USER_INPUT\",\"status\":\"DONE\",\"created_at\":\"2026-06-01T16:48:00Z\",\"content\":\"<USER_REQUEST>\\nhi\\n</USER_REQUEST>\"}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "transcript.jsonl")
			if err := os.WriteFile(path, []byte(tc.jsonl), 0o644); err != nil {
				t.Fatalf("write transcript: %v", err)
			}
			entries := readCLITranscriptEntries(path)
			if len(entries) == 0 {
				t.Fatalf("no entries decoded from fixture")
			}
			out := synthesizeTranscriptEvents(
				"/conv.pb", "conv", "/p", "", "conv",
				scrub.New(), entries, nil, nil, nil,
			)
			for _, ev := range out {
				if toolActionTypes[ev.ActionType] {
					t.Errorf("tool_calls was promoted to a tool action row (%s / %s, target=%q); "+
						"the structured path already emits this invocation and no dedup bucket exists for it",
						ev.ActionType, ev.RawToolName, ev.Target)
				}
			}
		})
	}
}

// TestTranscriptToolTargetsCannotDedupAgainstStructured is the
// evidence behind the deferral: it shows that the ONE gate a promotion
// would reach for — Target-keyed coverage, the mechanism that already
// dedups user/assistant text — provably cannot catch a duplicated tool
// row, because the two paths spell the same file two different ways.
//
// Removing the deferral without also fixing this mismatch produces
// silent double-counting, which is why the assertion is on the seam
// (the Target strings themselves) rather than on emitted rows.
func TestTranscriptToolTargetsCannotDedupAgainstStructured(t *testing.T) {
	const absPath = "/home/marmutapp/superbased-observer/cmd/observer/main.go"

	// What the structured path stores for this file. decodeFileURIToPath
	// strips "file:///" INCLUDING the third slash, so the leading slash
	// is gone — matching the live DB, which holds
	// "home/marmutapp/superbased-observer/cmd/observer/main.go".
	structuredTarget := truncate(decodeFileURIToPath("file://"+absPath), 200)
	if strings.HasPrefix(structuredTarget, "/") {
		t.Fatalf("premise drifted: structured target %q unexpectedly keeps its leading slash", structuredTarget)
	}

	// What transcript.jsonl carries for the SAME invocation: the raw
	// tool_calls arg, JSON-quoted and absolute.
	entries := decodeToolCallArgs(t, toolCallTranscriptJSONL)
	if len(entries) == 0 {
		t.Fatalf("fixture carried no tool_calls args")
	}
	transcriptArg := entries[0]
	if transcriptArg != `"`+absPath+`"` {
		t.Fatalf("fixture drifted: got %q, want the JSON-quoted absolute path", transcriptArg)
	}

	// The gate is byte-exact (see TargetCoverageReader's docstring).
	if structuredTarget == transcriptArg {
		t.Fatalf("targets matched — the dedup premise would hold and this guard is stale")
	}
	// ...and it still misses after the obvious normalisation, because
	// of the leading slash. Both mismatches must be fixed before a
	// Target-keyed gate can be trusted.
	if unquoted := strings.Trim(transcriptArg, `"`); structuredTarget == unquoted {
		t.Fatalf("targets matched after unquoting — only the quoting differs, not the path shape")
	}
}

// decodeToolCallArgs pulls the first string arg out of every
// tool_calls entry in a transcript fixture, in file order. Test-only
// helper: the production parser deliberately does not decode these.
func decodeToolCallArgs(t *testing.T, jsonl string) []string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	var out []string
	for _, e := range readCLITranscriptEntries(path) {
		if len(e.ToolCalls) == 0 {
			continue
		}
		var calls []struct {
			Name string            `json:"name"`
			Args map[string]string `json:"args"`
		}
		if err := json.Unmarshal(e.ToolCalls, &calls); err != nil {
			t.Fatalf("decode tool_calls: %v", err)
		}
		for _, c := range calls {
			if v, ok := c.Args["AbsolutePath"]; ok {
				out = append(out, v)
			}
		}
	}
	return out
}
