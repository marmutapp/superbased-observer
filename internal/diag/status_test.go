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
	if snap.Counts.CacheEvents != 0 {
		t.Errorf("cache_events empty-DB count: want 0 got %d", snap.Counts.CacheEvents)
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
	// Live badge: only s-codex has activity inside the 15-minute live
	// window (its action is 5m old; s-claude's newest is 20m old).
	if snap.Counts.LiveSessions != 1 {
		t.Errorf("live_sessions: want 1 got %d", snap.Counts.LiveSessions)
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

// TestSnapshot_CountsCacheEvents pins the migration-036 surface — a
// fresh-but-populated cache_events table should be counted by the
// snapshot so the sidebar badge has a non-zero number to render.
func TestSnapshot_CountsCacheEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.ExecContext(
		context.Background(), `
		INSERT INTO cache_events (session_id, tier, timestamp, model, kind, tokens_read, tokens_written)
		VALUES (?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?)`,
		"s-1", "tier1", "2026-06-09T12:00:00Z", "claude-opus-4-7", "hit", 1024, 0,
		"s-1", "tier1", "2026-06-09T12:00:30Z", "claude-opus-4-7", "write", 0, 4096,
	); err != nil {
		t.Fatalf("seed cache_events: %v", err)
	}

	snap, err := Snapshot(context.Background(), d, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.CacheEvents != 2 {
		t.Errorf("cache_events count: want 2 got %d", snap.Counts.CacheEvents)
	}
}

// TestSnapshot_CountsGuardAndRouterRows pins the migration-040/041
// surfaces — guard verdicts and router decisions feed the sidebar
// "Security" and "Routing" badges the same way cache_events feeds
// "Cache".
func TestSnapshot_CountsGuardAndRouterRows(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.ExecContext(
		context.Background(), `
		INSERT INTO guard_events (ts, rule_id, severity, decision, enforced, chain_prev, chain_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"2026-06-12T10:00:00Z", "R-101", "warn", "flag", 0, "", "deadbeef",
	); err != nil {
		t.Fatalf("seed guard_events: %v", err)
	}
	if _, err := d.ExecContext(
		context.Background(), `
		INSERT INTO router_decisions (ts, mode, channel, original_model, selected_model, turn_kind, policy_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?)`,
		"2026-06-12T10:00:00Z", "advise", "proxy", "claude-opus-4-8", "claude-haiku-4-5", "read_only", "h1",
		"2026-06-12T10:01:00Z", "advise", "proxy", "claude-opus-4-8", "claude-opus-4-8", "code_edit", "h1",
	); err != nil {
		t.Fatalf("seed router_decisions: %v", err)
	}

	snap, err := Snapshot(context.Background(), d, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.GuardEvents != 1 {
		t.Errorf("guard_events count: want 1 got %d", snap.Counts.GuardEvents)
	}
	if snap.Counts.RouterDecisions != 2 {
		t.Errorf("router_decisions count: want 2 got %d", snap.Counts.RouterDecisions)
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
	for _, want := range []string{"/tmp/x.db", "schema v1", "Projects:", "Failures (24h):   3", "Cache events:", "claude-code"} {
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
