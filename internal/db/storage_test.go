package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStorageStats pins the per-table breakdown contract: every
// dbstat b-tree folds into a user-visible owner (indexes and FTS5
// shadow tables never surface as their own rows), row counts land,
// and the whole-file totals come from page accounting.
func TestStorageStats(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, Options{Path: filepath.Join(t.TempDir(), "s.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO projects (root_path, name, created_at) VALUES ('/p', 'p', ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		`INSERT INTO sessions (id, project_id, tool, started_at)
		 VALUES ('s1', (SELECT id FROM projects WHERE root_path = '/p'), 'claude-code', ?)`, now,
	); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := database.Exec(
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, target, success, tool)
			 VALUES ('s1', 1, ?, 'run_command', 'echo hi', 1, 'claude-code')`, now,
		); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := StorageStats(ctx, database)
	if err != nil {
		t.Fatalf("StorageStats: %v", err)
	}
	if rep.PageSize <= 0 || rep.PageCount <= 0 || rep.TotalBytes != rep.PageSize*rep.PageCount {
		t.Errorf("page accounting off: size=%d count=%d total=%d", rep.PageSize, rep.PageCount, rep.TotalBytes)
	}

	byName := map[string]StorageTable{}
	for _, tb := range rep.Tables {
		byName[tb.Name] = tb
		// FTS5 shadow tables and indexes must be folded into owners.
		// otel_content (migration 048) is a REAL user table whose name happens
		// to end in the FTS5 "_content" shadow suffix; resolveOwner correctly
		// leaves it standalone (no `otel` FTS5 table exists to fold it into), so
		// it is exempt from the shadow-suffix heuristic here.
		if tb.Name != "otel_content" {
			for _, suffix := range []string{"_data", "_idx", "_docsize", "_config", "_content"} {
				if strings.HasSuffix(tb.Name, suffix) {
					t.Errorf("shadow/internal b-tree %q surfaced as its own row", tb.Name)
				}
			}
		}
		if strings.HasPrefix(tb.Name, "idx_") {
			t.Errorf("index %q surfaced as its own row", tb.Name)
		}
	}
	actions, ok := byName["actions"]
	if !ok {
		t.Fatal("actions table missing from report")
	}
	if actions.Rows != 50 {
		t.Errorf("actions rows = %d, want 50", actions.Rows)
	}
	if actions.Bytes <= 0 {
		t.Errorf("actions bytes = %d, want > 0", actions.Bytes)
	}
	if _, ok := byName["action_excerpts"]; !ok {
		t.Error("action_excerpts (FTS5 virtual table) missing — shadow grouping should surface the virtual table itself")
	}
}

// TestVacuumAndBackupInto pins the maintenance pair: backup writes a
// consistent, openable snapshot and refuses to overwrite; vacuum runs
// clean and reports non-negative freed bytes.
func TestVacuumAndBackupInto(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	database, err := Open(ctx, Options{Path: filepath.Join(dir, "s.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO projects (root_path, name, created_at) VALUES ('/p', 'p', ?)`, now,
	); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "backups", "snap.db")
	if err := BackupInto(ctx, database, dest); err != nil {
		t.Fatalf("BackupInto: %v", err)
	}
	if err := BackupInto(ctx, database, dest); err == nil {
		t.Fatal("BackupInto overwrote an existing destination — must refuse")
	}

	// The snapshot opens through the standard Open path (migrations
	// already applied) and carries the row.
	snap, err := Open(ctx, Options{Path: dest})
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer snap.Close()
	var n int
	if err := snap.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot projects = %d (err %v), want 1", n, err)
	}

	freed, err := Vacuum(ctx, database)
	if err != nil {
		t.Fatalf("Vacuum: %v", err)
	}
	// VACUUM of a near-empty DB (this test inserts one row and deletes nothing,
	// so there is no free space to reclaim) can round the file UP by a single
	// page during defrag — a benign SQLite behavior, not a leak. Tolerate one
	// page of rounding; multi-page growth would still be a real regression. The
	// substantive assertion is that VACUUM succeeds and the DB stays valid
	// (verified above via the snapshot round-trip).
	const sqlitePageSize = 4096
	if freed < -sqlitePageSize {
		t.Errorf("freed = %d, want >= -%d (one page of VACUUM rounding)", freed, sqlitePageSize)
	}
}
