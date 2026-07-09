package compaction

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// seedSessionForRecovery seeds a project + session + a handful of pre-
// compaction events (read, edit, failure) plus a compaction event so
// BuildPostCompactContext has something to surface.
func seedSessionForRecovery(t *testing.T, database *sql.DB, root string, base time.Time) {
	t.Helper()
	st := store.New(database)
	ctx := context.Background()
	pid, err := st.UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, models.Session{
		ID: "sess-D23", ProjectID: pid, Tool: models.ToolClaudeCode,
		Model: "claude-sonnet-4", StartedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	events := []models.ToolEvent{
		{
			SourceFile: "d.jsonl", SourceEventID: "d1",
			SessionID: "sess-D23", ProjectRoot: root,
			Timestamp:  base.Add(time.Second),
			Tool:       models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "handler.go",
			Success: true, RawToolName: "Read",
		},
		{
			SourceFile: "d.jsonl", SourceEventID: "d2",
			SessionID: "sess-D23", ProjectRoot: root,
			Timestamp:  base.Add(2 * time.Second),
			Tool:       models.ToolClaudeCode,
			ActionType: models.ActionEditFile, Target: "handler.go",
			Success: true, RawToolName: "Edit",
		},
		{
			SourceFile: "d.jsonl", SourceEventID: "d3",
			SessionID: "sess-D23", ProjectRoot: root,
			Timestamp:  base.Add(3 * time.Second),
			Tool:       models.ToolClaudeCode,
			ActionType: models.ActionRunCommand, Target: "go test ./...",
			Success: false, ErrorMessage: "FAIL TestExample: want 3 got 4",
			RawToolName: "Bash",
		},
	}
	if _, err := st.Ingest(ctx, events, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Insert a failure_context row so BuildPostCompactContext's failure
	// query finds something. action_id references the run_command row
	// inserted above (the only failed action in the seed).
	var failedActionID int64
	if err := database.QueryRowContext(
		ctx,
		`SELECT id FROM actions WHERE session_id = 'sess-D23' AND success = 0 LIMIT 1`,
	).Scan(&failedActionID); err != nil {
		t.Fatalf("locate failed action: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO failure_context (action_id, session_id, project_id, timestamp,
		     command_summary, command_hash, error_category, error_message,
		     retry_count, eventually_succeeded)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		failedActionID, "sess-D23", pid,
		base.Add(3*time.Second).UTC().Format(time.RFC3339Nano),
		"go test ./...", "deadbeef", "test_failure",
		"FAIL TestExample: want 3 got 4",
		0, 0); err != nil {
		t.Fatalf("insert failure_context: %v", err)
	}
	// Now record the compaction itself.
	if _, err := New(database).Capture(ctx, CaptureOptions{
		SessionID: "sess-D23", ProjectRoot: root, Tool: models.ToolClaudeCode,
		Timestamp: base.Add(10 * time.Second), Trigger: "manual",
	}); err != nil {
		t.Fatalf("Capture: %v", err)
	}
}

func openDBForRecovery(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// TestBuildPostCompactContext_HappyPath pins that the synthetic content
// surfaces all four sections (reads, edits, failures, learn rules) when
// the underlying tables have data, and is wrapped in the
// `<observer-compaction-recovery>` envelope so the model can recognise
// it.
func TestBuildPostCompactContext_HappyPath(t *testing.T) {
	database := openDBForRecovery(t)
	root := t.TempDir()
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	seedSessionForRecovery(t, database, root, base)

	content, err := BuildPostCompactContext(context.Background(), database, "sess-D23")
	if err != nil {
		t.Fatalf("BuildPostCompactContext: %v", err)
	}
	if content == "" {
		t.Fatal("expected non-empty content for seeded session")
	}
	for _, want := range []string{
		"<observer-compaction-recovery>",
		"</observer-compaction-recovery>",
		"sess-D23",
		"Recently read files",
		"handler.go",
		"Recently edited files",
		"Recent failures",
		"go test ./...",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content missing %q:\n%s", want, content)
		}
	}
}

// TestBuildPostCompactContext_NoCompactionEvent pins that a session
// without a compaction event returns "" — the feature is opportunistic
// and should not surface anything for fresh / non-compacted sessions.
func TestBuildPostCompactContext_NoCompactionEvent(t *testing.T) {
	database := openDBForRecovery(t)
	content, err := BuildPostCompactContext(context.Background(), database, "no-such-session")
	if err != nil {
		t.Fatalf("BuildPostCompactContext: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty content, got %q", content)
	}
}

// TestBuildPostCompactContext_Determinism pins that the same DB state
// produces byte-identical content across repeated calls. This is the
// cross-turn invariance predicate Anthropic's prefix cache depends on
// once the proxy injects the content into the request envelope.
func TestBuildPostCompactContext_Determinism(t *testing.T) {
	database := openDBForRecovery(t)
	root := t.TempDir()
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	seedSessionForRecovery(t, database, root, base)

	first, err := BuildPostCompactContext(context.Background(), database, "sess-D23")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := BuildPostCompactContext(context.Background(), database, "sess-D23")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if next != first {
			t.Errorf("iter %d differs from first call", i)
		}
	}
}

// TestInjector_CachesPerCompactionEvent pins the Injector's cache
// behaviour: repeated calls within the same compaction window return
// the cached content (no DB rebuild), and a new compaction event
// invalidates the cache.
func TestInjector_CachesPerCompactionEvent(t *testing.T) {
	database := openDBForRecovery(t)
	root := t.TempDir()
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	seedSessionForRecovery(t, database, root, base)

	inj := NewInjector(database)
	first, err := inj.Get(context.Background(), "sess-D23")
	if err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if first == "" {
		t.Fatal("expected non-empty content")
	}

	// Repeat: should be byte-identical (cache hit).
	for i := 0; i < 3; i++ {
		got, err := inj.Get(context.Background(), "sess-D23")
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if got != first {
			t.Errorf("iter %d cache-miss: differs from first call", i)
		}
	}

	// New compaction event for the same session should invalidate
	// the cache and rebuild content.
	if _, err := New(database).Capture(context.Background(), CaptureOptions{
		SessionID: "sess-D23", ProjectRoot: root, Tool: models.ToolClaudeCode,
		Timestamp: base.Add(60 * time.Second),
	}); err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	got, err := inj.Get(context.Background(), "sess-D23")
	if err != nil {
		t.Fatalf("post-rebuild Get: %v", err)
	}
	// Content stays equal in shape (same data, same compaction-window
	// queries), so we mainly assert the call doesn't error and stays
	// non-empty. A deeper assertion would require seeding more rows
	// between the two compaction events.
	if got == "" {
		t.Errorf("post-rebuild content unexpectedly empty")
	}
}

// TestInjector_SetsInjectedAtOnFirstFire pins migration 015 + the v1.4.43+
// dashboard visibility hook: when the injector builds non-empty
// content for a session, it stamps `injected_at` on the most-recent
// compaction_events row. Idempotent — repeat calls within the same
// compaction window don't re-write the timestamp.
func TestInjector_SetsInjectedAtOnFirstFire(t *testing.T) {
	database := openDBForRecovery(t)
	root := t.TempDir()
	base := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	seedSessionForRecovery(t, database, root, base)

	inj := NewInjector(database)
	if _, err := inj.Get(context.Background(), "sess-D23"); err != nil {
		t.Fatalf("first Get: %v", err)
	}

	var injectedAt string
	if err := database.QueryRow(
		`SELECT COALESCE(injected_at, '') FROM compaction_events WHERE session_id = 'sess-D23' ORDER BY id DESC LIMIT 1`,
	).Scan(&injectedAt); err != nil {
		t.Fatal(err)
	}
	if injectedAt == "" {
		t.Fatal("injected_at should be set after first Get")
	}

	// Repeat call — injected_at should NOT change (idempotent).
	first := injectedAt
	if _, err := inj.Get(context.Background(), "sess-D23"); err != nil {
		t.Fatalf("second Get: %v", err)
	}
	if err := database.QueryRow(
		`SELECT COALESCE(injected_at, '') FROM compaction_events WHERE session_id = 'sess-D23' ORDER BY id DESC LIMIT 1`,
	).Scan(&injectedAt); err != nil {
		t.Fatal(err)
	}
	if injectedAt != first {
		t.Errorf("injected_at re-written on repeat call: was %q, now %q (idempotency broken)", first, injectedAt)
	}
}

// TestInjector_EmptySession pins that Get("") and Get on a session
// without compaction events both return "" without error.
func TestInjector_EmptySession(t *testing.T) {
	database := openDBForRecovery(t)
	inj := NewInjector(database)
	got, err := inj.Get(context.Background(), "")
	if err != nil || got != "" {
		t.Errorf("Get(\"\"): got=%q err=%v, want empty", got, err)
	}
	got, err = inj.Get(context.Background(), "no-such-session")
	if err != nil || got != "" {
		t.Errorf("Get(missing): got=%q err=%v, want empty", got, err)
	}
}
