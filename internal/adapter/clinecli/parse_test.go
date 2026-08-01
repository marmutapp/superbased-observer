package clinecli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// TestParseSessionFile_FixtureCorpus is the full pipe end-to-end:
// load the captured testdata/clinecli/ corpus, run ParseSessionFile,
// assert the event mix.
//
// The fixture session (1780701711502_0v8d2) is a 6-message
// conversation: user "Hi" → assistant reads PROGRESS.md → user
// "what kind of model are you" → assistant answers. Exercises every
// content-block type that lands in the live capture: text, thinking,
// tool_use (read_files), tool_result.
func TestParseSessionFile_FixtureCorpus(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureCLineDataDir(t)

	a := New()
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// Count events by ActionType. Expected (per the fixture):
	//   - 1 session_start
	//   - 0 session_end (status="idle" — non-terminal)
	//   - 2 user_prompt (msgs 1, 5 — "Hi" and "what kind of model")
	//   - 3 task_complete (assistant_text rows on msgs 2, 4, 6)
	//   - 1 read_file (tool_use on msg 2: read_files)
	got := map[string]int{}
	for _, ev := range res.ToolEvents {
		got[ev.ActionType]++
	}
	if got[models.ActionSessionStart] != 1 {
		t.Errorf("session_start: %d; want 1", got[models.ActionSessionStart])
	}
	if got[models.ActionSessionEnd] != 0 {
		t.Errorf("session_end: %d; want 0 (status=idle non-terminal)", got[models.ActionSessionEnd])
	}
	if got[models.ActionUserPrompt] != 2 {
		t.Errorf("user_prompt: %d; want 2", got[models.ActionUserPrompt])
	}
	if got[models.ActionAssistantMessage] != 3 {
		t.Errorf("assistant_message: %d; want 3 (one per assistant_text block)", got[models.ActionAssistantMessage])
	}
	if got[models.ActionReadFile] != 1 {
		t.Errorf("read_file: %d; want 1 (read_files tool_use)", got[models.ActionReadFile])
	}

	// TokenEvents: 3 per-message rows ONLY (one per assistant message
	// with a metrics block). The session-level aggregate must NOT
	// coexist with them — per-message + aggregate was the 2026-06-11
	// operator-reported 2× double-count (the two rows differ on both
	// halves of the UNIQUE(source_file, source_event_id) key, so
	// nothing downstream merges them).
	if got, want := len(res.TokenEvents), 3; got != want {
		t.Errorf("len(TokenEvents) = %d; want %d (3 per-message, no session aggregate)", got, want)
	}
	for _, te := range res.TokenEvents {
		if te.SourceEventID == "tk:"+fixtureSessionID+":session_usage" {
			t.Errorf("session-level aggregate row emitted alongside per-message metrics rows — double-counts every token downstream")
		}
	}

	// NewOffset advances to the row's updated_at as UnixMilli.
	if res.NewOffset == 0 {
		t.Error("NewOffset = 0; want > 0 (watermark from sessions.updated_at)")
	}
}

// TestParseSessionFile_Idempotency confirms that running
// ParseSessionFile twice over the same fixture produces the same
// SourceEventIDs — the store layer's UNIQUE(source_file,
// source_event_id) drops the re-emits.
func TestParseSessionFile_Idempotency(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureCLineDataDir(t)

	a := New()
	res1, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	res2, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	if len(res1.ToolEvents) != len(res2.ToolEvents) {
		t.Fatalf("ToolEvent count drift: first=%d second=%d", len(res1.ToolEvents), len(res2.ToolEvents))
	}
	for i := range res1.ToolEvents {
		if res1.ToolEvents[i].SourceEventID != res2.ToolEvents[i].SourceEventID {
			t.Errorf("ToolEvent[%d] SourceEventID drift: first=%q second=%q",
				i, res1.ToolEvents[i].SourceEventID, res2.ToolEvents[i].SourceEventID)
		}
	}
	for i := range res1.TokenEvents {
		if res1.TokenEvents[i].SourceEventID != res2.TokenEvents[i].SourceEventID {
			t.Errorf("TokenEvent[%d] SourceEventID drift: first=%q second=%q",
				i, res1.TokenEvents[i].SourceEventID, res2.TokenEvents[i].SourceEventID)
		}
	}
}

