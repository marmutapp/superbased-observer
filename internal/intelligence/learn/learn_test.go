package learn

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "learn.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

var counter int

func newID() string {
	counter++
	b := []byte{'e'}
	n := counter
	var s []byte
	if n == 0 {
		s = []byte("0")
	}
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return string(append(b, s...))
}

func evt(session, tool, action, target string, ts time.Time, success bool, errMsg string) models.ToolEvent {
	return models.ToolEvent{
		SourceFile: "f-" + session, SourceEventID: newID(),
		SessionID: session, Tool: tool,
		Timestamp: ts, ActionType: action, Target: target,
		Success: success, ErrorMessage: errMsg,
	}
}

func ingest(t *testing.T, database *sql.DB, root string, events []models.ToolEvent) {
	t.Helper()
	for i := range events {
		events[i].ProjectRoot = root
	}
	if _, err := store.New(database).Ingest(context.Background(), events, nil, store.IngestOptions{RecordFailures: true}); err != nil {
		t.Fatal(err)
	}
}

func TestDerive_BasicRecoveryRule(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Session A: go test fails, we edit foo.go, go test succeeds.
	// Session B: same sequence, different edit (bar.go).
	ingest(t, database, root, []models.ToolEvent{
		evt("sA", models.ToolClaudeCode, models.ActionRunCommand, "go test", base, false, "FAIL TestFoo want 1 got 0"),
		evt("sA", models.ToolClaudeCode, models.ActionEditFile, "foo.go", base.Add(time.Second), true, ""),
		evt("sA", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(2*time.Second), true, ""),
		evt("sB", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour), false, "FAIL TestBar want 2 got 3"),
		evt("sB", models.ToolClaudeCode, models.ActionEditFile, "bar.go", base.Add(time.Hour+time.Second), true, ""),
		evt("sB", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour+2*time.Second), true, ""),
	})

	rules, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d: %+v", len(rules), rules)
	}
	r := rules[0]
	if r.FailureCount != 2 || r.RecoveryCount != 2 {
		t.Errorf("counts: %+v", r)
	}
	if len(r.EditedFiles) != 2 {
		t.Errorf("EditedFiles: %v, want foo.go + bar.go", r.EditedFiles)
	}
	if r.CommandSummary != "go test" {
		t.Errorf("CommandSummary: %q", r.CommandSummary)
	}
}

func TestDerive_MinFailuresFilter(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Only one failure → below default MinFailures = 2.
	ingest(t, database, root, []models.ToolEvent{
		evt("s", models.ToolClaudeCode, models.ActionRunCommand, "go build", base, false, "FAIL"),
		evt("s", models.ToolClaudeCode, models.ActionEditFile, "x.go", base.Add(time.Second), true, ""),
		evt("s", models.ToolClaudeCode, models.ActionRunCommand, "go build", base.Add(2*time.Second), true, ""),
	})
	rules, err := New(database).Derive(context.Background(), Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules, got %d", len(rules))
	}
}

func TestDerive_DropsFailuresWithoutRecovery(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Two failures, never succeeds → no rule.
	ingest(t, database, root, []models.ToolEvent{
		evt("s", models.ToolClaudeCode, models.ActionRunCommand, "go test ./bad", base, false, "FAIL"),
		evt("s", models.ToolClaudeCode, models.ActionRunCommand, "go test ./bad", base.Add(time.Minute), false, "FAIL again"),
	})
	rules, err := New(database).Derive(context.Background(), Options{ProjectRoot: root, MinFailures: 1})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules (no recovery), got %d: %+v", len(rules), rules)
	}
}

func TestDerive_ProjectFilter(t *testing.T) {
	database := openDB(t)
	rootA := t.TempDir()
	rootB := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	ingest(t, database, rootA, []models.ToolEvent{
		evt("sA", models.ToolClaudeCode, models.ActionRunCommand, "go test", base, false, "FAIL"),
		evt("sA", models.ToolClaudeCode, models.ActionEditFile, "a.go", base.Add(time.Second), true, ""),
		evt("sA", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(2*time.Second), true, ""),
		evt("sA2", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour), false, "FAIL"),
		evt("sA2", models.ToolClaudeCode, models.ActionEditFile, "a.go", base.Add(time.Hour+time.Second), true, ""),
		evt("sA2", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour+2*time.Second), true, ""),
	})
	ingest(t, database, rootB, []models.ToolEvent{
		evt("sB", models.ToolClaudeCode, models.ActionRunCommand, "go test", base, false, "FAIL"),
		evt("sB", models.ToolClaudeCode, models.ActionEditFile, "b.go", base.Add(time.Second), true, ""),
		evt("sB", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(2*time.Second), true, ""),
		evt("sB2", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour), false, "FAIL"),
		evt("sB2", models.ToolClaudeCode, models.ActionEditFile, "b.go", base.Add(time.Hour+time.Second), true, ""),
		evt("sB2", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour+2*time.Second), true, ""),
	})
	rules, err := New(database).Derive(context.Background(), Options{ProjectRoot: rootA})
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].Project != rootA {
		t.Errorf("project filter: %+v", rules[0])
	}
}
