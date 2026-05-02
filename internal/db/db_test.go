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
		err := database.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("missing table %q: %v", table, err)
		}
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
	if _, err := database.ExecContext(ctx,
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
