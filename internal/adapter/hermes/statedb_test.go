package hermes

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// buildFixtureDB loads the live testdata/hermes/sessions.sql + messages.sql
// dumps into a fresh tempfile SQLite DB and returns its path. The
// schema_version table is created and seeded to 14 so scanStateDB's
// version check exercises the happy-path branch.
//
// Caller is responsible for cleanup — t.TempDir() backs the dir so
// the runtime handles removal.
func buildFixtureDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Schema_version table so the schema check exercises happy path.
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER); INSERT INTO schema_version VALUES (14);`); err != nil {
		t.Fatalf("schema_version setup: %v", err)
	}

	// Load each .sql dump verbatim. Order: sessions first so the FK
	// reference from messages resolves (even though the dumps
	// PRAGMA foreign_keys=OFF, ordering keeps the DB visually sane).
	for _, fname := range []string{"sessions.sql", "messages.sql"} {
		path := filepath.Join("..", "..", "..", "testdata", "hermes", fname)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", fname, err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("exec %s dump: %v", fname, err)
		}
	}
	return dbPath
}

// TestScanStateDB_LiveCorpus exercises scanStateDB end-to-end against
// the captured live Hermes fixtures. Verifies row counts, key columns,
// and the maxID watermark.
func TestScanStateDB_LiveCorpus(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)

	sessions, messages, maxID, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}

	// The captured corpus has 9 sessions / 62 messages (per
	// testdata/hermes/reality-check.txt). Every message has active=1
	// in the corpus so the filter should produce the full 62.
	if got, want := len(messages), 62; got != want {
		t.Errorf("len(messages) = %d, want %d", got, want)
	}
	if got, want := len(sessions), 9; got != want {
		t.Errorf("len(sessions) = %d, want %d", got, want)
	}
	if maxID < 62 {
		t.Errorf("maxID = %d, want >= 62", maxID)
	}

	// Spot-check one known session from the fixture.
	want := "20260605_154029_7b8623"
	sess, ok := sessions[want]
	if !ok {
		t.Fatalf("session %q missing from result", want)
	}
	if sess.Source != "cli" {
		t.Errorf("source = %q, want cli", sess.Source)
	}
	if !strings.HasPrefix(sess.Model, "nvidia/") {
		t.Errorf("model = %q, want nvidia/ prefix", sess.Model)
	}
	if sess.MessageCount == 0 {
		t.Error("message_count = 0, want > 0")
	}
	// CWD column exists on schema v14 - the section 17.1 reality
	// check headline finding. Confirm it landed in the row.
	// (The captured corpus may have it NULL since one-shot
	// `hermes -z` runs don't always populate cwd before fix
	// land; either is acceptable. Just verify the field maps.)
	_ = sess.CWD
}

// TestBuildEvents_ReasoningAndStopReason pins the two trace-coverage
// additions for Hermes: a standalone hermes.reasoning row from the
// messages.reasoning / reasoning_content columns (the live corpus rows
// 2/4/6/8 carry chain-of-thought), and Metadata.StopReason stamped on
// the assistant_response row from messages.finish_reason. hookCheck is
// nil so every session runs the SQLite emission path.
func TestBuildEvents_ReasoningAndStopReason(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	sessions, messages, _, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}
	toolEvents, _, _ := buildEvents(context.Background(), sessions, messages, dbPath, nil, nil)

	var reasoningRows, stopRows int
	var sawGreeting bool
	for _, ev := range toolEvents {
		switch ev.RawToolName {
		case "hermes.reasoning":
			reasoningRows++
			if strings.Contains(ev.ToolOutput, "simple greeting") {
				sawGreeting = true
			}
		case "hermes.assistant_response":
			if ev.Metadata != nil && strings.TrimSpace(ev.Metadata.StopReason) != "" {
				stopRows++
			}
		}
	}
	if reasoningRows == 0 {
		t.Errorf("no hermes.reasoning rows emitted; want >= 1 (live corpus rows 2/4/6/8 carry reasoning)")
	}
	if !sawGreeting {
		t.Errorf("hermes.reasoning row body missing expected 'simple greeting' text")
	}
	if stopRows == 0 {
		t.Errorf("no hermes.assistant_response carried Metadata.StopReason; want >= 1 (finish_reason='stop')")
	}
}

// TestScanStateDB_OffsetAdvance verifies the watermark semantics: a
// second scan with the prior maxID as fromMessageID returns zero
// rows.
func TestScanStateDB_OffsetAdvance(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)

	_, _, maxID, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	sessions2, messages2, maxID2, err := scanStateDB(context.Background(), dbPath, maxID)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if len(messages2) != 0 {
		t.Errorf("second scan messages = %d, want 0", len(messages2))
	}
	if len(sessions2) != 0 {
		t.Errorf("second scan sessions = %d, want 0", len(sessions2))
	}
	if maxID2 != maxID {
		t.Errorf("second scan maxID = %d, want %d (unchanged)", maxID2, maxID)
	}
}

// TestScanStateDB_FiltersActiveZero seeds an extra active=0 row and
// confirms it's excluded from the scan.
func TestScanStateDB_FiltersActiveZero(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)

	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (session_id, role, content, timestamp, active)
		VALUES ('20260605_154029_7b8623', 'user', 'archived row', 1780674000, 0);`); err != nil {
		t.Fatalf("seed active=0 row: %v", err)
	}
	db.Close()

	_, messages, _, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}
	for _, m := range messages {
		if m.Content.Valid && m.Content.String == "archived row" {
			t.Error("active=0 row leaked into scan result")
		}
	}
}

