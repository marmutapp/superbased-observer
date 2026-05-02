package suggest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/intelligence/learn"
	"github.com/marmutapp/superbased-observer/internal/intelligence/patterns"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sg.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

var idCount int

func nextID() string {
	idCount++
	var n = idCount
	var s []byte
	for n > 0 {
		s = append([]byte{byte('0' + n%10)}, s...)
		n /= 10
	}
	return "e" + string(s)
}

func evt(session, tool, action, target string, ts time.Time, success bool) models.ToolEvent {
	return models.ToolEvent{
		SourceFile: "f-" + session, SourceEventID: nextID(),
		SessionID: session, Tool: tool,
		Timestamp: ts, ActionType: action, Target: target, Success: success,
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

func TestLoad_AssemblesPatternsAndRules(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	// Build data: hot file, common command, edit→test, failure→recovery.
	ingest(t, database, root, []models.ToolEvent{
		evt("s1", models.ToolClaudeCode, models.ActionReadFile, "hot.go", base, true),
		evt("s1", models.ToolClaudeCode, models.ActionEditFile, "hot.go", base.Add(time.Second), true),
		evt("s1", models.ToolClaudeCode, models.ActionReadFile, "hot.go", base.Add(2*time.Second), true),
		evt("s1", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(3*time.Second), false),
		evt("s1", models.ToolClaudeCode, models.ActionEditFile, "hot.go", base.Add(4*time.Second), true),
		evt("s1", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(5*time.Second), true),
		evt("s2", models.ToolClaudeCode, models.ActionReadFile, "hot.go", base.Add(time.Hour), true),
		evt("s2", models.ToolClaudeCode, models.ActionEditFile, "hot.go", base.Add(time.Hour+time.Second), true),
		evt("s2", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour+2*time.Second), false),
		evt("s2", models.ToolClaudeCode, models.ActionEditFile, "hot.go", base.Add(time.Hour+3*time.Second), true),
		evt("s2", models.ToolClaudeCode, models.ActionRunCommand, "go test", base.Add(time.Hour+4*time.Second), true),
	})
	// Derive patterns so project_patterns has rows.
	if _, err := patterns.New(database).Derive(context.Background(), patterns.Options{
		ProjectRoot: root,
	}); err != nil {
		t.Fatalf("patterns: %v", err)
	}

	in, err := Load(context.Background(), database, Options{ProjectRoot: root})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(in.HotFiles) == 0 {
		t.Errorf("HotFiles empty: %+v", in)
	}
	if len(in.CommonCommands) == 0 {
		t.Errorf("CommonCommands empty: %+v", in)
	}
	if len(in.Rules) != 1 {
		t.Errorf("Rules: got %d, want 1", len(in.Rules))
	}
}

func TestRenderMarkdown_Shape(t *testing.T) {
	in := Input{
		HotFiles: []patternRow{
			{Key: "main.go", Detail: "10 read, 3 edit", Confidence: 1, Count: 13},
		},
		CommonCommands: []patternRow{
			{Key: "go test ./...", Detail: "5 runs, 80% success", Confidence: 1, Count: 5},
		},
		Rules: []learn.Rule{
			{CommandSummary: "go test", ErrorCategory: "test_failure",
				FailureCount: 2, RecoveryCount: 2, EditedFiles: []string{"x.go"}},
		},
	}
	body := RenderMarkdown(in, time.Date(2026, 4, 16, 0, 0, 0, 0, time.UTC))
	for _, want := range []string{
		"Project context",
		"Hot files",
		"main.go",
		"Common commands",
		"go test ./...",
		"Known command corrections",
		"`x.go`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestApply_IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(path, []byte("# Header\n\nhello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := "## Project context\n\nFoo"
	changed, err := Apply(path, body)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Errorf("first apply should change")
	}
	changed, err = Apply(path, body)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("idempotent apply should not change")
	}
	// Verify pre-existing content survived.
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "# Header") {
		t.Errorf("header lost: %s", got)
	}
}

func TestApply_CoexistsWithLearnBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	// Seed with a learn-managed block.
	learnBody := "## Known corrections\n\n- foo"
	if _, err := learn.Apply(path, learnBody); err != nil {
		t.Fatal(err)
	}
	// Now write a suggest block.
	if _, err := Apply(path, "## Suggest body\n\n- bar"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), learn.ManagedBlockStart) {
		t.Errorf("learn marker lost: %s", got)
	}
	if !strings.Contains(string(got), ManagedBlockStart) {
		t.Errorf("suggest marker missing: %s", got)
	}
	if !strings.Contains(string(got), "foo") || !strings.Contains(string(got), "bar") {
		t.Errorf("content missing: %s", got)
	}
}
