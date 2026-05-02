package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/freshness"
	"github.com/marmutapp/superbased-observer/internal/models"
)

func newTestStore(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database), database
}

func TestUpsertProjectIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	id1, err := s.UpsertProject(ctx, "/tmp/p1", "git@example.com:x.git")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.UpsertProject(ctx, "/tmp/p1", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("project id changed: %d -> %d", id1, id2)
	}
}

// TestUpsertProject_NormalizesGitInternalPaths guards Round 3 issue #12:
// the live DB had accumulated a project row at
// <repo>/.git/worktrees because a session's cwd resolved
// into the worktree manager directory. Fold any "/.git/<...>" or
// "/.git" suffix back to the working-tree root so the table doesn't get
// polluted with admin paths that aren't actually projects.
func TestUpsertProject_NormalizesGitInternalPaths(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	cases := []struct {
		input string
		want  string
	}{
		{"/home/me/repo/.git/worktrees", "/home/me/repo"},
		{"/home/me/repo/.git/worktrees/feature-x", "/home/me/repo"},
		{"/home/me/repo/.git", "/home/me/repo"},
		{"/home/me/repo", "/home/me/repo"},                 // no-op
		{"/home/me/.git-stash/x", "/home/me/.git-stash/x"}, // not a real .git path
	}
	seen := map[string]int64{}
	for _, c := range cases {
		id, err := s.UpsertProject(ctx, c.input, "")
		if err != nil {
			t.Fatalf("UpsertProject(%q): %v", c.input, err)
		}
		if prev, ok := seen[c.want]; ok && prev != id {
			t.Errorf("input %q normalized to %q but got id %d, expected matching prior id %d",
				c.input, c.want, id, prev)
		}
		seen[c.want] = id
	}
}