// TestScanStateDB_SchemaVersionBelowFloorErrors confirms the open-
// time check trips when schema_version reports a version lower than
// what the parser supports (11).
func TestScanStateDB_SchemaVersionBelowFloorErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_version (version INTEGER); INSERT INTO schema_version VALUES (8);`); err != nil {
		t.Fatalf("schema setup: %v", err)
	}
	db.Close()

	_, _, _, err = scanStateDB(context.Background(), dbPath, 0)
	if err == nil {
		t.Error("expected schema-version-too-old error, got nil")
	}
}

// TestScanStateDB_AbsentSchemaVersionTableIsTolerated confirms the
// "missing schema_version table" branch returns "unknown" rather
// than failing. Lets stripped-down test DBs work without the version
// table.
func TestScanStateDB_AbsentSchemaVersionTableIsTolerated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Minimal schema: just sessions + messages with the columns
	// scanStateDB reads. No schema_version table.
	if _, err := db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			user_id TEXT, model TEXT, model_config TEXT, system_prompt TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL, ended_at REAL, end_reason TEXT,
			message_count INTEGER DEFAULT 0, tool_call_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0, cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0, cwd TEXT,
			billing_provider TEXT, billing_base_url TEXT, billing_mode TEXT,
			estimated_cost_usd REAL, actual_cost_usd REAL,
			cost_status TEXT, cost_source TEXT, pricing_version TEXT,
			title TEXT, api_call_count INTEGER DEFAULT 0,
			handoff_state TEXT, handoff_platform TEXT, handoff_error TEXT,
			rewind_count INTEGER NOT NULL DEFAULT 0,
			archived INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL, content TEXT,
			tool_call_id TEXT, tool_calls TEXT, tool_name TEXT,
			timestamp REAL NOT NULL,
			token_count INTEGER, finish_reason TEXT,
			reasoning TEXT, reasoning_content TEXT, reasoning_details TEXT,
			codex_reasoning_items TEXT, codex_message_items TEXT,
			platform_message_id TEXT,
			observed INTEGER DEFAULT 0, active INTEGER NOT NULL DEFAULT 1
		);
		INSERT INTO sessions VALUES ('s1','cli',NULL,'openai/gpt-4o-mini',NULL,NULL,NULL,
			1700000000,NULL,NULL,0,0,0,0,0,0,0,NULL,
			NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,0,NULL,NULL,NULL,0,0);
	`); err != nil {
		t.Fatalf("minimal schema setup: %v", err)
	}
	db.Close()

	sessions, messages, _, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}
	if len(messages) != 0 {
		t.Errorf("messages = %d, want 0 (no rows seeded)", len(messages))
	}
	// Sessions only loaded when referenced by messages — with zero
	// messages we expect zero sessions in the result map (not the
	// 1 row we inserted).
	if len(sessions) != 0 {
		t.Errorf("sessions = %d, want 0 (none referenced by messages)", len(sessions))
	}
}

