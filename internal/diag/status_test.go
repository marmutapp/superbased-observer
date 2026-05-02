package diag

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func TestSnapshot_EmptyDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	snap, err := Snapshot(context.Background(), d, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if snap.SchemaVersion == 0 {
		t.Errorf("schema version should be > 0 after migration: %d", snap.SchemaVersion)
	}
	if snap.Counts.Actions != 0 || snap.Counts.Sessions != 0 {
		t.Errorf("expected empty counts: %+v", snap.Counts)
	}
	if !snap.LastActionAt.IsZero() {
		t.Errorf("LastActionAt should be zero on empty DB")
	}
}

func TestSnapshot_PopulatedAndPerToolSummary(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := store.New(d)
	now := time.Now().UTC()
	events := []models.ToolEvent{
		{
			SourceFile: "a.jsonl", SourceEventID: "e1",
			SessionID: "s-claude", ProjectRoot: "/r1",
			Timestamp: now.Add(-30 * time.Minute),
			Tool:      models.ToolClaudeCode, ActionType: models.ActionReadFile,
			Target: "x.go", Success: true, RawToolName: "Read",
		},
		{
			SourceFile: "a.jsonl", SourceEventID: "e2",
			SessionID: "s-claude", ProjectRoot: "/r1",
			Timestamp: now.Add(-20 * time.Minute),
			Tool:      models.ToolClaudeCode, ActionType: models.ActionRunCommand,
			Target: "go test", Success: false, RawToolName: "Bash",
			ErrorMessage: "FAIL",
		},
		{
			SourceFile: "b.jsonl", SourceEventID: "e3",
			SessionID: "s-codex", ProjectRoot: "/r2",
			Timestamp: now.Add(-5 * time.Minute),
			Tool:      models.ToolCodex, ActionType: models.ActionReadFile,
			Target: "y.go", Success: true,
		},
	}
	if _, err := st.Ingest(context.Background(), events, nil, store.IngestOptions{
		RecordFailures: true,
	}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	snap, err := Snapshot(context.Background(), d, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Sessions != 2 {
		t.Errorf("sessions: %d", snap.Counts.Sessions)
	}
	if snap.Counts.Actions != 3 {
		t.Errorf("actions: %d", snap.Counts.Actions)
	}
	if snap.Counts.FailureContext != 1 {
		t.Errorf("failure_context: %d", snap.Counts.FailureContext)
	}
	if snap.LastActionTool != models.ToolCodex {
		t.Errorf("last action tool: %s (want codex — most recent)", snap.LastActionTool)
	}
	tools := map[string]bool{}
	for _, ta := range snap.PerToolLastSeen {
		tools[ta.Tool] = true
	}
	if !tools[models.ToolClaudeCode] || !tools[models.ToolCodex] {
		t.Errorf("per-tool missing entries: %+v", snap.PerToolLastSeen)
	}
}

func TestFormatStatus_RendersAllFields(t *testing.T) {
	snap := StatusSnapshot{
		DBPath:         "/tmp/x.db",
		DBSizeBytes:    2048,
		SchemaVersion:  1,
		Counts:         SnapshotCounts{Projects: 2, Sessions: 5, Actions: 100},
		LastActionAt:   time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC),
		LastActionTool: "claude-code",
		PerToolLastSeen: []ToolActivity{
			{Tool: "claude-code", LastSeenAt: time.Now(), ActionCount: 80},
		},
		RecentFailures24: 3,
	}
	out := FormatStatus(snap)
	for _, want := range []string{"/tmp/x.db", "schema v1", "Projects:", "Failures (24h):   3", "claude-code"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{500, "500 B"},
		{2048, "2.0 KB"},
		{2 * 1024 * 1024, "2.0 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q want %q", c.in, got, c.want)
		}
	}
}

// dummy import keeper — bytes used in main file but not here
var _ = bytes.Buffer{}
