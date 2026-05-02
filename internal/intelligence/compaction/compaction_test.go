package compaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func seed(t *testing.T, database *sql.DB, root string, base time.Time) {
	t.Helper()
	ctx := context.Background()
	pid, err := store.New(database).UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = pid
	if err := store.New(database).UpsertSession(ctx, models.Session{
		ID: "s1", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	// A couple of file_state rows so the snapshot has content.
	rows := []struct {
		path string
		size int64
	}{
		{"a.go", 100},
		{"b.go", 200},
	}
	for _, r := range rows {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO file_state (project_id, file_path, content_hash, file_mtime,
				file_size_bytes, last_action_type, last_seen_at)
			 VALUES (?, ?, 'h', ?, ?, 'read_file', ?)`,
			pid, r.path, base.Format(time.RFC3339Nano), r.size, base.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCapture_WritesSnapshot(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	seed(t, database, root, base)

	id, err := New(database).Capture(context.Background(), CaptureOptions{
		SessionID: "s1", ProjectRoot: root, Tool: models.ToolClaudeCode,
		Timestamp: base.Add(time.Minute), Trigger: "manual", PreActionCount: 42,
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if id == 0 {
		t.Errorf("Capture returned id=0")
	}
	var snap string
	if err := database.QueryRowContext(context.Background(),
		`SELECT file_state_snapshot FROM compaction_events WHERE id = ?`, id).Scan(&snap); err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	if err := json.Unmarshal([]byte(snap), &s); err != nil {
		t.Fatal(err)
	}
	if s.FileCount != 2 {
		t.Errorf("FileCount: %d", s.FileCount)
	}
	if _, ok := s.Files["a.go"]; !ok {
		t.Errorf("missing a.go: %+v", s)
	}
}

func TestReconcile_MarksGhostFiles(t *testing.T) {
	database := openDB(t)
	root := t.TempDir()
	base := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	seed(t, database, root, base)

	_, err := New(database).Capture(context.Background(), CaptureOptions{
		SessionID: "s1", ProjectRoot: root, Tool: models.ToolClaudeCode,
		Timestamp: base.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Post-compaction, the session only re-reads a.go, not b.go → b.go
	// becomes a ghost file.
	if _, err := store.New(database).Ingest(context.Background(), []models.ToolEvent{
		{SourceFile: "f", SourceEventID: "p1", SessionID: "s1", ProjectRoot: root,
			Timestamp: base.Add(time.Minute + time.Second),
			Tool:      models.ToolClaudeCode, ActionType: models.ActionReadFile,
			Target: "a.go", Success: true},
	}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := New(database).Reconcile(context.Background(), ReconcileOptions{
		SessionID: "s1",
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var ghosts string
	if err := database.QueryRowContext(context.Background(),
		`SELECT ghost_files_after FROM compaction_events WHERE session_id = 's1'`).Scan(&ghosts); err != nil {
		t.Fatal(err)
	}
	var list []string
	if err := json.Unmarshal([]byte(ghosts), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != "b.go" {
		t.Errorf("ghosts: %v, want [b.go]", list)
	}
}

func TestReconcile_NoOpWithoutEvent(t *testing.T) {
	database := openDB(t)
	if err := New(database).Reconcile(context.Background(), ReconcileOptions{SessionID: "no-such"}); err != nil {
		t.Errorf("Reconcile should be a no-op: %v", err)
	}
}
