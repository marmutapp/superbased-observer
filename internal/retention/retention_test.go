package retention

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// fakeNow lets tests anchor "now" so age cutoffs are deterministic.
var fakeNow = time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)

func init() {
	nowUTC = func() time.Time { return fakeNow }
}

// seed builds a db with a few sessions of varying ages and returns it,
// the path, and the store for additional inserts.
func seed(t *testing.T) (string, *store.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	st := store.New(d)

	// 3 sessions at -200d, -90d, -10d.
	ages := []time.Duration{-200 * 24 * time.Hour, -90 * 24 * time.Hour, -10 * 24 * time.Hour}
	idx := indexing.New(d, 0)
	for i, age := range ages {
		ts := fakeNow.Add(age)
		ev := models.ToolEvent{
			SourceFile: "f", SourceEventID: makeID(i),
			SessionID:   "sess-" + makeID(i),
			ProjectRoot: "/repo",
			Timestamp:   ts,
			Tool:        models.ToolClaudeCode,
			ActionType:  models.ActionRunCommand,
			Target:      "go test",
			Success:     i == 1, // mid-age succeeds, others fail
			ErrorMessage: func() string {
				if i == 1 {
					return ""
				}
				return "FAIL"
			}(),
			ToolOutput: "PASS some test output content",
		}
		if _, err := st.Ingest(context.Background(), []models.ToolEvent{ev}, nil, store.IngestOptions{
			RecordFailures: true,
			Indexer:        idx,
		}); err != nil {
			t.Fatalf("Ingest %d: %v", i, err)
		}
	}
	return dbPath, st
}

func makeID(i int) string {
	return string(rune('a' + i))
}