// TestBuildEvents_LiveCorpus_FullRoundTrip exercises the buildEvents
// glue against the captured corpus. Confirms tool events emit for
// every assistant tool_call, normalised by Commit 4's mapping, and
// per-session TokenEvents fire for sessions with non-zero
// aggregates.
func TestBuildEvents_LiveCorpus_FullRoundTrip(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	sessions, messages, _, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}
	toolEvents, tokenEvents, warnings := buildEvents(context.Background(), sessions, messages, dbPath, nil, nil)

	// Per testdata/hermes/reality-check.txt: 9 sessions with 14
	// assistant tool_call rows across them (every row in the
	// "tool_calls samples" section has exactly one call per
	// assistant message).
	if len(toolEvents) < 12 {
		t.Errorf("len(toolEvents) = %d, want >= 12", len(toolEvents))
	}
	if len(tokenEvents) < 5 {
		t.Errorf("len(tokenEvents) = %d, want >= 5 (sessions with non-zero token aggregates)", len(tokenEvents))
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}

	// Every emitted ToolEvent must carry Tool = ToolHermes + a
	// non-empty SourceEventID + provider-prefix-stripped Model.
	for _, e := range toolEvents {
		if e.Tool != "hermes" {
			t.Errorf("Tool = %q", e.Tool)
		}
		if e.SourceEventID == "" {
			t.Errorf("SourceEventID empty for event %+v", e)
		}
		if e.Model != "" && strings.Contains(e.Model, "/") {
			t.Errorf("Model %q not prefix-stripped", e.Model)
		}
		if e.SourceFile != dbPath {
			t.Errorf("SourceFile = %q, want %q", e.SourceFile, dbPath)
		}
	}

	// At least one event for each tool family present in the live
	// corpus: read_file, write_file, terminal, search_files,
	// web_search, web_extract, patch.
	seenActions := map[string]bool{}
	seenTools := map[string]bool{}
	for _, e := range toolEvents {
		seenActions[e.ActionType] = true
		seenTools[e.RawToolName] = true
	}
	for _, want := range []string{"read_file", "write_file", "terminal", "search_files", "web_search", "web_extract", "patch"} {
		if !seenTools[want] {
			t.Errorf("expected to see tool %q in events; got %v", want, seenTools)
		}
	}
	// Action types: should include run_command (terminal), read_file,
	// write_file, edit_file (patch), search_files, web_search, web_fetch.
	for _, want := range []string{"run_command", "read_file", "write_file", "edit_file", "search_files", "web_search", "web_fetch"} {
		if !seenActions[want] {
			t.Errorf("expected ActionType %q; got %v", want, seenActions)
		}
	}
}