// TestStripUserInputWrapper covers the regex helper. Cline CLI wraps
// every operator prompt in `<user_input mode="...">…</user_input>`;
// we strip the wrapper before emitting the user_prompt action target.
func TestStripUserInputWrapper(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{`<user_input mode="act">Hi</user_input>`, "Hi"},
		{`<user_input mode="plan">Plan something</user_input>`, "Plan something"},
		{`<user_input mode="chat">Just chat</user_input>`, "Just chat"},
		{`<user_input>no mode attr</user_input>`, "no mode attr"},
		{"no wrapper", "no wrapper"},
		{"", ""},
		{`<user_input mode="act">multi
line
body</user_input>`, "multi\nline\nbody"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := stripUserInputWrapper(tc.in)
			if got != tc.want {
				t.Errorf("stripUserInputWrapper(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDecodeToolResultContent covers the structured-list shape
// (Phase 0 reality-check find) plus the bare-string fallback.
func TestDecodeToolResultContent(t *testing.T) {
	t.Parallel()

	t.Run("structured_list_single_success", func(t *testing.T) {
		t.Parallel()
		b := messageBlock{
			Content: json.RawMessage(`[{"query":"/x","result":"file body","success":true}]`),
		}
		body, ok := decodeToolResultContent(b)
		if !ok {
			t.Error("success = false; want true")
		}
		if body != "file body" {
			t.Errorf("body = %q; want %q", body, "file body")
		}
	})
	t.Run("structured_list_with_failure_AND_isError", func(t *testing.T) {
		t.Parallel()
		b := messageBlock{
			IsError: true,
			Content: json.RawMessage(`[{"query":"/x","result":"","success":false,"error":"file not found"}]`),
		}
		body, ok := decodeToolResultContent(b)
		if ok {
			t.Error("success = true; want false")
		}
		if !strings.Contains(body, "file not found") {
			t.Errorf("body = %q; want substring 'file not found'", body)
		}
	})
	t.Run("structured_list_mixed_success", func(t *testing.T) {
		t.Parallel()
		b := messageBlock{
			Content: json.RawMessage(`[
				{"query":"/a","result":"AAA","success":true},
				{"query":"/b","result":"BBB","success":true},
				{"query":"/c","result":"","success":false,"error":"nope"}
			]`),
		}
		body, ok := decodeToolResultContent(b)
		if ok {
			t.Error("success = true; want false (one item failed)")
		}
		if !strings.Contains(body, "AAA") || !strings.Contains(body, "BBB") {
			t.Errorf("body = %q; want both AAA and BBB present", body)
		}
	})
	t.Run("bare_string_fallback", func(t *testing.T) {
		t.Parallel()
		b := messageBlock{
			Content: json.RawMessage(`"raw string body"`),
		}
		body, ok := decodeToolResultContent(b)
		if !ok {
			t.Error("success = false; want true (no is_error)")
		}
		if body != "raw string body" {
			t.Errorf("body = %q; want %q", body, "raw string body")
		}
	})
	t.Run("empty_content", func(t *testing.T) {
		t.Parallel()
		b := messageBlock{}
		body, ok := decodeToolResultContent(b)
		if !ok {
			t.Error("success = false; want true (no is_error)")
		}
		if body != "" {
			t.Errorf("body = %q; want \"\"", body)
		}
	})
}

// TestExtractTarget covers the per-tool target extraction. Critical
// for the dashboard's "what file / command was this row about"
// surface.
func TestExtractTarget(t *testing.T) {
	t.Parallel()
	sc := scrub.New()
	cases := []struct {
		tool, input, want string
	}{
		{
			tool:  "read_files",
			input: `{"files":[{"path":"/proj/main.go"}]}`,
			want:  "/proj/main.go",
		},
		{
			tool:  "read_files",
			input: `{"files":[{"path":"/proj/a.go"},{"path":"/proj/b.go"},{"path":"/proj/c.go"}]}`,
			want:  "/proj/a.go (+2 more)",
		},
		{
			tool:  "run_commands",
			input: `{"commands":[{"command":"go test ./..."}]}`,
			want:  "go test ./...",
		},
		{
			tool:  "run_commands",
			input: `{"commands":[{"command":"go build"},{"command":"go test"}]}`,
			want:  "go build; go test",
		},
		{
			// Live cline 3.x shape: commands is a list of PLAIN STRINGS,
			// not objects (WP-T6 finding C1). Single command.
			tool:  "run_commands",
			input: `{"commands":["printf 'DONE' > /tmp/probe_out.txt"]}`,
			want:  "printf 'DONE' > /tmp/probe_out.txt",
		},
		{
			// Live cline 3.x shape, multiple plain-string commands —
			// the exact payload captured in /tmp/wpt6/cline-findings.md.
			// Every command is joined into the target (F4): a batch is a
			// sequence of independent commands, and only the head was
			// visible before.
			tool: "run_commands",
			input: `{"commands":["printf 'DONE' > /tmp/wpt6/cline/probe_out.txt",` +
				`"echo probe-marker-cline",` +
				`"printf '\n--- probe_out.txt ---\n'; cat /tmp/wpt6/cline/probe_out.txt"]}`,
			want: "printf 'DONE' > /tmp/wpt6/cline/probe_out.txt; echo probe-marker-cline; " +
				`printf '` + "\n" + `--- probe_out.txt ---` + "\n" + `'; cat /tmp/wpt6/cline/probe_out.txt`,
		},
		{
			// F4 REGRESSION: a destructive TAIL command must appear in
			// the target. Keeping only the head hid it from both the
			// target-based guard policy (internal/policy's engine calls
			// ParseCommand(ev.Target) and splits on ";") and from every
			// target-based analytics surface.
			tool:  "run_commands",
			input: `{"commands":["go build ./...","rm -rf /home/u/proj/.git"]}`,
			want:  "go build ./...; rm -rf /home/u/proj/.git",
		},
		{
			// Legacy OBJECT-list batches join identically — the two
			// wire shapes must not disagree about what a batch target
			// looks like.
			tool:  "run_commands",
			input: `{"commands":[{"command":"go build"},{"command":"rm -rf ~/x"}]}`,
			want:  "go build; rm -rf ~/x",
		},
		{
			// Empty commands list: no target, no crash.
			tool:  "run_commands",
			input: `{"commands":[]}`,
			want:  "",
		},
		{
			// Mixed types in one list (defensive: shouldn't happen in
			// practice, but the type-switch must not panic on an
			// unexpected element type). Extraction is PER ELEMENT, so
			// the string element still lands in the target and the one
			// element we could not read is reported honestly as
			// "(+1 more)" rather than silently dropped.
			tool:  "run_commands",
			input: `{"commands":[123,"echo hi"]}`,
			want:  "echo hi (+1 more)",
		},
		{
			// Nothing readable at all: the target says so rather than
			// pretending the batch was empty.
			tool:  "run_commands",
			input: `{"commands":[123,456]}`,
			want:  "(+2 more)",
		},
		{
			tool:  "editor",
			input: `{"path":"/proj/new.go","content":"package x"}`,
			want:  "/proj/new.go",
		},
		{
			tool:  "search_codebase",
			input: `{"pattern":"^func.*Error","target":"."}`,
			want:  "^func.*Error",
		},
		{
			tool:  "fetch_web_content",
			input: `{"url":"https://example.com/page"}`,
			want:  "https://example.com/page",
		},
		{
			tool:  "ask_question",
			input: `{"question":"Should I delete this?"}`,
			want:  "Should I delete this?",
		},
		{
			tool:  "spawn_agent",
			input: `{"task":"Refactor the parser"}`,
			want:  "Refactor the parser",
		},
		{
			tool:  "team_spawn_teammate",
			input: `{"name":"reviewer","agent_id":"agt_rev_001"}`,
			want:  "reviewer",
		},
		{
			tool:  "team_send_message",
			input: `{"to":"reviewer","message":"Please check"}`,
			want:  "reviewer",
		},
		{
			tool:  "unknown_tool",
			input: `{"anything":"goes"}`,
			want:  "",
		},
		{
			tool:  "read_files",
			input: `not json`,
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.tool+"/"+tc.input, func(t *testing.T) {
			t.Parallel()
			got := extractTarget(tc.tool, json.RawMessage(tc.input), sc)
			if got != tc.want {
				t.Errorf("extractTarget(%q, %s) = %q; want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

// TestJoinBatchCommands_CapsAtByteBudget pins the one bound on the
// joined multi-command target: a pathological batch cannot put an
// unbounded string in actions.target (the store does not truncate it),
// and whatever the cap drops is reported, never silently lost.
func TestJoinBatchCommands_CapsAtByteBudget(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 900)
	got := joinBatchCommands([]string{big, big, big, big})
	if len(got) > maxBatchCommandTargetBytes+len(" (+9 more)") {
		t.Errorf("len(got) = %d; want <= cap %d plus the suffix", len(got), maxBatchCommandTargetBytes)
	}
	if !strings.HasSuffix(got, "(+2 more)") {
		t.Errorf("got = %q; want the dropped-tail count reported as (+2 more)", got)
	}
	// A single oversized command is NOT capped: single-command batches
	// never reach this helper (extractTarget returns them verbatim), and
	// the first element is always rendered so the target is never empty.
	if first := joinBatchCommands([]string{big + big + big, "echo hi"}); !strings.HasPrefix(first, "xxx") {
		t.Errorf("oversized head dropped: %q", first[:min(40, len(first))])
	}
}

// TestSessionEnd_EmittedForTerminalStatus pins the dispatch for
// terminal-status sessions. status="idle" must NOT emit session_end
// (intermediate state); status="completed" / "failed" / etc. must.
func TestSessionEnd_EmittedForTerminalStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status   string
		wantEnd  bool
		wantOK   bool // session_end.Success
		wantText string
	}{
		{status: "idle", wantEnd: false},
		{status: "running", wantEnd: false},
		{status: "completed", wantEnd: true, wantOK: true, wantText: "completed"},
		{status: "failed", wantEnd: true, wantOK: false, wantText: "failed"},
		{status: "cancelled", wantEnd: true, wantOK: false, wantText: "cancelled"},
		{status: "stopped", wantEnd: true, wantOK: false, wantText: "stopped"},
		{status: "aborted", wantEnd: true, wantOK: false, wantText: "aborted"},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			s := sessionRow{
				ID:        "sid-" + tc.status,
				Source:    "cli",
				Status:    tc.status,
				StartedAt: "2026-06-05T23:21:51.000Z",
				UpdatedAt: "2026-06-05T23:28:30.000Z",
				CWD:       "/home/dev/proj",
				Model:     "deepseek/deepseek-v4-flash",
			}
			tool, _, _ := buildEvents(context.Background(), []sessionRow{s}, "/x/sessions.db", scrub.New(), nil)
			var foundEnd bool
			var endEvt models.ToolEvent
			for _, ev := range tool {
				if ev.ActionType == models.ActionSessionEnd {
					foundEnd = true
					endEvt = ev
				}
			}
			if foundEnd != tc.wantEnd {
				t.Errorf("session_end emitted = %v; want %v", foundEnd, tc.wantEnd)
			}
			if tc.wantEnd {
				if endEvt.Success != tc.wantOK {
					t.Errorf("session_end.Success = %v; want %v", endEvt.Success, tc.wantOK)
				}
				if endEvt.Target != tc.wantText {
					t.Errorf("session_end.Target = %q; want %q", endEvt.Target, tc.wantText)
				}
			}
		})
	}
}

// TestPerMessageTokenEvent_UsesModelInfoOverride confirms the Phase 0
// reality-check find: per-message modelInfo (when present) wins over
// the session-level model. Defends against the dashboard mis-attributing
// usage to the session's start-time model after a mid-session
// provider switch.
func TestPerMessageTokenEvent_UsesModelInfoOverride(t *testing.T) {
	t.Parallel()
	s := sessionRow{
		ID:        "sess-x",
		Model:     "session-level-model",
		StartedAt: "2026-06-05T23:21:51.000Z",
		UpdatedAt: "2026-06-05T23:21:55.000Z",
		// Synthetic MessagesPath so resolveMessagesPath returns
		// non-empty without needing the file on disk. The walker
		// doesn't open the file when Messages.Messages is already
		// populated in-memory.
		MessagesPath: sql.NullString{String: "/synthetic/path/sid.messages.json", Valid: true},
	}
	m := messageRecord{
		ID:        "asst-1",
		Role:      "assistant",
		Ts:        1780701727101,
		ModelInfo: &messageModelInfo{ID: "switched-mid-session-model", Provider: "openrouter"},
		Metrics:   &messageUsageBlock{InputTokens: 100, OutputTokens: 50, Cost: 0.01},
	}
	s.Messages.Messages = []messageRecord{m}
	_, tokens, _ := buildEvents(context.Background(), []sessionRow{s}, "/x/sessions.db", scrub.New(), nil)
	if len(tokens) != 1 {
		t.Fatalf("token events = %d; want 1 (per-message only — session usage is zero)", len(tokens))
	}
	if tokens[0].Model != "switched-mid-session-model" {
		t.Errorf("Model = %q; want %q", tokens[0].Model, "switched-mid-session-model")
	}
}

// TestActionMetadataFor pins the sub-agent + team linkage builder.
// A lead session with no team returns nil (no metadata to record);
// every other shape returns a populated *ActionMetadata.
func TestActionMetadataFor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		row       sessionRow
		wantNil   bool
		wantField func(*models.ActionMetadata) string
		wantValue string
	}{
		{
			name:    "lead_session_no_team",
			row:     sessionRow{ID: "lead-1"},
			wantNil: true,
		},
		{
			name: "lead_session_with_team",
			row: sessionRow{
				ID:       "lead-1",
				TeamName: sql.NullString{Valid: true, String: "team-X"},
			},
			wantField: func(m *models.ActionMetadata) string { return m.TeamName },
			wantValue: "team-X",
		},
		{
			name: "subagent_with_parent_linkage",
			row: sessionRow{
				ID:              "child-1",
				IsSubagent:      1,
				ParentSessionID: sql.NullString{Valid: true, String: "parent-sid"},
				ParentAgentID:   sql.NullString{Valid: true, String: "parent-agt"},
				AgentID:         sql.NullString{Valid: true, String: "child-agt"},
			},
			wantField: func(m *models.ActionMetadata) string { return m.ParentSessionID },
			wantValue: "parent-sid",
		},
		{
			name: "is_subagent_only_no_parent_columns",
			row: sessionRow{
				ID:         "child-2",
				IsSubagent: 1,
			},
			wantField: func(m *models.ActionMetadata) string {
				if m.IsSubagent {
					return "true"
				}
				return "false"
			},
			wantValue: "true",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := actionMetadataFor(&tc.row)
			if tc.wantNil {
				if got != nil {
					t.Errorf("actionMetadataFor() = %+v; want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("actionMetadataFor() = nil; want populated")
			}
			if tc.wantField != nil {
				v := tc.wantField(got)
				if v != tc.wantValue {
					t.Errorf("metadata field = %q; want %q", v, tc.wantValue)
				}
			}
		})
	}
}

// TestBuildEvents_AttachesMetadataOnEveryRow confirms the Metadata
// pointer flows from sessionRow → every emitted ToolEvent (when the
// session has any sub-agent / team linkage to surface). Defends
// against a regression where a new event emitter forgets to plumb
// the metadata pointer.
func TestBuildEvents_AttachesMetadataOnEveryRow(t *testing.T) {
	t.Parallel()
	s := sessionRow{
		ID:              "child-x",
		Source:          "subagent",
		Status:          "completed",
		Model:           "deepseek/deepseek-v4-flash",
		CWD:             "/home/dev/proj",
		StartedAt:       "2026-06-05T23:21:51.000Z",
		UpdatedAt:       "2026-06-05T23:28:30.000Z",
		IsSubagent:      1,
		ParentSessionID: sql.NullString{Valid: true, String: "lead-y"},
		ParentAgentID:   sql.NullString{Valid: true, String: "agt_lead"},
		AgentID:         sql.NullString{Valid: true, String: "agt_child"},
		TeamName:        sql.NullString{Valid: true, String: "team-Z"},
		MessagesPath:    sql.NullString{Valid: true, String: "/synth/sid.messages.json"},
	}
	s.Messages.Messages = []messageRecord{
		{
			ID:   "u1",
			Role: "user",
			Ts:   1780701724628,
			Content: []messageBlock{
				{Type: "text", Text: `<user_input mode="act">Hi</user_input>`},
			},
		},
		{
			ID:        "a1",
			Role:      "assistant",
			Ts:        1780701727101,
			ModelInfo: &messageModelInfo{ID: "deepseek/deepseek-v4-flash", Provider: "cline"},
			Metrics:   &messageUsageBlock{InputTokens: 100, OutputTokens: 50},
			Content: []messageBlock{
				{Type: "text", Text: "Hi back."},
				{Type: "tool_use", ID: "call_1", Name: "read_files", Input: json.RawMessage(`{"files":[{"path":"/x"}]}`)},
			},
		},
	}
	tools, _, _ := buildEvents(context.Background(), []sessionRow{s}, "/x/sessions.db", scrub.New(), nil)
	if len(tools) == 0 {
		t.Fatal("no events emitted")
	}
	for i, ev := range tools {
		if ev.Metadata == nil {
			t.Errorf("event[%d] (action=%s) Metadata = nil; want populated", i, ev.ActionType)
			continue
		}
		if ev.Metadata.ParentSessionID != "lead-y" {
			t.Errorf("event[%d] parent_session_id = %q; want lead-y", i, ev.Metadata.ParentSessionID)
		}
		if !ev.Metadata.IsSubagent {
			t.Errorf("event[%d] is_subagent = false; want true", i)
		}
		if ev.Metadata.TeamName != "team-Z" {
			t.Errorf("event[%d] team_name = %q; want team-Z", i, ev.Metadata.TeamName)
		}
	}
}

// TestBuildEvents_SkipsHookCoveredSessions confirms the cross-path
// dedup gate. When the SessionHookChecker reports a session is
// already covered by hook-derived rows, the SQLite path skips its
// emit entirely — preventing the H1 hermes-audit double-count.
//
// The checker is invoked once per session; sessions it returns
// false for are emitted normally. An error from the checker
// surfaces as a warning and falls back to SQLite emission (safer
// to over-count than silently drop).
func TestBuildEvents_SkipsHookCoveredSessions(t *testing.T) {
	t.Parallel()
	const (
		hookedSID    = "covered-by-hook"
		uncoveredSID = "needs-sqlite"
	)
	hooked := sessionRow{
		ID:           hookedSID,
		Source:       "cli",
		Status:       "completed",
		StartedAt:    "2026-06-05T23:21:51.000Z",
		UpdatedAt:    "2026-06-05T23:28:30.000Z",
		Model:        "deepseek/deepseek-v4-flash",
		CWD:          "/home/dev/proj",
		MessagesPath: sql.NullString{Valid: true, String: "/synthetic/path/hooked.messages.json"},
	}
	hooked.Messages.Messages = []messageRecord{
		{
			ID:   "u1",
			Role: "user",
			Content: []messageBlock{
				{Type: "text", Text: "Hi"},
			},
		},
	}
	uncovered := sessionRow{
		ID:           uncoveredSID,
		Source:       "cli",
		Status:       "completed",
		StartedAt:    "2026-06-05T23:21:51.000Z",
		UpdatedAt:    "2026-06-05T23:28:30.000Z",
		Model:        "deepseek/deepseek-v4-flash",
		CWD:          "/home/dev/proj",
		MessagesPath: sql.NullString{Valid: true, String: "/synthetic/path/uncovered.messages.json"},
	}
	uncovered.Messages.Messages = []messageRecord{
		{
			ID:   "u2",
			Role: "user",
			Content: []messageBlock{
				{Type: "text", Text: "Other"},
			},
		},
	}

	check := SessionHookChecker(func(_ context.Context, sid string) (bool, error) {
		return sid == hookedSID, nil
	})
	tools, _, warnings := buildEvents(
		context.Background(),
		[]sessionRow{hooked, uncovered},
		"/x/sessions.db",
		scrub.New(),
		check,
	)
	if len(warnings) > 0 {
		t.Logf("warnings: %v", warnings)
	}

	// No row should reference the hooked session.
	for _, ev := range tools {
		if ev.SessionID == hookedSID {
			t.Errorf("emitted event for hook-covered session %q: %s/%s", ev.SessionID, ev.ActionType, ev.RawToolName)
		}
	}
	// The uncovered session should produce at least a session_start
	// + a session_end + the user_prompt row.
	var foundUncovered bool
	for _, ev := range tools {
		if ev.SessionID == uncoveredSID {
			foundUncovered = true
			break
		}
	}
	if !foundUncovered {
		t.Error("no events emitted for uncovered session — dedup gate over-applied")
	}
}

// TestBuildEvents_HookCheckerErrorFallsBack confirms the warn-and-
// emit fallback when the checker returns an error.
func TestBuildEvents_HookCheckerErrorFallsBack(t *testing.T) {
	t.Parallel()
	s := sessionRow{
		ID:        "sess",
		Source:    "cli",
		Status:    "completed",
		StartedAt: "2026-06-05T23:21:51.000Z",
		UpdatedAt: "2026-06-05T23:28:30.000Z",
		Model:     "deepseek/deepseek-v4-flash",
		CWD:       "/home/dev/proj",
	}
	check := SessionHookChecker(func(_ context.Context, _ string) (bool, error) {
		return false, fmt.Errorf("DB connection lost")
	})
	tools, _, warnings := buildEvents(context.Background(), []sessionRow{s}, "/x/sessions.db", scrub.New(), check)
	if len(tools) == 0 {
		t.Error("no events emitted; expected fallback emission on checker error")
	}
	if len(warnings) == 0 {
		t.Error("no warnings; expected one for the checker error")
	}
}

// TestBuildEvents_SessionAggregateGate pins the session-aggregate
// TokenEvent's emission contract (the 2026-06-11 operator-reported
// double-count fix): the aggregate is a FALLBACK for terminal
// sessions whose messages.json carries no per-message metrics
// blocks. When metrics exist, the per-message rows are the session's
// usage and the aggregate must not coexist with them — the two
// shapes differ on both halves of the UNIQUE(source_file,
// source_event_id) key, so neither the store's MAX-upgrade conflict
// path nor the cost engine's proxy↔jsonl dedup would ever merge
// them, and every downstream SUM reads 2×.
func TestBuildEvents_SessionAggregateGate(t *testing.T) {
	t.Parallel()

	mkSession := func(id, status string, withMetrics bool) sessionRow {
		s := sessionRow{
			ID:           id,
			Source:       "cli",
			Status:       status,
			StartedAt:    "2026-06-11T13:27:08.000Z",
			UpdatedAt:    "2026-06-11T13:27:12.000Z",
			Model:        "minimax/minimax-m3",
			CWD:          "/home/dev/proj",
			MessagesPath: sql.NullString{Valid: true, String: "/synthetic/" + id + ".messages.json"},
		}
		s.Metadata.Usage = metadataUsage{
			InputTokens:     4802,
			OutputTokens:    1,
			CacheReadTokens: 114,
			TotalCost:       0.00141444,
		}
		msg := messageRecord{
			ID:   "m1",
			Role: "assistant",
			Ts:   1781184432540,
			Content: []messageBlock{
				{Type: "text", Text: "ping"},
			},
		}
		if withMetrics {
			msg.Metrics = &messageUsageBlock{
				InputTokens:     4802,
				OutputTokens:    1,
				CacheReadTokens: 114,
				Cost:            0.00141444,
			}
		}
		s.Messages.Messages = []messageRecord{msg}
		return s
	}

	cases := []struct {
		name          string
		status        string
		withMetrics   bool
		wantAggregate bool
		wantPerMsg    bool
	}{
		// Current cline builds: per-message metrics present → the
		// aggregate must be suppressed regardless of status.
		{"terminal+metrics", "completed", true, false, true},
		{"running+metrics", "running", true, false, true},
		// Pre-metrics builds: aggregate is the only token signal,
		// but only once the session settles (terminal status) — a
		// mid-session scan must not strand a permanent aggregate
		// that later per-message rows would double.
		{"terminal+no-metrics", "completed", false, true, false},
		{"running+no-metrics", "running", false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mkSession("sess-"+tc.name, tc.status, tc.withMetrics)
			_, tokens, warnings := buildEvents(
				context.Background(),
				[]sessionRow{s},
				"/x/sessions.db",
				scrub.New(),
				nil,
			)
			if len(warnings) > 0 {
				t.Logf("warnings: %v", warnings)
			}
			var gotAggregate, gotPerMsg bool
			for _, te := range tokens {
				if strings.HasSuffix(te.SourceEventID, ":session_usage") {
					gotAggregate = true
				}
				if strings.HasSuffix(te.SourceEventID, ":metrics") {
					gotPerMsg = true
				}
			}
			if gotAggregate != tc.wantAggregate {
				t.Errorf("aggregate emitted = %v; want %v", gotAggregate, tc.wantAggregate)
			}
			if gotPerMsg != tc.wantPerMsg {
				t.Errorf("per-message emitted = %v; want %v", gotPerMsg, tc.wantPerMsg)
			}
			if gotAggregate && gotPerMsg {
				t.Error("aggregate and per-message rows coexist — the double-count shape")
			}
		})
	}
}

// TestWithSessionHookChecker_Chains pins the setter's chainable
// return value — useful for the typical
// `clinecli.New().WithSessionHookChecker(check)` install pattern.
func TestWithSessionHookChecker_Chains(t *testing.T) {
	t.Parallel()
	a := New().WithSessionHookChecker(func(_ context.Context, _ string) (bool, error) {
		return false, nil
	})
	if a == nil {
		t.Fatal("WithSessionHookChecker returned nil; want chainable receiver")
	}
	if a.hookCheck == nil {
		t.Error("hookCheck not set on adapter after WithSessionHookChecker")
	}
}

// TestNormalizeProjectRoot_CrossMount mirrors the hermes H2 fix:
// Windows-side cwd values must route through crossmount.TranslateForeignPath
// so a WSL2 observer reading a Windows-native Cline CLI install lands
// on /mnt/c/… rather than the literal C:\… (which would CWD-prefix
// onto observer's own .git).
func TestNormalizeProjectRoot_CrossMount(t *testing.T) {
	t.Parallel()
	// Empty stays empty.
	if got := normalizeProjectRoot(""); got != "" {
		t.Errorf("normalizeProjectRoot(\"\") = %q; want \"\"", got)
	}
	// Linux-native paths pass through unchanged.
	if got := normalizeProjectRoot("/home/dev/proj"); got != "/home/dev/proj" {
		t.Errorf("normalizeProjectRoot linux-native = %q; want unchanged", got)
	}
	// Windows-shape paths get a non-empty result (the exact value
	// is environment-dependent — TranslateForeignPath returns
	// /mnt/c/Users/... on WSL2 and the input verbatim on Windows
	// native).
	if got := normalizeProjectRoot(`C:\Users\dev\proj`); got == "" {
		t.Error("normalizeProjectRoot dropped a non-empty Windows path")
	}
}

// TestBuildEvents_ReasoningNeverMintsAnAction is the B3 regression pin.
// A `thinking` content block must produce NO action row (it used to mint
// a phantom `clinecli.reasoning` task_complete row) and its body must
// ride the next event's PrecedingReasoning under the consumed-once /
// last-wins / turn-boundary-discard contract.
func TestBuildEvents_ReasoningNeverMintsAnAction(t *testing.T) {
	t.Parallel()
	s := sessionRow{
		ID:           "b3-sid",
		Source:       "cli",
		Status:       "idle",
		Model:        "deepseek/deepseek-v4-flash",
		CWD:          "/home/dev/proj",
		StartedAt:    "2026-06-05T23:21:51.000Z",
		UpdatedAt:    "2026-06-05T23:28:30.000Z",
		MessagesPath: sql.NullString{Valid: true, String: "/synth/b3.messages.json"},
	}
	s.Messages.Messages = []messageRecord{
		{
			ID: "u1", Role: "user", Ts: 1780701724628,
			Content: []messageBlock{{Type: "text", Text: `<user_input mode="act">do the thing</user_input>`}},
		},
		{
			ID: "a1", Role: "assistant", Ts: 1780701727101,
			Content: []messageBlock{
				{Type: "thinking", Thinking: "THINK_ONE"},
				{Type: "tool_use", ID: "call_1", Name: "read_files", Input: json.RawMessage(`{"files":[{"path":"/a"}]}`)},
				{Type: "tool_use", ID: "call_2", Name: "read_files", Input: json.RawMessage(`{"files":[{"path":"/b"}]}`)},
			},
		},
		{
			ID: "a2", Role: "assistant", Ts: 1780701728101,
			Content: []messageBlock{{Type: "thinking", Thinking: "STALE_ACROSS_TURN"}},
		},
		{
			ID: "u2", Role: "user", Ts: 1780701729101,
			Content: []messageBlock{{Type: "text", Text: `<user_input mode="act">next thing</user_input>`}},
		},
		{
			ID: "a3", Role: "assistant", Ts: 1780701730101,
			Content: []messageBlock{
				{Type: "tool_use", ID: "call_3", Name: "read_files", Input: json.RawMessage(`{"files":[{"path":"/c"}]}`)},
			},
		},
		{
			ID: "a4", Role: "assistant", Ts: 1780701731101,
			Content: []messageBlock{
				{Type: "thinking", Thinking: "FIRST"},
				{Type: "thinking", Thinking: "SECOND"},
				{Type: "tool_use", ID: "call_4", Name: "read_files", Input: json.RawMessage(`{"files":[{"path":"/d"}]}`)},
			},
		},
	}

	tools, _, _ := buildEvents(context.Background(), []sessionRow{s}, "/x/sessions.db", scrub.New(), nil)

	byCall := map[string]models.ToolEvent{}
	for _, ev := range tools {
		if strings.Contains(strings.ToLower(ev.RawToolName), "reasoning") ||
			strings.Contains(strings.ToLower(ev.RawToolName), "thinking") {
			t.Fatalf("reasoning-named action row emitted: raw=%q %+v", ev.RawToolName, ev)
		}
		for _, body := range []string{"THINK_ONE", "STALE_ACROSS_TURN", "FIRST", "SECOND"} {
			if ev.Target == body || ev.ToolOutput == body {
				t.Fatalf("reasoning body %q surfaced as action content: %+v", body, ev)
			}
		}
		if ev.RawToolName == "read_files" {
			byCall[ev.SourceEventID] = ev
		}
	}

	cases := []struct{ id, want, why string }{
		{"ma1:tool_use:call_1", "THINK_ONE", "the thinking block that introduced it"},
		{"ma1:tool_use:call_2", "", "consumed-once: the sibling call already took it"},
		{"ma3:tool_use:call_3", "", "turn-boundary discard: a user prompt intervened"},
		{"ma4:tool_use:call_4", "SECOND", "last-wins: the newer thinking block replaces the older"},
	}
	for _, c := range cases {
		ev, ok := byCall[c.id]
		if !ok {
			t.Fatalf("missing tool_use event %q", c.id)
		}
		if ev.PrecedingReasoning != c.want {
			t.Errorf("%s PrecedingReasoning = %q, want %q (%s)", c.id, ev.PrecedingReasoning, c.want, c.why)
		}
	}
}

// TestParseSessionFile_FixtureCorpus_ReasoningThreaded is the same B3
// pin against the LIVE captured corpus, whose assistant messages carry
// thinking → text → tool_use in that block order: the assistant-text row
// is the first successor, so it (not the tool_use row) carries the
// chain-of-thought.
func TestParseSessionFile_FixtureCorpus_ReasoningThreaded(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureCLineDataDir(t)
	a := New()
	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var threaded int
	for _, ev := range res.ToolEvents {
		if strings.Contains(strings.ToLower(ev.RawToolName), "reasoning") {
			t.Fatalf("reasoning-named action row emitted: raw=%q %+v", ev.RawToolName, ev)
		}
		if ev.RawToolName == "clinecli.assistant_text" &&
			strings.Contains(ev.PrecedingReasoning, "session protocol in the AGENTS.md") {
			threaded++
		}
	}
	if threaded == 0 {
		t.Errorf("no assistant_text row carried the fixture's thinking block in PrecedingReasoning")
	}
}
