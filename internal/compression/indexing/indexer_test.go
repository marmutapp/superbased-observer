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
	if _, err := d.ExecContext(
		context.Background(),
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

// TestSearchWithSnippets pins the P6.2 surface: the snippet column
// carries the match region bracketed by the caller's sentinel marks,
// and plain-Search semantics (sanitization, ordering) are shared.
func TestSearchWithSnippets(t *testing.T) {
	t.Parallel()
	d := seedDB(t)
	ctx := context.Background()
	seedAction(t, d, 11, "npm test")
	seedAction(t, d, 12, "npm run lint")

	idx := New(d, 2048)
	if err := idx.Index(ctx, 11, "Bash", "npm test",
		strings.Repeat("setup line\n", 40)+"FAIL src/auth.test.ts deadline exceeded\n"+strings.Repeat("teardown line\n", 40), ""); err != nil {
		t.Fatal(err)
	}
	if err := idx.Index(ctx, 12, "Bash", "npm run lint", "all clean", ""); err != nil {
		t.Fatal(err)
	}

	hits, err := idx.SearchWithSnippets(ctx, "deadline", 10, "\x01", "\x02")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ActionID != 11 {
		t.Fatalf("hits: %+v", hits)
	}
	snip := hits[0].Snippet
	if !strings.Contains(snip, "\x01deadline\x02") {
		t.Errorf("snippet missing marked match: %q", snip)
	}
	if len(snip) >= len(hits[0].Excerpt) {
		t.Errorf("snippet should be the match region, not the whole excerpt (%d vs %d bytes)", len(snip), len(hits[0].Excerpt))
	}

	// Special-char single tokens get the same auto-quoting as Search.
	if _, err := idx.SearchWithSnippets(ctx, "auth.test.ts", 10, "\x01", "\x02"); err != nil {
		t.Errorf("sanitized query errored: %v", err)
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

// TestSanitizeFTSQuery_DottedTokensWrapped pins the load-bearing fix:
// a single-token query like `app.set` (which trips FTS5's tokenizer
// with `syntax error near "."`) gets auto-wrapped in double quotes
// so the search succeeds.
func TestSanitizeFTSQuery_DottedTokensWrapped(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dotted token", "app.set", `"app.set"`},
		{"colon token", "ns:key", `"ns:key"`},
		{"parens", "f(x)", `"f(x)"`},
		{"hyphen", "wrap-around", `"wrap-around"`},
		{"plain word — passthrough", "checkout", "checkout"},
		{"already quoted — passthrough", `"app.set"`, `"app.set"`},
		{"multi-token boolean — passthrough", "pointer OR banana", "pointer OR banana"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeFTSQuery(tc.in); got != tc.want {
				t.Errorf("sanitizeFTSQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSearch_DottedQueryNoLongerErrors is the integration-level pin
// against the FTS5 syntax error we hit in dogfood (search_past_outputs
// query="app.set" → SQL logic error). Post-fix the query is auto-
// quoted and FTS5 treats it as a literal phrase.
func TestSearch_DottedQueryNoLongerErrors(t *testing.T) {
	t.Parallel()
	d := seedDB(t)
	ctx := context.Background()
	seedAction(t, d, 1, "grep app.set")
	idx := New(d, 0)
	if err := idx.Index(ctx, 1, "Bash", "grep app.set",
		"app.set('foo', 'bar') called twice", ""); err != nil {
		t.Fatal(err)
	}
	// Pre-fix this returned `SQL logic error: fts5: syntax error near "."`
	res, err := idx.Search(ctx, "app.set", 10)
	if err != nil {
		t.Fatalf("search 'app.set' should succeed post-fix; got %v", err)
	}
	if len(res) != 1 {
		t.Errorf("expected 1 hit, got %d", len(res))
	}
}