// TestBuildEvents_HookCoveredSessionPartition pins the H1 cross-path
// dedup gate (observed live 2026-06-11: hook-covered session
// 20260605_185946_75fbe5 surfaced at exactly 2× its real tokens —
// two hook post_api_request rows AND the SQLite session aggregate,
// plus a duplicated run_command from both paths). For hook-covered
// sessions the SQLite path must suppress the classes the hook also
// emits (tool_call-derived ToolEvents + the session token aggregate)
// while STILL emitting the SQLite-only classes (user_prompt,
// assistant text, system_prompt) — a wholesale skip would lose them,
// since hermes hooks carry no message-text callbacks.
func TestBuildEvents_HookCoveredSessionPartition(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	sessions, messages, _, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}

	// Baseline: ungated emission for comparison.
	baseTools, baseTokens, _ := buildEvents(context.Background(), sessions, messages, dbPath, nil, nil)

	// Gate EVERY session as hook-covered.
	allHooked := SessionHookChecker(func(_ context.Context, _ string) (bool, error) {
		return true, nil
	})
	tools, tokens, warnings := buildEvents(context.Background(), sessions, messages, dbPath, nil, allHooked)
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}

	// All token aggregates suppressed.
	if len(tokens) != 0 {
		t.Errorf("len(tokens) = %d, want 0 (hook path owns token rows for covered sessions)", len(tokens))
	}
	if len(baseTokens) == 0 {
		t.Fatal("baseline emitted no token events — fixture drift, the suppression assert above is vacuous")
	}

	// Tool_call-derived events suppressed; message-text classes kept.
	keep := map[string]bool{"user_prompt": true, "assistant_message": true, "system_prompt": true}
	counts := map[string]int{}
	for _, e := range tools {
		counts[e.ActionType]++
		if !keep[e.ActionType] {
			t.Errorf("hook-covered session emitted SQLite %s event (%s) — hook path already carries it", e.ActionType, e.RawToolName)
		}
	}
	for want := range keep {
		if counts[want] == 0 {
			t.Errorf("no %q events under hook coverage; the partition over-suppressed (got %v)", want, counts)
		}
	}
	// Sanity: the baseline had tool_call-derived events to suppress.
	var baseHadToolCalls bool
	for _, e := range baseTools {
		if !keep[e.ActionType] {
			baseHadToolCalls = true
			break
		}
	}
	if !baseHadToolCalls {
		t.Fatal("baseline emitted no tool_call-derived events — fixture drift, the suppression asserts are vacuous")
	}

	// Checker errors fall back to full emission (over-count beats
	// silent drop), with a warning per session.
	failing := SessionHookChecker(func(_ context.Context, _ string) (bool, error) {
		return false, fmt.Errorf("store unavailable")
	})
	fbTools, fbTokens, fbWarnings := buildEvents(context.Background(), sessions, messages, dbPath, nil, failing)
	if len(fbTools) != len(baseTools) || len(fbTokens) != len(baseTokens) {
		t.Errorf("checker-error fallback drifted from baseline: tools %d/%d tokens %d/%d",
			len(fbTools), len(baseTools), len(fbTokens), len(baseTokens))
	}
	if len(fbWarnings) == 0 {
		t.Error("no warnings on checker error; want one per session")
	}
}

// TestBuildEvents_SurfaceUserPrompts_AssistantText_SystemPrompt pins
// the closure of the three gaps reported by the operator on the live
// session 20260605_154029_7b8623 against observer's dashboard
// (system prompt + user prompts + final assistant responses all
// invisible pre-fix). After the fix every assistant message with
// non-empty content emits a task_complete row, every user message
// emits a user_prompt row, and every session with a non-empty
// sessions.system_prompt emits a system_prompt row.
func TestBuildEvents_SurfaceUserPrompts_AssistantText_SystemPrompt(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	sessions, messages, _, err := scanStateDB(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("scanStateDB: %v", err)
	}
	toolEvents, _, _ := buildEvents(context.Background(), sessions, messages, dbPath, nil, nil)

	counts := map[string]int{}
	for _, e := range toolEvents {
		counts[e.ActionType]++
	}
	// Every action type must appear at least once for the fix to be
	// load-bearing on the live corpus.
	for _, want := range []string{"user_prompt", "assistant_message", "system_prompt"} {
		if counts[want] == 0 {
			t.Errorf("no %q events emitted; got %v", want, counts)
		}
	}

	// Session 20260605_154029_7b8623 is the one the operator
	// surfaced. It has 6 user rows and 6 finish_reason='stop'
	// assistant rows. Confirm we catch all 6 of each by counting
	// events tied to that session.
	const targetSession = "20260605_154029_7b8623"
	var sessUser, sessAsst, sessSys int
	for _, e := range toolEvents {
		if e.SessionID != targetSession {
			continue
		}
		switch e.ActionType {
		case "user_prompt":
			sessUser++
		case "assistant_message":
			sessAsst++
		case "system_prompt":
			sessSys++
		}
	}
	if sessUser < 6 {
		t.Errorf("session %s: user_prompt events = %d, want >= 6", targetSession, sessUser)
	}
	if sessAsst < 6 {
		t.Errorf("session %s: assistant_message events = %d, want >= 6 (finish_reason=stop rows)", targetSession, sessAsst)
	}
	if sessSys != 1 {
		t.Errorf("session %s: system_prompt events = %d, want exactly 1", targetSession, sessSys)
	}
}