func TestUpsertSessionMergesFields(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	pid, _ := s.UpsertProject(ctx, "/tmp/p2", "")
	start := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	if err := s.UpsertSession(ctx, models.Session{
		ID: "s1", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: start,
	}); err != nil {
		t.Fatal(err)
	}
	// Second call provides model + ended_at.
	end := start.Add(10 * time.Minute)
	if err := s.UpsertSession(ctx, models.Session{
		ID: "s1", ProjectID: pid, Tool: models.ToolClaudeCode,
		Model: "claude-sonnet-4", StartedAt: start, EndedAt: end,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestParseCursors(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	off, err := s.GetCursor(ctx, "/x.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if off != 0 {
		t.Errorf("fresh cursor: %d", off)
	}
	if err := s.SetCursor(ctx, "/x.jsonl", 1234); err != nil {
		t.Fatal(err)
	}
	off, err = s.GetCursor(ctx, "/x.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if off != 1234 {
		t.Errorf("cursor: got %d want 1234", off)
	}
	// A lower value must not rewind.
	if err := s.SetCursor(ctx, "/x.jsonl", 500); err != nil {
		t.Fatal(err)
	}
	off, _ = s.GetCursor(ctx, "/x.jsonl")
	if off != 1234 {
		t.Errorf("cursor rewound: got %d want 1234", off)
	}
}

func TestListCursors(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	got, err := s.ListCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty DB: got %d cursors", len(got))
	}

	if err := s.SetCursor(ctx, "/a.jsonl", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCursor(ctx, "/b.jsonl", 250); err != nil {
		t.Fatal(err)
	}
	got, err = s.ListCursors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after two inserts: got %d cursors", len(got))
	}
	byPath := map[string]int64{}
	for _, c := range got {
		byPath[c.SourceFile] = c.ByteOffset
	}
	if byPath["/a.jsonl"] != 100 || byPath["/b.jsonl"] != 250 {
		t.Errorf("offsets mismatch: %v", byPath)
	}
}

func TestInsertActionsIdempotent(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	pid, _ := s.UpsertProject(ctx, "/tmp/p3", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "s1", ProjectID: pid, Tool: models.ToolClaudeCode,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	batch := []models.Action{
		{SessionID: "s1", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f.jsonl", SourceEventID: "e1"},
		{SessionID: "s1", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: models.ActionRunCommand, Target: "go test", Success: false,
			Tool: models.ToolClaudeCode, SourceFile: "f.jsonl", SourceEventID: "e2"},
	}
	n, err := s.InsertActions(ctx, batch)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("inserted %d want 2", n)
	}
	// Same batch again → idempotent at the table level (no new rows
	// land), but the v1.4.28 ON CONFLICT DO UPDATE for duration_ms
	// touches every conflict row, so SQLite's RowsAffected counts
	// each as 1. Assert against COUNT(*) instead — the contract that
	// matters is "no row duplication", not the specific counter.
	if _, err := s.InsertActions(ctx, batch); err != nil {
		t.Fatal(err)
	}
	total, _ := s.CountActions(ctx)
	if total != 2 {
		t.Errorf("total rows %d want 2", total)
	}
}

// TestInsertActions_DurationRefreshOnConflict pins the v1.4.28 fix:
// an action row first ingested with duration_ms=0 (pre-fix adapter)
// gets refreshed when re-ingested with a non-zero duration (post-fix
// adapter that derives DurationMs from the tool_use→tool_result or
// function_call→output timestamp gap). A non-zero existing value is
// NEVER clobbered, even by a smaller positive value.
func TestInsertActions_DurationRefreshOnConflict(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	pid, _ := s.UpsertProject(ctx, "/tmp/dur", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "s-dur", ProjectID: pid, Tool: models.ToolClaudeCode,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	pre := []models.Action{
		{SessionID: "s-dur", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e-zero",
			DurationMs: 0},
		{SessionID: "s-dur", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: models.ActionReadFile, Target: "b.go", Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e-set",
			DurationMs: 500},
	}
	if _, err := s.InsertActions(ctx, pre); err != nil {
		t.Fatal(err)
	}

	// Re-ingest: the zero-duration row gets a real value; the
	// already-set row must NOT be lowered.
	post := []models.Action{
		{SessionID: "s-dur", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: models.ActionReadFile, Target: "a.go", Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e-zero",
			DurationMs: 1234},
		{SessionID: "s-dur", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: models.ActionReadFile, Target: "b.go", Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e-set",
			DurationMs: 100}, // smaller than existing 500
	}
	if _, err := s.InsertActions(ctx, post); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		eid string
		want int64
	}{
		{"e-zero", 1234}, // refreshed from 0 → 1234
		{"e-set", 500},   // protected; would have become 100 if we clobbered
	} {
		var got int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(duration_ms, 0) FROM actions WHERE source_event_id = ?`, c.eid,
		).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: duration_ms = %d, want %d", c.eid, got, c.want)
		}
	}
}

func TestIngestWithFreshness(t *testing.T) {
	t.Parallel()
	s, d := newTestStore(t)
	ctx := context.Background()

	// Materialize a real file so the classifier can hash it.
	root := t.TempDir()
	p := filepath.Join(root, "a.go")
	if err := os.WriteFile(p, []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	classifier := freshness.New(d, freshness.Options{MaxHashSizeMB: 10, FastPathStatOnly: true})

	now := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)
	events := []models.ToolEvent{
		{
			SourceFile: "f.jsonl", SourceEventID: "fe1",
			SessionID: "sess-f1", ProjectRoot: root,
			Timestamp: now, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go",
			Success: true, RawToolName: "Read",
		},
		{
			SourceFile: "f.jsonl", SourceEventID: "fe2",
			SessionID: "sess-f1", ProjectRoot: root,
			Timestamp: now.Add(time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go",
			Success: true, RawToolName: "Read",
		},
	}

	native := func(n string) bool { return n == "Read" }
	if _, err := s.Ingest(ctx, events, nil, IngestOptions{
		IsNativeTool: native,
		Classifier:   classifier,
	}); err != nil {
		t.Fatal(err)
	}

	// First Read should be fresh, second should be stale (same session).
	var fresh1, fresh2 string
	err := d.QueryRowContext(ctx,
		`SELECT freshness FROM actions WHERE source_event_id = 'fe1'`).Scan(&fresh1)
	if err != nil {
		t.Fatal(err)
	}
	err = d.QueryRowContext(ctx,
		`SELECT freshness FROM actions WHERE source_event_id = 'fe2'`).Scan(&fresh2)
	if err != nil {
		t.Fatal(err)
	}
	if fresh1 != models.FreshnessFresh {
		t.Errorf("fe1 freshness: %q want fresh", fresh1)
	}
	if fresh2 != models.FreshnessStale {
		t.Errorf("fe2 freshness: %q want stale", fresh2)
	}

	// file_state should have exactly one row for this file.
	var n int
	_ = d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM file_state WHERE file_path = ?`, p).Scan(&n)
	if n != 1 {
		t.Errorf("file_state rows: %d want 1", n)
	}

	// content_hash should be set.
	var hash string
	_ = d.QueryRowContext(ctx,
		`SELECT content_hash FROM actions WHERE source_event_id = 'fe1'`).Scan(&hash)
	if hash == "" {
		t.Error("content_hash not populated for file action")
	}
}

func TestIngestRecordsFailureContextAndRetries(t *testing.T) {
	t.Parallel()
	s, d := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)
	mk := func(id, cmd string, ok bool, errMsg string, ts time.Time) models.ToolEvent {
		return models.ToolEvent{
			SourceFile: "cmds.jsonl", SourceEventID: id,
			SessionID: "sess-fc", ProjectRoot: "/tmp/pfc",
			Timestamp: ts, Tool: models.ToolClaudeCode,
			ActionType: models.ActionRunCommand, Target: cmd,
			Success: ok, ErrorMessage: errMsg, RawToolName: "Bash",
		}
	}
	events := []models.ToolEvent{
		mk("c1", "go test ./...", false, "--- FAIL: TestFoo", now),
		mk("c2", "go  test   ./...", false, "--- FAIL: TestFoo", now.Add(time.Second)), // same hash, whitespace collapsed
		mk("c3", "go test ./...", true, "", now.Add(2*time.Second)),
		mk("c4", "ls /no-such", false, "ls: cannot access '/no-such': No such file or directory", now.Add(3*time.Second)),
	}

	if _, err := s.Ingest(ctx, events, nil, IngestOptions{RecordFailures: true}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	// 3 failure rows total: c1, c2, c4.
	var total int
	_ = d.QueryRowContext(ctx, `SELECT COUNT(*) FROM failure_context`).Scan(&total)
	if total != 3 {
		t.Errorf("failure_context rows: %d want 3", total)
	}

	// c1 retry_count = 0, c2 retry_count = 1 (both for go test).
	var r1, r2 int
	_ = d.QueryRowContext(ctx,
		`SELECT retry_count FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c1')`).Scan(&r1)
	_ = d.QueryRowContext(ctx,
		`SELECT retry_count FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c2')`).Scan(&r2)
	if r1 != 0 || r2 != 1 {
		t.Errorf("retry_count: c1=%d c2=%d want 0, 1", r1, r2)
	}

	// After c3 succeeded, both go test failure rows should flip.
	var es1, es2 int
	_ = d.QueryRowContext(ctx,
		`SELECT eventually_succeeded FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c1')`).Scan(&es1)
	_ = d.QueryRowContext(ctx,
		`SELECT eventually_succeeded FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c2')`).Scan(&es2)
	if es1 != 1 || es2 != 1 {
		t.Errorf("eventually_succeeded: c1=%d c2=%d want 1, 1", es1, es2)
	}

	// c4 (unrelated command) should NOT be flipped by c3's success.
	var es4 int
	_ = d.QueryRowContext(ctx,
		`SELECT eventually_succeeded FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c4')`).Scan(&es4)
	if es4 != 0 {
		t.Errorf("eventually_succeeded for unrelated failure: %d want 0", es4)
	}

	// Error categories.
	var cat1, cat4 string
	_ = d.QueryRowContext(ctx,
		`SELECT error_category FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c1')`).Scan(&cat1)
	_ = d.QueryRowContext(ctx,
		`SELECT error_category FROM failure_context WHERE action_id =
		 (SELECT id FROM actions WHERE source_event_id = 'c4')`).Scan(&cat4)
	if cat1 != "test_failure" {
		t.Errorf("c1 category: %q want test_failure", cat1)
	}
	if cat4 != "runtime" {
		t.Errorf("c4 category: %q want runtime", cat4)
	}
}

func TestIngestEndToEnd(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	events := []models.ToolEvent{
		{
			SourceFile: "f.jsonl", SourceEventID: "e1",
			SessionID: "sess-1", ProjectRoot: "/tmp/proj",
			Timestamp: now, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "README.md",
			Success: true, RawToolName: "Read",
		},
		{
			SourceFile: "f.jsonl", SourceEventID: "e2",
			SessionID: "sess-1", ProjectRoot: "/tmp/proj",
			Timestamp: now.Add(time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionRunCommand, Target: "ls",
			Success: true, RawToolName: "Bash",
		},
		// Skippable: missing SessionID.
		{
			SourceFile: "f.jsonl", SourceEventID: "e3",
			ProjectRoot: "/tmp/proj", Tool: models.ToolClaudeCode,
			Timestamp: now, ActionType: models.ActionReadFile,
		},
	}
	tokens := []models.TokenEvent{
		{
			SourceFile: "f.jsonl", SourceEventID: "t1",
			SessionID: "sess-1", Timestamp: now,
			Tool: models.ToolClaudeCode, InputTokens: 10, OutputTokens: 20,
			Source: models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
		},
	}

	native := map[string]bool{"Read": true, "Edit": true, "Write": true, "Grep": true, "Glob": true}
	res, err := s.Ingest(ctx, events, tokens, IngestOptions{
		IsNativeTool: func(n string) bool { return native[n] },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ActionsInserted != 2 {
		t.Errorf("actions inserted: %d", res.ActionsInserted)
	}
	if res.TokensInserted != 1 {
		t.Errorf("tokens inserted: %d", res.TokensInserted)
	}
	if res.ProjectsTouched != 1 {
		t.Errorf("projects touched: %d", res.ProjectsTouched)
	}
	if res.SessionsTouched != 1 {
		t.Errorf("sessions touched: %d", res.SessionsTouched)
	}

	// is_native_tool was set correctly.
	var readNative, bashNative int
	_ = s.db.QueryRowContext(ctx,
		`SELECT is_native_tool FROM actions WHERE source_event_id = 'e1'`).Scan(&readNative)
	_ = s.db.QueryRowContext(ctx,
		`SELECT is_native_tool FROM actions WHERE source_event_id = 'e2'`).Scan(&bashNative)
	if readNative != 1 || bashNative != 0 {
		t.Errorf("is_native_tool: Read=%d Bash=%d want 1, 0", readNative, bashNative)
	}

	// Re-ingesting produces no duplicates. Both actions and
	// token_usage now use ON CONFLICT DO UPDATE for select fields
	// (actions.duration_ms, token_usage.model — both descriptive,
	// adapter-improvement-driven), so RowsAffected counts shifted to
	// "rows touched" rather than "actually new rows" in v1.4.27 +
	// v1.4.28. The real "no duplicates" property is the
	// post-condition row count, not the metric.
	if _, err := s.Ingest(ctx, events, tokens, IngestOptions{
		IsNativeTool: func(n string) bool { return native[n] },
	}); err != nil {
		t.Fatal(err)
	}
	var actionRows, tokenRows int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&actionRows)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_usage`).Scan(&tokenRows)
	if actionRows != 2 {
		t.Errorf("post re-ingest action rows: %d want 2", actionRows)
	}
	if tokenRows != 1 {
		t.Errorf("post re-ingest token rows: %d want 1", tokenRows)
	}
}

// TestInsertTokenEvents_ModelRefreshOnConflict pins the audit-fix v1.4.27
// behavior where re-ingesting a token row with a more specific model
// (e.g. an adapter improving from "copilot/auto" → "claude-haiku-4-5...")
// upgrades the existing row in place. Counts/cost/source/reliability
// must NOT change — they are quality-sensitive and a re-parse should
// never clobber a proxy-accurate value.
func TestInsertTokenEvents_ModelRefreshOnConflict(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 2, 11, 8, 10, 0, time.UTC)

	if _, err := s.UpsertProject(ctx, "/tmp/proj", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSession(ctx, models.Session{
		ID: "sess-mr", ProjectID: 1, Tool: models.ToolCopilot,
		StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// First insert: placeholder model.
	first := []models.TokenEvent{{
		SourceFile: "f.jsonl", SourceEventID: "request_x:usage",
		SessionID: "sess-mr", Timestamp: now,
		Tool: models.ToolCopilot, Model: "copilot/auto",
		InputTokens: 100, OutputTokens: 50,
		Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
	}}
	if _, err := s.InsertTokenEvents(ctx, first); err != nil {
		t.Fatal(err)
	}

	// Re-insert with the resolved model — adapter-side improvement.
	// Counts and reliability are intentionally different to confirm
	// they are NOT clobbered.
	second := []models.TokenEvent{{
		SourceFile: "f.jsonl", SourceEventID: "request_x:usage",
		SessionID: "sess-mr", Timestamp: now,
		Tool: models.ToolCopilot, Model: "claude-haiku-4-5-20251001",
		InputTokens: 999999, OutputTokens: 999999,
		Source: models.TokenSourceJSONL, Reliability: models.ReliabilityUnreliable,
	}}
	if _, err := s.InsertTokenEvents(ctx, second); err != nil {
		t.Fatal(err)
	}

	var rows int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM token_usage`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("post re-ingest rows: got %d want 1", rows)
	}

	var model, reliability string
	var inTok, outTok int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT model, reliability, input_tokens, output_tokens
		FROM token_usage WHERE source_event_id = 'request_x:usage'`).Scan(
		&model, &reliability, &inTok, &outTok,
	); err != nil {
		t.Fatal(err)
	}
	if model != "claude-haiku-4-5-20251001" {
		t.Errorf("model: got %q want claude-haiku-4-5-20251001", model)
	}
	// Counts + reliability must reflect the FIRST insert.
	if inTok != 100 || outTok != 50 {
		t.Errorf("counts overwritten: in=%d out=%d want 100/50", inTok, outTok)
	}
	if reliability != models.ReliabilityApproximate {
		t.Errorf("reliability overwritten: got %q want %q", reliability, models.ReliabilityApproximate)
	}

	// Empty-model re-insert must NOT erase a previously-resolved model.
	third := []models.TokenEvent{{
		SourceFile: "f.jsonl", SourceEventID: "request_x:usage",
		SessionID: "sess-mr", Timestamp: now,
		Tool: models.ToolCopilot, Model: "",
		InputTokens: 100, OutputTokens: 50,
		Source: models.TokenSourceJSONL, Reliability: models.ReliabilityApproximate,
	}}
	if _, err := s.InsertTokenEvents(ctx, third); err != nil {
		t.Fatal(err)
	}
	_ = s.db.QueryRowContext(ctx, `
		SELECT model FROM token_usage WHERE source_event_id = 'request_x:usage'`).Scan(&model)
	if model != "claude-haiku-4-5-20251001" {
		t.Errorf("empty-model re-insert clobbered resolved value: got %q", model)
	}
}

func TestInsertAPITurn(t *testing.T) {
	t.Parallel()
	s, raw := newTestStore(t)
	ctx := context.Background()

	ts := time.Date(2026, 4, 16, 14, 0, 0, 0, time.UTC)
	id, err := s.InsertAPITurn(ctx, models.APITurn{
		SessionID:           "sess-1",
		Timestamp:           ts,
		Provider:            models.ProviderAnthropic,
		Model:               "claude-sonnet-4",
		RequestID:           "req_1",
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     200,
		CacheCreationTokens: 0,
		MessageCount:        3,
		ToolUseCount:        1,
		SystemPromptHash:    "hash",
		TotalResponseMS:     1234,
		StopReason:          "end_turn",
	})
	if err != nil {
		t.Fatalf("InsertAPITurn: %v", err)
	}
	if id == 0 {
		t.Error("expected non-zero row id")
	}

	// Nullable round-trip: empty session + zero project + zero cache_creation + zero cost should land as NULL.
	var sess sql.NullString
	var proj sql.NullInt64
	var cacheCreation sql.NullInt64
	var cost sql.NullFloat64
	if err := raw.QueryRowContext(ctx,
		`SELECT session_id, project_id, cache_creation_tokens, cost_usd FROM api_turns WHERE id = ?`, id,
	).Scan(&sess, &proj, &cacheCreation, &cost); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !sess.Valid || sess.String != "sess-1" {
		t.Errorf("session_id: %+v", sess)
	}
	if proj.Valid {
		t.Errorf("project_id should be NULL: %+v", proj)
	}
	if cacheCreation.Valid {
		t.Errorf("cache_creation_tokens should be NULL: %+v", cacheCreation)
	}
	if cost.Valid {
		t.Errorf("cost_usd should be NULL: %+v", cost)
	}

	// Second insert with empty session leaves session_id NULL.
	id2, err := s.InsertAPITurn(ctx, models.APITurn{
		Timestamp: ts,
		Provider:  models.ProviderOpenAI,
		Model:     "gpt-5",
	})
	if err != nil {
		t.Fatalf("InsertAPITurn 2: %v", err)
	}
	var sess2 sql.NullString
	if err := raw.QueryRowContext(ctx, `SELECT session_id FROM api_turns WHERE id = ?`, id2).Scan(&sess2); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if sess2.Valid {
		t.Errorf("session_id should be NULL: %+v", sess2)
	}

	n, err := s.CountAPITurns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("count: got %d want 2", n)
	}

	// Validation: missing Provider/Model must error.
	if _, err := s.InsertAPITurn(ctx, models.APITurn{Timestamp: ts}); err == nil {
		t.Error("expected error for missing provider/model")
	}
}
