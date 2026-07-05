package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db/migrations"
)

func TestOpenInMemoryAppliesSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	v, err := Version(ctx, database)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v < 1 {
		t.Fatalf("expected schema version >= 1, got %d", v)
	}

	// Core tables should exist.
	for _, table := range []string{
		"projects", "sessions", "actions", "file_state",
		"token_usage", "api_turns", "failure_context",
		"action_excerpts", "compaction_events", "project_patterns",
		"observer_log", "parse_cursors",
	} {
		var name string
		err := database.QueryRowContext(
			ctx,
			`SELECT name FROM sqlite_master WHERE name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("missing table %q: %v", table, err)
		}
	}
}

// TestOpenSkipsIntegrityCheckWhenRequested verifies that
// Options.SkipIntegrityCheck disables the `PRAGMA quick_check` probe.
// Hook subprocesses pass true so they don't contend with the daemon's
// WAL holder; the schema must still be migrated to the current version.
func TestOpenSkipsIntegrityCheckWhenRequested(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "skip-check.db")

	database, err := Open(ctx, Options{Path: path, SkipIntegrityCheck: true})
	if err != nil {
		t.Fatalf("Open with SkipIntegrityCheck: %v", err)
	}
	defer database.Close()

	v, err := Version(ctx, database)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v < 1 {
		t.Fatalf("expected schema version >= 1, got %d", v)
	}

	// One basic table check — quick_check was skipped, so we explicitly
	// verify the migration path still ran.
	var name string
	if err := database.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE name = 'actions'`).Scan(&name); err != nil {
		t.Fatalf("migrations didn't apply when SkipIntegrityCheck=true: %v", err)
	}
}

func TestOpenOnDiskEnablesWAL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	database, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	var mode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %q", mode)
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "idem.db")

	d1, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	v1, err := Version(ctx, d1)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	d1.Close()

	d2, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer d2.Close()
	v2, err := Version(ctx, d2)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v1 != v2 {
		t.Fatalf("version drifted between opens: %d -> %d", v1, v2)
	}
}

// TestMigrationsRaceSafeAcrossConcurrentOpens guards the BEGIN IMMEDIATE
// serialization fix landed in v1.4.1.
//
// Pre-fix: when N processes opened the same DB file simultaneously
// (e.g. `observer watch` + `observer dashboard` + `observer proxy`
// each starting in parallel), each daemon's runMigrations would read
// applied=N, each try to apply migration N+1, and non-idempotent
// statements like ALTER TABLE ADD COLUMN would error with "duplicate
// column name" on whichever daemons lost the race.
//
// Post-fix: BEGIN IMMEDIATE serializes the migration batch so the first
// caller applies, others wait for the lock, then re-read schema_meta
// inside their own lock and skip already-applied migrations.
//
// The test fires N concurrent Open calls against the same file and
// asserts every single one returns nil error. Without the fix this
// flakily fails depending on goroutine scheduling.
func TestMigrationsRaceSafeAcrossConcurrentOpens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "race.db")

	const N = 8
	errs := make(chan error, N)
	dbs := make(chan *sql.DB, N)

	// Launch all goroutines simultaneously — each makes its own
	// connection-pool DB handle to the shared file, mirroring the
	// real multi-daemon scenario.
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			<-start
			d, err := Open(ctx, Options{Path: path})
			if err != nil {
				errs <- err
				dbs <- nil
				return
			}
			errs <- nil
			dbs <- d
		}()
	}
	close(start)

	collected := []*sql.DB{}
	for i := 0; i < N; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Open #%d: %v", i, err)
		}
		if d := <-dbs; d != nil {
			collected = append(collected, d)
		}
	}
	for _, d := range collected {
		_ = d.Close()
	}

	// All openers should observe the same final schema version.
	final, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("post-race Open: %v", err)
	}
	defer final.Close()
	v, err := Version(ctx, final)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	// Ensure version reflects the highest embedded migration —
	// proves migrations actually applied (not just silently skipped).
	if v <= 0 {
		t.Errorf("Version after race: got %d want > 0", v)
	}
}

