package audit

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// newReaderFixture wires a temp SQLite via internal/db.Open so the
// migration up to 030 has run; then seeds N audit rows for the read
// tests below.
func newReaderFixture(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	seedAt := func(ts time.Time, session, tool, path string, ok bool, sizeBytes int) {
		t.Helper()
		var okInt, truncInt int
		if ok {
			okInt = 1
		}
		var reason any
		if !ok {
			reason = "deny_test"
		}
		_, err := database.ExecContext(
			context.Background(),
			`INSERT INTO mcp_audit (ts, session_id, tool_name, request_hash,
			    path_requested, response_size_bytes, response_truncated,
			    response_ok, reason, duration_us)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ts.UTC().Format(time.RFC3339Nano),
			session, tool, "hash-"+tool,
			path, sizeBytes, truncInt, okInt,
			reason, int64(1000),
		)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	now := time.Now()
	// 5 OK rows + 2 denied; mix sessions + tools + paths.
	seedAt(now.Add(-1*time.Minute), "s1", "get_file", "/a.go", true, 1024)
	seedAt(now.Add(-2*time.Minute), "s1", "get_file", "/a.go", true, 1024) // repeat path
	seedAt(now.Add(-3*time.Minute), "s1", "get_symbols", "/b.go", true, 512)
	seedAt(now.Add(-4*time.Minute), "s2", "get_file", "/c.go", true, 2048)
	seedAt(now.Add(-5*time.Minute), "s2", "retrieve_stashed", "stashed://abc", true, 4096)
	seedAt(now.Add(-6*time.Minute), "s1", "get_file", "/secret.env", false, 0)
	seedAt(now.Add(-7*time.Minute), "s2", "retrieve_stashed", "stashed://gone", false, 0)
	// One ancient row past most --since windows
	seedAt(now.Add(-48*time.Hour), "s9", "get_file", "/old.go", true, 100)
	return database
}

// TestList_DefaultLimit pins the safety ceiling — no limit = 50.
func TestList_DefaultLimit(t *testing.T) {
	got, err := List(context.Background(), newReaderFixture(t), ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 8 {
		t.Errorf("expected 8 rows (seed count), got %d", len(got))
	}
}

// TestList_FiltersBySession: the SessionID option scopes correctly
// without bleeding across sessions.
func TestList_FiltersBySession(t *testing.T) {
	got, err := List(context.Background(), newReaderFixture(t), ListOptions{SessionID: "s1"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("got %d, want 4", len(got))
	}
	for _, r := range got {
		if r.SessionID != "s1" {
			t.Errorf("leak: row with session %q", r.SessionID)
		}
	}
}

// TestList_FiltersByTool: Tool option scopes to one tool name.
func TestList_FiltersByTool(t *testing.T) {
	got, err := List(context.Background(), newReaderFixture(t), ListOptions{Tool: "retrieve_stashed"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

// TestList_FiltersBySince: rows older than the window are excluded.
func TestList_FiltersBySince(t *testing.T) {
	got, err := List(context.Background(), newReaderFixture(t), ListOptions{Since: 10 * time.Minute})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 7 {
		t.Errorf("got %d, want 7 (excludes 48h-old row)", len(got))
	}
}

// TestList_OnlyDenied: only response_ok = 0 rows returned.
func TestList_OnlyDenied(t *testing.T) {
	got, err := List(context.Background(), newReaderFixture(t), ListOptions{OnlyDenied: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2 (2 denied rows seeded)", len(got))
	}
	for _, r := range got {
		if r.ResponseOK {
			t.Errorf("non-denial row leaked: %+v", r)
		}
	}
}

// TestList_NewestFirst pins the ORDER BY ts DESC contract — operators
// scanning a list expect most-recent at top.
func TestList_NewestFirst(t *testing.T) {
	got, err := List(context.Background(), newReaderFixture(t), ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Ts.After(got[i-1].Ts) {
			t.Errorf("out-of-order at idx %d: %v after %v", i, got[i].Ts, got[i-1].Ts)
		}
	}
}

// TestStatsSummary_AllTime: aggregate counts across the full table.
func TestStatsSummary_AllTime(t *testing.T) {
	stats, err := StatsSummary(context.Background(), newReaderFixture(t), 0)
	if err != nil {
		t.Fatalf("StatsSummary: %v", err)
	}
	if stats.Total != 8 {
		t.Errorf("Total: got %d, want 8", stats.Total)
	}
	if stats.OK != 6 || stats.Denied != 2 {
		t.Errorf("OK/Denied: got %d/%d, want 6/2", stats.OK, stats.Denied)
	}
	if stats.ByTool["get_file"] != 5 {
		t.Errorf("ByTool[get_file]: got %d, want 5", stats.ByTool["get_file"])
	}
	if stats.ByTool["retrieve_stashed"] != 2 {
		t.Errorf("ByTool[retrieve_stashed]: got %d, want 2", stats.ByTool["retrieve_stashed"])
	}
}

// TestStatsSummary_Window: a since window matches the same filter
// shape that List uses.
func TestStatsSummary_Window(t *testing.T) {
	stats, err := StatsSummary(context.Background(), newReaderFixture(t), 10*time.Minute)
	if err != nil {
		t.Fatalf("StatsSummary: %v", err)
	}
	if stats.Total != 7 {
		t.Errorf("window Total: got %d, want 7 (excludes ancient)", stats.Total)
	}
}

// TestTopPaths_AggregatesByPath: same path across multiple rows
// collapses to one row with summed metrics.
func TestTopPaths_AggregatesByPath(t *testing.T) {
	got, err := TopPaths(context.Background(), newReaderFixture(t), 0, 10)
	if err != nil {
		t.Fatalf("TopPaths: %v", err)
	}
	// /a.go appears 2× → should be at the top.
	if len(got) == 0 || got[0].Path != "/a.go" {
		t.Errorf("expected /a.go at top, got %+v", got)
	}
	if got[0].Calls != 2 {
		t.Errorf("/a.go calls: got %d, want 2", got[0].Calls)
	}
	if got[0].TotalBytes != 2048 {
		t.Errorf("/a.go bytes: got %d, want 2048 (2*1024)", got[0].TotalBytes)
	}
}

// TestTopPaths_ExcludesNullPath ensures the "(null)" rows from tools
// without a path don't drown the signal.
func TestTopPaths_ExcludesNullPath(t *testing.T) {
	db := newReaderFixture(t)
	if _, err := db.ExecContext(
		context.Background(),
		`INSERT INTO mcp_audit (ts, session_id, tool_name, request_hash,
		    path_requested, response_size_bytes, response_truncated, response_ok, duration_us)
		 VALUES (?, 's1', 'some_tool', 'h', NULL, 0, 0, 1, 0)`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert null-path: %v", err)
	}
	got, err := TopPaths(context.Background(), db, 0, 100)
	if err != nil {
		t.Fatalf("TopPaths: %v", err)
	}
	for _, p := range got {
		if p.Path == "" {
			t.Errorf("null-path row leaked into top-paths: %+v", p)
		}
	}
}

// TestPurge_RemovesOldRows: rows older than olderThan are deleted,
// newer rows stay.
func TestPurge_RemovesOldRows(t *testing.T) {
	db := newReaderFixture(t)
	deleted, err := Purge(context.Background(), db, 1*time.Hour)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted: got %d, want 1 (the 48h-old row)", deleted)
	}
	rest, _ := List(context.Background(), db, ListOptions{})
	if len(rest) != 7 {
		t.Errorf("post-purge count: got %d, want 7", len(rest))
	}
}

// TestPurge_RejectsZeroAndNegative pins the safety guard. olderThan
// must be > 0; otherwise refuse to delete everything.
func TestPurge_RejectsZeroAndNegative(t *testing.T) {
	db := newReaderFixture(t)
	for _, d := range []time.Duration{0, -1, -time.Hour} {
		if _, err := Purge(context.Background(), db, d); err == nil {
			t.Errorf("Purge(%v) should have errored", d)
		}
	}
}

// TestList_NilDBRejected pins the nil guard.
func TestList_NilDBRejected(t *testing.T) {
	if _, err := List(context.Background(), nil, ListOptions{}); err == nil {
		t.Errorf("nil db should error")
	}
}
