package indexing

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
)

func seedDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "idx.db")
	d, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO projects (root_path, created_at) VALUES ('/tmp/idx', ?)`, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('s', 1, ?, ?)`,
		models.ToolClaudeCode, ts); err != nil {
		t.Fatal(err)
	}
	return d
}

func seedAction(t *testing.T, d *sql.DB, id int64, target string) {
	t.Helper()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO actions (id, session_id, project_id, timestamp, action_type, target, tool,
		 source_file, source_event_id, success) VALUES (?, 's', 1, ?, ?, ?, ?, ?, ?, 1)`,
		id, ts, models.ActionRunCommand, target, models.ToolClaudeCode,
		"f.jsonl", "evt-"+target,
	); err != nil {
		t.Fatal(err)
	}
}

func TestTrimExcerpt(t *testing.T) {
	t.Parallel()
	if got := TrimExcerpt("hello", 100); got != "hello" {
		t.Errorf("short: got %q", got)
	}
	long := strings.Repeat("a", 400) + "MIDDLE" + strings.Repeat("b", 400)
	got := TrimExcerpt(long, 200)
	if len(got) > 200 {
		t.Errorf("len %d > 200", len(got))
	}
	if !strings.Contains(got, "…[truncated]…") {
		t.Error("missing truncation marker")
	}
	if !strings.HasPrefix(got, "a") || !strings.HasSuffix(got, "b") {
		t.Errorf("head/tail not preserved: %q", got)
	}
}

func TestIndexAndSearch(t *testing.T) {
	t.Parallel()
	d := seedDB(t)
	ctx := context.Background()
	seedAction(t, d, 1, "go test ./...")
	seedAction(t, d, 2, "go build ./...")
	seedAction(t, d, 3, "ls -la")

	idx := New(d, 2048)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	must(idx.Index(ctx, 1, "Bash", "go test ./...",
		"panic: nil pointer in checkout handler\nSome trailing context", ""))
	must(idx.Index(ctx, 2, "Bash", "go build ./...",
		"ok github.com/example/foo 0.012s", ""))
	must(idx.Index(ctx, 3, "Bash", "ls -la",
		"total 4\ndrwxr-xr-x 2 user user", ""))

	res, err := idx.Search(ctx, "checkout", 10)
	must(err)
	if len(res) != 1 || res[0].ActionID != 1 {
		t.Errorf("search 'checkout': %+v want single result for action 1", res)
	}
	if !strings.Contains(res[0].Excerpt, "checkout") {
		t.Errorf("excerpt missing keyword: %q", res[0].Excerpt)
	}

	res, err = idx.Search(ctx, "pointer OR banana", 10)
	must(err)
	if len(res) == 0 {
		t.Fatal("expected match for 'pointer OR banana'")
	}

	// Re-index action 1 → the prior row should be replaced.
	must(idx.Index(ctx, 1, "Bash", "go test ./...",
		"completely different text with keyword banana", ""))
	var n int
	_ = d.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM action_excerpts WHERE action_id = 1`).Scan(&n)
	if n != 1 {
		t.Errorf("re-index produced %d rows, want 1", n)
	}
	res, _ = idx.Search(ctx, "banana", 10)
	if len(res) != 1 || res[0].ActionID != 1 {
		t.Errorf("re-indexed content not searchable: %+v", res)
	}
	res, _ = idx.Search(ctx, "checkout", 10)
	if len(res) != 0 {
		t.Errorf("old content still searchable after re-index: %+v", res)
	}
}

func TestIndexRequiresActionID(t *testing.T) {
	t.Parallel()
	d := seedDB(t)
	idx := New(d, 0)
	if err := idx.Index(context.Background(), 0, "Bash", "x", "y", ""); err == nil {
		t.Fatal("expected error for actionID=0")
	}
}