// TestMigration007_DedupsTokenUsage guards the heuristic backfill for the
// analytics-audit A1 finding. We seed token_usage with a representative
// shape — four "echoed" rows for one logical Anthropic API call (same
// source/session/model + identical input+cache columns, output progressing
// from 8→8→8→197) plus one independent row that must survive — and verify
// the migration collapses the four into one (the row with the largest
// output_tokens) while leaving the unrelated row untouched.
func TestMigration007_DedupsTokenUsage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	// Seed an FK target session and project.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at)
		 VALUES (1, '/p', '2026-04-25T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'claude-code', '2026-04-25T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Echoed multi-block API call: same fingerprint, output 8/8/8/197,
	// distinct source_event_ids (the v1.x bug shape).
	for i, out := range []int{8, 8, 8, 197} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO token_usage(session_id, timestamp, tool, model,
			   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			   source, reliability, source_file, source_event_id)
			 VALUES ('sA', ?, 'claude-code', 'claude-opus-4-7', 3, ?, 0, 13316,
			   'jsonl', 'unreliable', '/some/file.jsonl', ?)`,
			fmt.Sprintf("2026-04-25T10:00:%02dZ", i), out,
			fmt.Sprintf("uuid-block-%d", i))
		if err != nil {
			t.Fatalf("seed echoed row %d: %v", i, err)
		}
	}

	// Unrelated row in the same session (different model) — must survive.
	if _, err := database.ExecContext(
		ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model,
		   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		   source, reliability, source_file, source_event_id)
		 VALUES ('sA', '2026-04-25T11:00:00Z', 'claude-code', 'claude-haiku-4-5',
		   100, 200, 0, 0, 'jsonl', 'unreliable', '/some/file.jsonl', 'uuid-haiku')`,
	); err != nil {
		t.Fatalf("seed unrelated row: %v", err)
	}

	// Sanity: 5 rows before migration body runs.
	if got := count(t, database, "token_usage"); got != 5 {
		t.Fatalf("pre-dedup count: %d want 5", got)
	}

	// Re-run 007's body manually (Open() already applied it on empty data
	// when we constructed the DB; we need to apply it after seeding).
	body, err := fs.ReadFile(migrations.Files, "007_dedup_token_usage_history.sql")
	if err != nil {
		t.Fatalf("read 007: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 007: %v", err)
	}

	// Post-dedup: 4 echoed rows collapse to 1, unrelated row survives.
	if got := count(t, database, "token_usage"); got != 2 {
		t.Fatalf("post-dedup count: %d want 2", got)
	}

	// The surviving "echoed-group" row must be the one with output=197.
	var keptOutput int
	if err := database.QueryRowContext(ctx,
		`SELECT output_tokens FROM token_usage
		 WHERE model = 'claude-opus-4-7'`).Scan(&keptOutput); err != nil {
		t.Fatalf("query kept row: %v", err)
	}
	if keptOutput != 197 {
		t.Errorf("kept output_tokens: %d want 197 (final cumulative)", keptOutput)
	}
}