func TestRun_AgePruning(t *testing.T) {
	dbPath, st := seed(t)

	// Cap MaxAgeDays at 100 → only the -200d action gets pruned.
	d := openExisting(t, dbPath)
	p := New(d)
	res, err := p.Run(context.Background(), Options{
		MaxAgeDays: 100,
		DBPath:     dbPath,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ActionsDeleted != 1 {
		t.Errorf("actions deleted: %d (want 1)", res.ActionsDeleted)
	}
	if res.OrphanedSessionsDeleted != 1 {
		t.Errorf("orphaned sessions deleted: %d (want 1)", res.OrphanedSessionsDeleted)
	}
	if res.ExcerptsDeleted != 1 {
		t.Errorf("excerpts deleted: %d (want 1)", res.ExcerptsDeleted)
	}
	if res.FailureContextDeleted != 1 {
		t.Errorf("failure_context deleted: %d (want 1 — the -200d failure)", res.FailureContextDeleted)
	}
	// Verify remaining counts.
	n, _ := st.CountActions(context.Background())
	if n != 2 {
		t.Errorf("remaining actions: %d (want 2)", n)
	}
}

func TestRun_NoAgeCapKeepsEverything(t *testing.T) {
	dbPath, _ := seed(t)
	d := openExisting(t, dbPath)
	p := New(d)
	res, err := p.Run(context.Background(), Options{
		MaxAgeDays: 0, // disabled
		DBPath:     dbPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ActionsDeleted != 0 {
		t.Errorf("expected 0 actions deleted with disabled age cap: %d", res.ActionsDeleted)
	}
}

func TestRun_FileStateAge(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Insert a project and an old + recent file_state row directly so we
	// don't need to drive the freshness pipeline.
	_, _ = d.ExecContext(context.Background(),
		`INSERT INTO projects (root_path, created_at) VALUES ('/r', ?)`,
		fakeNow.Format(time.RFC3339Nano))
	var pid int64
	_ = d.QueryRowContext(context.Background(), `SELECT id FROM projects WHERE root_path='/r'`).Scan(&pid)

	stale := fakeNow.AddDate(0, 0, -45).Format(time.RFC3339Nano)
	recent := fakeNow.AddDate(0, 0, -5).Format(time.RFC3339Nano)
	_, _ = d.ExecContext(context.Background(),
		`INSERT INTO file_state (project_id, file_path, content_hash, file_mtime, file_size_bytes, last_action_type, last_seen_at)
		 VALUES (?, 'a.go', 'h', ?, 0, 'read_file', ?)`, pid, stale, stale)
	_, _ = d.ExecContext(context.Background(),
		`INSERT INTO file_state (project_id, file_path, content_hash, file_mtime, file_size_bytes, last_action_type, last_seen_at)
		 VALUES (?, 'b.go', 'h', ?, 0, 'read_file', ?)`, pid, recent, recent)

	p := New(d)
	res, err := p.Run(context.Background(), Options{DBPath: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if res.FileStateDeleted != 1 {
		t.Errorf("file_state deleted: %d (want 1 — the 45-day-old row)", res.FileStateDeleted)
	}
}

func TestRun_ObserverLogPruning(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	old := fakeNow.AddDate(0, 0, -45).Format(time.RFC3339Nano)
	young := fakeNow.AddDate(0, 0, -5).Format(time.RFC3339Nano)
	_, _ = d.ExecContext(context.Background(),
		`INSERT INTO observer_log (timestamp, level, component, message) VALUES (?, 'info', 'x', 'old')`, old)
	_, _ = d.ExecContext(context.Background(),
		`INSERT INTO observer_log (timestamp, level, component, message) VALUES (?, 'info', 'x', 'young')`, young)

	p := New(d)
	res, err := p.Run(context.Background(), Options{
		ObserverLogMaxAgeDays: 30,
		DBPath:                dbPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.LogEntriesDeleted != 1 {
		t.Errorf("log entries deleted: %d (want 1)", res.LogEntriesDeleted)
	}
}

func TestRun_NilDBErrors(t *testing.T) {
	p := New(nil)
	if _, err := p.Run(context.Background(), Options{}); err == nil {
		t.Error("expected error for nil DB")
	}
}

func TestRun_FileStateLastActionIDNulledWhenActionDeleted(t *testing.T) {
	dbPath, _ := seed(t)
	d := openExisting(t, dbPath)
	p := New(d)

	if _, err := p.Run(context.Background(), Options{
		MaxAgeDays: 100,
		DBPath:     dbPath,
	}); err != nil {
		t.Fatal(err)
	}
	// All file_state rows should now have last_action_id NULL or pointing
	// to actions that still exist.
	var orphaned int
	if err := d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM file_state fs
		 WHERE fs.last_action_id IS NOT NULL
		   AND fs.last_action_id NOT IN (SELECT id FROM actions)`,
	).Scan(&orphaned); err != nil {
		t.Fatal(err)
	}
	if orphaned != 0 {
		t.Errorf("found %d file_state rows with dangling last_action_id", orphaned)
	}
}

// TestRun_OrphanedSessions_TokenUsageReferencePreserved guards a real
// foreign-key bug from the live install: sessions whose actions had all
// aged out but whose token_usage rows still existed (subagent compaction
// turns emit usage with no tool_use blocks — see PROGRESS.md decision log
// 2026-04-16) tripped the FK constraint when the orphan predicate looked
// only at the actions table. The pruner must keep those sessions.
func TestRun_OrphanedSessions_TokenUsageReferencePreserved(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	st := store.New(d)

	// Seed one session that has only token_usage (no actions).
	tokenOnly := models.TokenEvent{
		SourceFile:    "j",
		SourceEventID: "tok-1",
		SessionID:     "sess-tokens-only",
		ProjectRoot:   "/repo",
		Timestamp:     fakeNow.Add(-5 * 24 * time.Hour),
		Tool:          models.ToolClaudeCode,
		Model:         "claude-sonnet-4-5",
		InputTokens:   1000,
		OutputTokens:  200,
		Source:        models.TokenSourceJSONL,
		Reliability:   models.ReliabilityApproximate,
	}
	if _, err := st.Ingest(context.Background(), nil, []models.TokenEvent{tokenOnly}, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest tokens-only: %v", err)
	}
	// Sanity: session row landed.
	var n int
	if err := d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = 'sess-tokens-only'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("setup: expected 1 sess-tokens-only row, got %d", n)
	}

	// Run retention. The orphan predicate must not touch the session
	// because token_usage still references it.
	p := New(d)
	res, err := p.Run(context.Background(), Options{
		MaxAgeDays: 365, // doesn't apply to this 5-day-old data
		DBPath:     dbPath,
	})
	if err != nil {
		t.Fatalf("Run: %v (FK probably tripped — orphan predicate too narrow)", err)
	}
	if res.OrphanedSessionsDeleted != 0 {
		t.Errorf("OrphanedSessionsDeleted: %d (want 0 — session has token_usage)", res.OrphanedSessionsDeleted)
	}
	if err := d.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sessions WHERE id = 'sess-tokens-only'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("session was deleted despite token_usage reference (count=%d)", n)
	}
}

// openExisting reopens a DB at path so we can re-create the *sql.DB after
// the seed test cleanup. Avoids leaking handles between subtests.
func openExisting(t *testing.T, path string) *sql.DB {
	t.Helper()
	d, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