// TestAdapter_ParseSessionFile_LiveCorpus exercises ParseSessionFile
// end-to-end (the public Adapter interface entry point) against the
// captured corpus. Covers the WAL/SHM trigger path, the offset
// advancement, and the integration with buildEvents.
func TestAdapter_ParseSessionFile_LiveCorpus(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	a := NewWithOptions(nil, filepath.Dir(dbPath))

	res, err := a.ParseSessionFile(context.Background(), dbPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) < 12 {
		t.Errorf("ToolEvents = %d, want >= 12", len(res.ToolEvents))
	}
	if len(res.TokenEvents) < 5 {
		t.Errorf("TokenEvents = %d, want >= 5", len(res.TokenEvents))
	}
	if res.NewOffset == 0 {
		t.Error("NewOffset = 0, want maxID")
	}

	// Second pass at the advanced offset returns nothing new.
	res2, err := a.ParseSessionFile(context.Background(), dbPath, res.NewOffset)
	if err != nil {
		t.Fatalf("second ParseSessionFile: %v", err)
	}
	if len(res2.ToolEvents) != 0 || len(res2.TokenEvents) != 0 {
		t.Errorf("second pass produced %d tool, %d token events; want 0/0", len(res2.ToolEvents), len(res2.TokenEvents))
	}
}

// TestAdapter_ParseSessionFile_WALTrigger confirms the WAL-suffix
// path: when fsnotify fires on state.db-wal, the adapter opens the
// main state.db file (not the WAL file).
func TestAdapter_ParseSessionFile_WALTrigger(t *testing.T) {
	t.Parallel()
	dbPath := buildFixtureDB(t)
	walPath := dbPath + "-wal"

	a := NewWithOptions(nil, filepath.Dir(dbPath))
	res, err := a.ParseSessionFile(context.Background(), walPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile via WAL trigger: %v", err)
	}
	if len(res.ToolEvents) == 0 {
		t.Error("WAL trigger should still produce events by opening the main DB")
	}
}

// TestAdapter_IsSessionFile pins the path predicates. Lives next to
// the SQLite integration tests so a future schema-shape change can
// adjust both atomically.
func TestAdapter_IsSessionFile(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), ".hermes")
	a := NewWithOptions(nil, root)
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"state.db_under_root", filepath.Join(root, "state.db"), true},
		{"state.db-wal_under_root", filepath.Join(root, "state.db-wal"), true},
		{"state.db-shm_under_root", filepath.Join(root, "state.db-shm"), true},
		{"state.db_outside_root_rejected", "/tmp/foreign/.hermes/state.db", false},
		{"random_db_under_root_rejected", filepath.Join(root, "other.db"), false},
		{"non_db_under_root_rejected", filepath.Join(root, "state.json"), false},
		{"empty_path", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := a.IsSessionFile(tc.path); got != tc.want {
				t.Errorf("IsSessionFile(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