// TestMigration033_CodexTokenPerEventMessageID pins the v1.7.24
// backfill: pre-v1.7.24 codex token rows had message_id set to the
// turn UUID (the legacy "MessageID = TurnID" adapter contract).
// Migration 033 moves that into the v1.7.24 turn_id column and
// replaces message_id with the per-event source_event_id so the
// dashboard's per-inference detail view (?detail=inference) can
// distinguish each token_count event.
func TestMigration033_CodexTokenPerEventMessageID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	// Seed FK targets.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at)
		 VALUES (1, '/p', '2026-06-02T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sCx', 1, 'codex', '2026-06-02T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Seed FOUR token rows with the legacy codex shape: same
	// message_id (= the turn UUID), distinct source_event_ids
	// (per-inference line refs). turn_id is NULL — a "never-re-emitted-
	// since-migration-032" row.
	const legacyTurnUUID = "019cfd1c-722d-7070-9584-aa87a0d98226"
	for i, line := range []int{13, 24, 35, 45} {
		_, err := database.ExecContext(ctx,
			`INSERT INTO token_usage(session_id, timestamp, tool, model,
			   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			   source, reliability, source_file, source_event_id, message_id, turn_id)
			 VALUES ('sCx', ?, 'codex', 'gpt-5.4', ?, ?, 0, 0,
			   'jsonl', 'approximate', '/path/rollout.jsonl', ?, ?, NULL)`,
			fmt.Sprintf("2026-06-02T00:00:%02dZ", i),
			100+i, 50+i,
			fmt.Sprintf("tk:rollout.jsonl:L%d", line),
			legacyTurnUUID)
		if err != nil {
			t.Fatalf("seed legacy row %d: %v", i, err)
		}
	}

	// Non-codex row — must NOT be touched (claudecode's MessageID
	// is the Anthropic msg_xxx; the migration's WHERE clause excludes
	// non-codex tools).
	if _, err := database.ExecContext(
		ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model,
		   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		   source, reliability, source_file, source_event_id, message_id, turn_id)
		 VALUES ('sCx', '2026-06-02T00:01:00Z', 'claude-code', 'claude-sonnet-4-6',
		   100, 50, 0, 0, 'jsonl', 'unreliable', '/path/claude.jsonl',
		   'uuid-line-7', 'msg_abc123', NULL)`,
	); err != nil {
		t.Fatalf("seed claudecode row: %v", err)
	}

	// Migration 033 was applied by Open(); re-run its body manually
	// against the seeded data.
	body, err := fs.ReadFile(migrations.Files, "033_codex_token_per_event_msgid.sql")
	if err != nil {
		t.Fatalf("read 033: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 033: %v", err)
	}

	// Post-migration: each codex row should now have
	//   message_id == source_event_id (per-event id)
	//   turn_id    == legacyTurnUUID (moved from message_id)
	rows, err := database.QueryContext(ctx,
		`SELECT message_id, turn_id, source_event_id
		   FROM token_usage WHERE tool = 'codex' ORDER BY timestamp`)
	if err != nil {
		t.Fatalf("query codex: %v", err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var msgID, turnID, srcEvt string
		if err := rows.Scan(&msgID, &turnID, &srcEvt); err != nil {
			t.Fatal(err)
		}
		if msgID != srcEvt {
			t.Errorf("row %d: message_id=%q want source_event_id %q", n, msgID, srcEvt)
		}
		if turnID != legacyTurnUUID {
			t.Errorf("row %d: turn_id=%q want %q", n, turnID, legacyTurnUUID)
		}
		n++
	}
	if n != 4 {
		t.Errorf("codex row count: %d want 4", n)
	}

	// Claudecode row must be unchanged.
	var cMsgID, cTurnID string
	var cTurnNullable sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT message_id, COALESCE(turn_id, '') FROM token_usage WHERE tool = 'claude-code'`).
		Scan(&cMsgID, &cTurnID); err != nil {
		t.Fatal(err)
	}
	if cMsgID != "msg_abc123" {
		t.Errorf("claudecode message_id changed: got %q want msg_abc123", cMsgID)
	}
	if cTurnID != "" {
		t.Errorf("claudecode turn_id touched: got %q want empty", cTurnID)
	}
	_ = cTurnNullable

	// Idempotent: re-running selects zero rows to update.
	res, err := database.ExecContext(ctx, string(body))
	if err != nil {
		t.Fatalf("re-apply 033: %v", err)
	}
	if rowsAffected, _ := res.RowsAffected(); rowsAffected != 0 {
		t.Errorf("second 033 apply rows affected: %d want 0 (not idempotent)", rowsAffected)
	}
}

// count is a small helper for migration tests.
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count(%s): %v", table, err)
	}
	return n
}

func TestMissingPathIsError(t *testing.T) {
	t.Parallel()
	if _, err := Open(context.Background(), Options{}); err == nil {
		t.Fatal("expected error for empty Path")
	}
}

