package retention

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// TestShrinkToCap_SourceNeverIssuesBareVacuum is a source-level guard for
// T0.1 of the 2026-08-26 disk/compute remediation plan: shrinkToCap's
// function BODY (not its doc comment, which is allowed to talk about
// VACUUM in prose) must never contain the literal SQL statement `VACUUM`,
// and must never contain a loop. Both are exactly the shape of the
// 2026-08-26 incident — a bare `VACUUM` re-issued up to 12x per retention
// pass, rewriting a ~40GB DB file to a /var/tmp temp copy on every
// startup and every 24h tick. This test pins the fix at the source level
// so a future edit can't silently reintroduce either.
func TestShrinkToCap_SourceNeverIssuesBareVacuum(t *testing.T) {
	const path = "retention.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Parse WITHOUT comments: shrinkToCap's doc comment (and inline
	// comments in its body) legitimately discuss VACUUM in prose — only
	// the compiled statement shape matters here. go/printer renders an
	// ast.Node without attaching any comments unless given an explicit
	// ast.CommentedNode, so re-printing the extracted body strips them.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "shrinkToCap" {
			continue
		}
		body = fn.Body
	}
	if body == nil {
		t.Fatal("shrinkToCap function not found in retention.go")
	}

	var buf bytes.Buffer
	if err := printer.Fprint(&buf, fset, body); err != nil {
		t.Fatalf("re-print shrinkToCap body: %v", err)
	}
	bodySrc := buf.String()

	if strings.Contains(bodySrc, "VACUUM") {
		t.Errorf("shrinkToCap body contains the literal string VACUUM — the automatic "+
			"size-cap path must never issue a bare full VACUUM:\n%s", bodySrc)
	}
	// A for-loop is exactly the shape of the pre-fix 12x VACUUM loop; the
	// automatic size-cap path must perform at most one bounded pass.
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.ForStmt); ok {
			t.Errorf("shrinkToCap body contains a for-loop — the automatic size-cap "+
				"path must perform at most one bounded pass, never loop:\n%s", bodySrc)
		}
		if _, ok := n.(*ast.RangeStmt); ok {
			t.Errorf("shrinkToCap body contains a range-loop — the automatic size-cap "+
				"path must perform at most one bounded pass, never loop:\n%s", bodySrc)
		}
		return true
	})
}

// TestShrinkToCap_SinglePassShedsThenFlagsUnreachableCap pins the
// behavioral half of T0.1/T0.4: on a DB whose oldest `actions` row IS
// older than the sizeCapActionFloorDays keep-floor (so the shed pass
// actually runs, unlike TestShrinkToCap_FloorProtectsRecentActions where
// the floor guard short-circuits before any deletion), shrinkToCap sheds
// the old actions in ONE pass, issues no VACUUM (so the file — dominated
// by a non-actions token_usage bulk — does not shrink), and sets
// SizeCapUnmet rather than looping to chase the unreachable cap.
func TestShrinkToCap_SinglePassShedsThenFlagsUnreachableCap(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "big.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	var projectID int64
	if err := d.QueryRowContext(ctx,
		`INSERT INTO projects (root_path, created_at) VALUES ('/r', '2026-01-01T00:00:00Z') RETURNING id`).
		Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx,
		`INSERT INTO sessions (id, tool, project_id, started_at) VALUES ('s1','claude-code',?,?)`,
		projectID, fakeNow.Add(-90*24*time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// A handful of OLD actions — outside the 30-day keep-floor relative
	// to fakeNow, so the shed pass proceeds instead of short-circuiting.
	oldTS := fakeNow.Add(-90 * 24 * time.Hour).Format(time.RFC3339Nano)
	for i := 0; i < 5; i++ {
		if _, err := d.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
			 VALUES ('s1',?,?,'run_command','claude-code','f',?)`,
			projectID, oldTS, makeID(i)); err != nil {
			t.Fatal(err)
		}
	}
	// Inflate token_usage (the non-actions bulk) past a 1MB cap — same
	// pattern as TestShrinkToCap_FloorProtectsRecentActions.
	pad := make([]byte, 1200)
	for i := range pad {
		pad[i] = 'x'
	}
	padStr := string(pad)
	for i := 0; i < 1500; i++ {
		if _, err := d.ExecContext(ctx,
			`INSERT INTO token_usage (session_id, timestamp, tool, source, source_file) VALUES ('s1',?, 'claude-code','jsonl',?)`,
			oldTS, padStr); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = d.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	_, _ = d.ExecContext(ctx, `VACUUM`) // test-setup only: establish a known baseline file size

	sizeBefore := sizeOf(dbPath)

	p := New(d)
	res, err := p.Run(ctx, Options{MaxDBSizeMB: 1, DBPath: dbPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.SizePassesRun > 1 {
		t.Errorf("SizePassesRun = %d, want at most 1 (single bounded pass, never a loop)", res.SizePassesRun)
	}
	if !res.SizeCapUnmet {
		t.Errorf("SizeCapUnmet should be set — the bulk is in token_usage, unreachable by shedding actions alone")
	}

	// The shed pass DID run (proving it wasn't short-circuited by the
	// keep-floor guard): all 5 old actions fell inside the single 30-day
	// shed window and were deleted.
	var n int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("actions remaining = %d, want 0 (the single shed pass should have cleared the old window)", n)
	}
	if res.ActionsDeleted != 5 {
		t.Errorf("ActionsDeleted = %d, want 5", res.ActionsDeleted)
	}

	// No VACUUM ran, so the file did not shrink relative to the
	// pre-Run baseline (deleting rows without a VACUUM only frees pages
	// to the internal freelist — it does not release bytes to the OS).
	sizeAfter := sizeOf(dbPath)
	if sizeAfter < sizeBefore {
		t.Errorf("DB file shrank from %d to %d bytes — shrinkToCap must never issue a VACUUM automatically", sizeBefore, sizeAfter)
	}
}
