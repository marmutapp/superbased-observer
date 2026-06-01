package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db/migrations"
)

// orgTables are the six tables migration 028 introduces.
var orgTables = []string{
	"org_enrolment", "org_members", "org_teams",
	"org_team_members", "org_project_team", "org_push_log",
}

// orgAttributionTables maps each table migration 029 touches to the two
// attribution columns it adds.
var orgAttributionTables = []string{"actions", "sessions", "api_turns", "token_usage"}

// orgPartialIndexes are the four partial indexes migration 029 creates.
var orgPartialIndexes = []string{
	"idx_actions_org_user", "idx_sessions_org_user",
	"idx_api_turns_org_user", "idx_token_usage_org_user",
}

// TestMigrationsFresh_AllApplied proves a fresh database migrates to the
// highest embedded version and exposes the M0 org schema (028 tables +
// 029 attribution columns + partial indexes).
func TestMigrationsFresh_AllApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	entries, err := readMigrationEntries()
	if err != nil {
		t.Fatalf("readMigrationEntries: %v", err)
	}
	wantVersion := entries[len(entries)-1].version

	database, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	got, err := Version(ctx, database)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if got != wantVersion {
		t.Fatalf("version = %d, want highest embedded migration %d", got, wantVersion)
	}

	for _, table := range orgTables {
		if !tableExists(t, database, table) {
			t.Errorf("migration 028: table %q missing on fresh DB", table)
		}
	}
	for _, table := range orgAttributionTables {
		for _, col := range []string{"org_id", "user_email"} {
			if !columnExists(t, database, table, col) {
				t.Errorf("migration 029: %s.%s column missing on fresh DB", table, col)
			}
		}
	}
	for _, idx := range orgPartialIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("migration 029: partial index %q missing on fresh DB", idx)
		}
	}

	// org_enrolment is a singleton (CHECK id = 1): a second row must fail.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO org_enrolment (id, org_id, org_name, org_server_url, user_id, user_email, enrolled_at, bearer_key_id)
		 VALUES (2, 'o', 'n', 'u', 'uid', 'e', 't', 'k')`); err == nil {
		t.Error("org_enrolment accepted id=2; CHECK (id = 1) not enforced")
	}
}

// TestMigrationsUpgrade_27_then_28_29 proves the upgrade path: a database
// already at version 27 with pre-existing rows upgrades cleanly to the
// latest version, the new attribution columns land NULL on those existing
// rows, and the new tables/indexes appear. This exercises the real
// runMigrations runner (not a fresh-apply), matching what an existing
// install hits on first launch of the M0 binary.
func TestMigrationsUpgrade_27_then_28_29(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")

	// Raw open (no migrations) so we can stop at version 27 deliberately.
	dsn := "file:" + path + "?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	entries, err := readMigrationEntries()
	if err != nil {
		t.Fatalf("readMigrationEntries: %v", err)
	}

	// Bootstrap schema_meta and replay migration bodies 001..027 in order.
	if _, err := database.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("bootstrap schema_meta: %v", err)
	}
	for _, e := range entries {
		if e.version > 27 {
			continue
		}
		body, readErr := fs.ReadFile(migrations.Files, e.filename)
		if readErr != nil {
			t.Fatalf("read %s: %v", e.filename, readErr)
		}
		if _, err := database.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", e.filename, err)
		}
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES ('version', '27')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
		t.Fatalf("pin version 27: %v", err)
	}

	// Sanity: at version 27, the M0 columns/tables do NOT yet exist.
	if v, err := Version(ctx, database); err != nil || v != 27 {
		t.Fatalf("pre-upgrade version = %d (err=%v), want 27", v, err)
	}
	if columnExists(t, database, "actions", "org_id") {
		t.Fatal("actions.org_id present before migration 029")
	}
	if tableExists(t, database, "org_enrolment") {
		t.Fatal("org_enrolment present before migration 028")
	}

	// Seed pre-existing rows so we can assert the new columns land NULL.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (1, '/p', '2026-05-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, total_actions)
		 VALUES ('sA', 1, 'claude-code', '2026-05-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO token_usage(session_id, timestamp, tool, model,
		   input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		   source, reliability, source_file, source_event_id)
		 VALUES ('sA', '2026-05-01T01:00:00Z', 'claude-code', 'claude-opus-4-7',
		   10, 20, 0, 0, 'jsonl', 'unreliable', '/f.jsonl', 'evt-1')`); err != nil {
		t.Fatalf("seed token_usage: %v", err)
	}

	// Run the REAL runner — it sees applied=27 and applies 028 + 029.
	if err := runMigrations(ctx, database); err != nil {
		t.Fatalf("upgrade runMigrations: %v", err)
	}

	wantVersion := entries[len(entries)-1].version
	if v, err := Version(ctx, database); err != nil || v != wantVersion {
		t.Fatalf("post-upgrade version = %d (err=%v), want %d", v, err, wantVersion)
	}

	// New schema present after upgrade.
	for _, table := range orgTables {
		if !tableExists(t, database, table) {
			t.Errorf("post-upgrade: table %q missing", table)
		}
	}
	for _, table := range orgAttributionTables {
		for _, col := range []string{"org_id", "user_email"} {
			if !columnExists(t, database, table, col) {
				t.Errorf("post-upgrade: %s.%s column missing", table, col)
			}
		}
	}
	for _, idx := range orgPartialIndexes {
		if !indexExists(t, database, idx) {
			t.Errorf("post-upgrade: partial index %q missing", idx)
		}
	}

	// Pre-existing rows must have NULL attribution (additive, no backfill).
	for _, q := range []string{
		`SELECT COUNT(*) FROM sessions WHERE id = 'sA' AND org_id IS NULL AND user_email IS NULL`,
		`SELECT COUNT(*) FROM token_usage WHERE source_event_id = 'evt-1' AND org_id IS NULL AND user_email IS NULL`,
	} {
		var n int
		if err := database.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("null-attribution check: %v\nquery: %s", err, q)
		}
		if n != 1 {
			t.Errorf("expected pre-existing row with NULL attribution, got %d for: %s", n, q)
		}
	}
}

func tableExists(t *testing.T, database *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := database.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		t.Fatalf("tableExists(%s): %v", name, err)
	}
	return true
}

func indexExists(t *testing.T, database *sql.DB, name string) bool {
	t.Helper()
	var got string
	err := database.QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		return false
	case err != nil:
		t.Fatalf("indexExists(%s): %v", name, err)
	}
	return true
}

func columnExists(t *testing.T, database *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := database.QueryContext(context.Background(),
		`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("columnExists(%s.%s): %v", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("columnExists scan: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("columnExists rows: %v", err)
	}
	return false
}