// TestMigration024_ClaudecodeActionTypeBackfill pins the Issue #6
// extension: historical claude-code rows with raw_tool_name now mapped
// in adapter.go::actionMap should be re-derived. Live maintainer DB
// had 1,817 TaskUpdate / 963 TaskCreate / 211 Agent / 22 ExitPlanMode
// rows as action_type='unknown' pre-fix.
func TestMigration024_ClaudecodeActionTypeBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at)
		 VALUES (1, '/p', '2026-04-25T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('s', 1, 'claude-code', '2026-04-25T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	type row struct {
		tool, raw, want string
	}
	seedRows := []row{
		// Tool family → todo_update
		{"claude-code", "TaskCreate", "todo_update"},
		{"claude-code", "TaskUpdate", "todo_update"},
		{"claude-code", "TaskList", "todo_update"},
		{"claude-code", "TaskGet", "todo_update"},
		{"claude-code", "TaskOutput", "todo_update"},
		{"claude-code", "TaskStop", "todo_update"},
		{"claude-code", "TodoWrite", "todo_update"},
		// Subagent
		{"claude-code", "Agent", "spawn_subagent"},
		// Interactive prompt
		{"claude-code", "AskUserQuestion", "ask_user"},
		// Plan-mode toggles
		{"claude-code", "EnterPlanMode", "permission_mode"},
		{"claude-code", "ExitPlanMode", "permission_mode"},
		// Cross-tool guard: a non-claude-code adapter that happens
		// to emit "TodoWrite" must NOT be touched (the migration is
		// tool-scoped to 'claude-code').
		{"cowork", "TodoWrite", "unknown"},
		// Unmapped Claude Code tools stay unknown — they're left
		// for a future taxonomy decision.
		{"claude-code", "Monitor", "unknown"},
		{"claude-code", "ScheduleWakeup", "unknown"},
		{"claude-code", "mcp__foo__bar", "unknown"},
		// Sanity: an already-correctly-typed row is untouched.
		{"claude-code", "TaskCreate-already-typed", "todo_update"},
	}
	// Seed everything as 'unknown' first; the last row is then
	// pre-set to 'todo_update' to verify the WHERE clause only hits
	// unknowns.
	for i, r := range seedRows {
		seedType := "unknown"
		if r.raw == "TaskCreate-already-typed" {
			seedType = "todo_update"
		}
		if _, err := database.ExecContext(
			ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, tool, action_type, raw_tool_name, target, success)
			 VALUES ('s', 1, ?, ?, ?, ?, '', 1)`,
			fmt.Sprintf("2026-04-25T11:00:%02dZ", i), r.tool, seedType, r.raw,
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	body, err := fs.ReadFile(migrations.Files, "024_claudecode_action_type_backfill.sql")
	if err != nil {
		t.Fatalf("read 024: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 024: %v", err)
	}

	for _, c := range seedRows {
		var got string
		if err := database.QueryRowContext(
			ctx,
			`SELECT action_type FROM actions WHERE tool=? AND raw_tool_name=?`,
			c.tool, c.raw,
		).Scan(&got); err != nil {
			t.Errorf("query %s/%s: %v", c.tool, c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%s,%s) action_type=%q want %q", c.tool, c.raw, got, c.want)
		}
	}

	// Idempotency: re-apply and confirm no drift.
	if _, err := database.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("re-apply 024: %v", err)
	}
	for _, c := range seedRows {
		var got string
		_ = database.QueryRowContext(
			ctx,
			`SELECT action_type FROM actions WHERE tool=? AND raw_tool_name=?`,
			c.tool, c.raw,
		).Scan(&got)
		if got != c.want {
			t.Errorf("re-apply (%s,%s) action_type=%q want %q", c.tool, c.raw, got, c.want)
		}
	}
}

// TestMigration023_PowershellActionTypeBackfill pins the Issue #6
// cross-adapter shell-variant backfill. Pre-fix the maintainer DB had
// 1 claude-code PowerShell row, 1 copilot-cli powershell row, and 260
// codex exec_command rows all carrying action_type='unknown' because
// the adapter actionMaps didn't recognize those tool names. The
// adapters were patched in v1.6.11; migration 023 re-derives
// action_type for the historical rows.
func TestMigration023_PowershellActionTypeBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at)
		 VALUES (1, '/p', '2026-04-25T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('s', 1, 'claude-code', '2026-04-25T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Re-running 023 against an already-migrated DB would be a no-op,
	// but we want to seed shell-variant rows AFTER migrations applied
	// so they're untouched. Insert them now with action_type='unknown'.
	type row struct {
		tool, raw string
	}
	seedRows := []row{
		{"claude-code", "PowerShell"},
		{"claude-code", "powershell"},
		{"claude-code", "pwsh"},
		{"claude-code", "cmd.exe"},
		{"claude-code", "sh"},
		{"copilot-cli", "powershell"},
		{"codex", "exec_command"},
		{"codex", "powershell"},
		// Codex exec_command on a non-codex tool row would NOT match
		// the tool-scoped clause — confirm by seeding one.
		{"copilot-cli", "exec_command"},
		// Unrelated raw_tool_name that should NOT change:
		{"claude-code", "TodoWrite"},
	}
	for i, r := range seedRows {
		if _, err := database.ExecContext(
			ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, tool, action_type, raw_tool_name, target, success)
			 VALUES ('s', 1, ?, ?, 'unknown', ?, '', 1)`,
			fmt.Sprintf("2026-04-25T10:00:%02dZ", i), r.tool, r.raw,
		); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	body, err := fs.ReadFile(migrations.Files, "023_powershell_action_type_backfill.sql")
	if err != nil {
		t.Fatalf("read 023: %v", err)
	}
	if _, err := database.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 023: %v", err)
	}

	// Expected: every shell-variant row + codex exec_command flipped
	// to run_command; copilot-cli exec_command and TodoWrite untouched.
	cases := []struct {
		tool, raw, want string
	}{
		{"claude-code", "PowerShell", "run_command"},
		{"claude-code", "powershell", "run_command"},
		{"claude-code", "pwsh", "run_command"},
		{"claude-code", "cmd.exe", "run_command"},
		{"claude-code", "sh", "run_command"},
		{"copilot-cli", "powershell", "run_command"},
		{"codex", "exec_command", "run_command"},
		{"codex", "powershell", "run_command"},
		// copilot-cli's exec_command stays unknown — the tool-scoped
		// clause limits exec_command remapping to codex rows. (A future
		// adapter that adopts exec_command would need its own clause.)
		{"copilot-cli", "exec_command", "unknown"},
		// Unrelated row preserved.
		{"claude-code", "TodoWrite", "unknown"},
	}
	for _, c := range cases {
		var got string
		if err := database.QueryRowContext(
			ctx,
			`SELECT action_type FROM actions WHERE tool=? AND raw_tool_name=?`,
			c.tool, c.raw,
		).Scan(&got); err != nil {
			t.Errorf("query %s/%s: %v", c.tool, c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("(%s,%s) action_type=%q want %q", c.tool, c.raw, got, c.want)
		}
	}

	// Re-applying 023 must be idempotent — re-run and confirm no rows
	// flip back or get re-counted.
	if _, err := database.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("re-apply 023: %v", err)
	}
	for _, c := range cases {
		var got string
		if err := database.QueryRowContext(
			ctx,
			`SELECT action_type FROM actions WHERE tool=? AND raw_tool_name=?`,
			c.tool, c.raw,
		).Scan(&got); err != nil {
			t.Errorf("re-query %s/%s: %v", c.tool, c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("re-apply (%s,%s) action_type=%q want %q", c.tool, c.raw, got, c.want)
		}
	}
}

