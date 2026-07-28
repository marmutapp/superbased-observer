package db

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"
)

// The Ticket-A regression tests (docs/plans/claude-code-hook-stall-ticket-
// and-db-prune-plan-2026-07-12.md): backfillPathHashes must not run on the
// default fast open (IntegrityCheck=false — the hook path, every CLI
// command, and the daemon's own opens), and the maintenance path must pay
// the table scan at most once per DB thanks to the schema_meta
// done-marker. All assertions are structural (a scan-was-called probe),
// never wall-clock.
//
// These tests mutate the package-global backfillProbeHook, so they must
// NOT call t.Parallel() — sequential tests never overlap parallel ones,
// keeping the counter uncontaminated by sibling Opens.

// installBackfillProbe replaces backfillProbeHook with a counter for the
// duration of the test and returns the counter.
func installBackfillProbe(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := backfillProbeHook
	backfillProbeHook = func() { n.Add(1) }
	t.Cleanup(func() { backfillProbeHook = prev })
	return &n
}

// seedLegacyRows simulates a pre-migration-034 corpus: a project and an
// action whose hash columns are empty while their source columns are
// populated — exactly the rows backfillPathHashes exists to repair. It
// also clears the done-marker (the seeding Open will have set it on the
// then-empty DB) so the next daemon open must perform a real scan.
func seedLegacyRows(ctx context.Context, t *testing.T, database *sql.DB) {
	t.Helper()
	stmts := []struct {
		name string
		sql  string
		args []any
	}{
		{"project", `INSERT INTO projects(id, root_path, root_path_hash, created_at)
			VALUES (1, '/legacy/repo', '', '2026-01-01T00:00:00Z')`, nil},
		{"session", `INSERT INTO sessions(id, project_id, tool, started_at)
			VALUES ('sess-legacy', 1, 'claude-code', '2026-01-01T00:00:00Z')`, nil},
		{"action", `INSERT INTO actions(session_id, project_id, timestamp, action_type, tool, source_file, source_file_hash)
			VALUES ('sess-legacy', 1, '2026-01-01T00:00:01Z', 'file_read', 'claude-code', '/legacy/repo/main.go', '')`, nil},
		{"clear-marker", `DELETE FROM schema_meta WHERE key = ?`, []any{pathHashBackfillDoneKey}},
	}
	for _, s := range stmts {
		if _, err := database.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
	}
}

func TestBackfillPathHashes_OpenPathGating(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		// opens is the sequence of Open calls the scenario performs after
		// legacy rows are seeded; each entry is that call's IntegrityCheck
		// value (opt-IN since 2026-07-28 — false is the default fast open
		// every hook and CLI command takes, true is the maintenance open).
		opens []bool
		// wantScans is the expected cumulative probe count after all opens.
		wantScans int32
		// wantHashFilled asserts the legacy action row's hash was repaired
		// by the end of the sequence.
		wantHashFilled bool
	}{
		{
			name:           "default open skips the scan entirely",
			opens:          []bool{false},
			wantScans:      0,
			wantHashFilled: false,
		},
		{
			name:           "repeated default opens never scan",
			opens:          []bool{false, false, false},
			wantScans:      0,
			wantHashFilled: false,
		},
		{
			name:           "maintenance open scans once and repairs hashes",
			opens:          []bool{true},
			wantScans:      1,
			wantHashFilled: true,
		},
		{
			name:           "second maintenance open short-circuits on the marker",
			opens:          []bool{true, true},
			wantScans:      1,
			wantHashFilled: true,
		},
		{
			name:           "default then maintenance then default scans exactly once",
			opens:          []bool{false, true, false, true},
			wantScans:      1,
			wantHashFilled: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/backfill.db"

			// Seed pass: create the schema, then plant legacy rows and
			// clear the marker so the scenario starts as a
			// pre-marker, unbackfilled DB.
			seedDB, err := Open(ctx, Options{Path: path})
			if err != nil {
				t.Fatalf("seed Open: %v", err)
			}
			seedLegacyRows(ctx, t, seedDB)
			if err := seedDB.Close(); err != nil {
				t.Fatalf("seed Close: %v", err)
			}

			probe := installBackfillProbe(t)
			for i, check := range tc.opens {
				database, err := Open(ctx, Options{Path: path, IntegrityCheck: check})
				if err != nil {
					t.Fatalf("Open #%d (check=%v): %v", i, check, err)
				}
				if err := database.Close(); err != nil {
					t.Fatalf("Close #%d: %v", i, err)
				}
			}

			if got := probe.Load(); got != tc.wantScans {
				t.Errorf("scan count = %d, want %d", got, tc.wantScans)
			}

			verify, err := Open(ctx, Options{Path: path})
			if err != nil {
				t.Fatalf("verify Open: %v", err)
			}
			defer verify.Close()
			var hash string
			if err := verify.QueryRowContext(ctx,
				`SELECT source_file_hash FROM actions WHERE session_id = 'sess-legacy'`).Scan(&hash); err != nil {
				t.Fatalf("read action hash: %v", err)
			}
			if filled := hash != ""; filled != tc.wantHashFilled {
				t.Errorf("action hash filled = %v (hash=%q), want %v", filled, hash, tc.wantHashFilled)
			}
			if tc.wantHashFilled {
				var rootHash string
				if err := verify.QueryRowContext(ctx,
					`SELECT root_path_hash FROM projects WHERE id = 1`).Scan(&rootHash); err != nil {
					t.Fatalf("read project hash: %v", err)
				}
				if rootHash == "" {
					t.Error("project root_path_hash still empty after daemon backfill")
				}
				if !pathHashBackfillDone(ctx, verify) {
					t.Error("done-marker not set after daemon backfill")
				}
			}
		})
	}
}

// TestBackfillPathHashes_FreshMaintenanceSetsMarker pins that the first
// maintenance pass over a fresh DB (no legacy rows) records the done-marker,
// so a daemon restart never re-pays even the empty scan.
//
// Since the 2026-07-28 polarity inversion the marker is written by
// RunStartupMaintenance — the daemon's once-per-process background pass —
// rather than by whichever Open happened to come first. That is the correct
// single owner: plain Opens (hooks, CLI commands, the daemon's own dozen
// feature goroutines) now neither scan nor mark.
func TestBackfillPathHashes_FreshMaintenanceSetsMarker(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/fresh.db"

	first, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if pathHashBackfillDone(ctx, first) {
		t.Fatal("done-marker set by a plain Open — the scan must be opt-in")
	}
	if err := RunStartupMaintenance(ctx, first); err != nil {
		t.Fatalf("RunStartupMaintenance: %v", err)
	}
	if !pathHashBackfillDone(ctx, first) {
		t.Fatal("done-marker not set after the first maintenance pass")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	probe := installBackfillProbe(t)
	second, err := Open(ctx, Options{Path: path, IntegrityCheck: true})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer second.Close()
	if got := probe.Load(); got != 0 {
		t.Errorf("re-open scan count = %d, want 0 (marker should short-circuit)", got)
	}
}