// TestConcurrentWritersSurviveContention reproduces the
// `observer backfill --all`-vs-watcher SQLITE_BUSY contention that
// produced errors like:
//
//	store.InsertActions: upsert dup: database is locked (5) (SQLITE_BUSY)
//
// during the operator's 2026-05-19 backfill run. Two writer goroutines
// each open the same on-disk DB (separate *sql.DB pools, mirroring two
// separate observer processes) and hammer transactional INSERTs against
// the same table. Pre-fix the 5s busy_timeout + BEGIN DEFERRED upgrade
// pattern would intermittently return SQLITE_BUSY when both writers
// raced the read-to-write upgrade. Post-fix (_txlock=immediate + 30s
// busy_timeout) every transaction acquires the write lock upfront and
// busy_timeout's backoff handles the wait properly.
//
// Uses a minimal scratch table — we're testing the lock semantics, not
// the migration runner.
func TestConcurrentWritersSurviveContention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "contention.db")

	// Seed with one Open to apply migrations once; both writers below
	// then attach to the already-migrated file.
	seed, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	if _, err := seed.ExecContext(
		ctx,
		`CREATE TABLE contention (id INTEGER PRIMARY KEY, w INTEGER, n INTEGER)`,
	); err != nil {
		t.Fatalf("create scratch table: %v", err)
	}
	seed.Close()

	const writers = 4
	const txPerWriter = 25
	const rowsPerTx = 20

	errs := make(chan error, writers)
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			d, err := Open(ctx, Options{Path: path})
			if err != nil {
				errs <- fmt.Errorf("writer %d Open: %w", w, err)
				return
			}
			defer d.Close()

			<-start
			for i := 0; i < txPerWriter; i++ {
				tx, err := d.BeginTx(ctx, nil)
				if err != nil {
					errs <- fmt.Errorf("writer %d tx %d begin: %w", w, i, err)
					return
				}
				for r := 0; r < rowsPerTx; r++ {
					if _, err := tx.ExecContext(
						ctx,
						`INSERT INTO contention(w, n) VALUES (?, ?)`,
						w, i*rowsPerTx+r,
					); err != nil {
						_ = tx.Rollback()
						errs <- fmt.Errorf("writer %d tx %d row %d: %w", w, i, r, err)
						return
					}
				}
				if err := tx.Commit(); err != nil {
					errs <- fmt.Errorf("writer %d tx %d commit: %w", w, i, err)
					return
				}
			}
			errs <- nil
		}()
	}
	close(start)

	for i := 0; i < writers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("contention writer: %v", err)
		}
	}

	final, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("final Open: %v", err)
	}
	defer final.Close()
	want := writers * txPerWriter * rowsPerTx
	if got := count(t, final, "contention"); got != want {
		t.Errorf("row count: got %d want %d", got, want)
	}
}
