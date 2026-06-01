package mcp

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

// seedActionsForListAround inserts N sequentially-timestamped actions in
// one session so list_actions_around can be exercised against a known
// timeline. Returns the action IDs in chronological order.
func seedActionsForListAround(t *testing.T, database *sql.DB, n int) []int64 {
	t.Helper()
	st := store.New(database)
	ctx := context.Background()
	dir := t.TempDir()
	pid, err := st.UpsertProject(ctx, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	if err := st.UpsertSession(ctx, models.Session{
		ID: "sess-G33", ProjectID: pid, Tool: models.ToolClaudeCode,
		Model: "claude-sonnet-4", StartedAt: start,
	}); err != nil {
		t.Fatal(err)
	}
	events := make([]models.ToolEvent, n)
	for i := 0; i < n; i++ {
		events[i] = models.ToolEvent{
			SourceFile:    "G.jsonl",
			SourceEventID: "g" + itoa(i),
			SessionID:     "sess-G33", ProjectRoot: dir,
			Timestamp:  start.Add(time.Duration(i) * time.Second),
			Tool:       models.ToolClaudeCode,
			ActionType: models.ActionReadFile,
			Target:     "file" + itoa(i) + ".go",
			Success:    true, RawToolName: "Read",
		}
	}
	if _, err := st.Ingest(ctx, events, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	rows, err := database.QueryContext(ctx,
		`SELECT id FROM actions WHERE session_id = 'sess-G33' ORDER BY timestamp ASC, id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) != n {
		t.Fatalf("seeded %d actions but got %d back", n, len(ids))
	}
	return ids
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// TestServer_ListActionsAround_HappyPath pins the three-layer
// progressive-disclosure contract: ±N actions chronological, target
// in the middle, position labels correct.
func TestServer_ListActionsAround_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ids := seedActionsForListAround(t, database, 11)
	target := ids[5] // middle of 11

	s, err := New(Options{DB: database, ServerName: "test", ServerVersion: "0"})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	parsed := callTool(t, s, "list_actions_around", map[string]any{
		"action_id": target,
		"before":    3,
		"after":     2,
	})
	if found, _ := parsed["found"].(bool); !found {
		t.Errorf("found=false on existing action_id, parsed=%v", parsed)
	}
	actions := parsed["actions"].([]any)
	if len(actions) != 6 { // 3 before + 1 target + 2 after
		t.Fatalf("got %d actions, want 6: %v", len(actions), actions)
	}

	// Chronological order: ids[2..7] inclusive = 6 ids.
	want := []int64{ids[2], ids[3], ids[4], ids[5], ids[6], ids[7]}
	wantPos := []string{"before", "before", "before", "target", "after", "after"}
	for i, raw := range actions {
		row := raw.(map[string]any)
		if int64(row["id"].(float64)) != want[i] {
			t.Errorf("idx %d: id %v, want %d", i, row["id"], want[i])
		}
		if row["position"] != wantPos[i] {
			t.Errorf("idx %d: position %q, want %q", i, row["position"], wantPos[i])
		}
	}
}

// TestServer_ListActionsAround_NotFound pins that an unknown action_id
// returns found=false + empty actions, not an error.
func TestServer_ListActionsAround_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	s, err := New(Options{DB: database, ServerName: "test", ServerVersion: "0"})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	parsed := callTool(t, s, "list_actions_around", map[string]any{
		"action_id": int64(99999),
	})
	if found, _ := parsed["found"].(bool); found {
		t.Errorf("found=true on missing action_id, parsed=%v", parsed)
	}
	actions := parsed["actions"].([]any)
	if len(actions) != 0 {
		t.Errorf("expected empty actions, got %v", actions)
	}
}

// TestServer_ListActionsAround_AtSessionBoundary pins that asking for
// before=10 on the first action in a session returns just the target +
// after-rows (the before window is empty at the head).
func TestServer_ListActionsAround_AtSessionBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ids := seedActionsForListAround(t, database, 5)
	first := ids[0]

	s, err := New(Options{DB: database, ServerName: "test", ServerVersion: "0"})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	parsed := callTool(t, s, "list_actions_around", map[string]any{
		"action_id": first,
		"before":    10,
		"after":     2,
	})
	actions := parsed["actions"].([]any)
	if len(actions) != 3 { // target + 2 after
		t.Fatalf("got %d actions, want 3: %v", len(actions), actions)
	}
	if actions[0].(map[string]any)["position"] != "target" {
		t.Errorf("first row should be target at session boundary, got %v", actions[0])
	}
}

// TestServer_ListActionsAround_DefaultsAndCaps pins:
//   - before/after default to 5 when zero/missing.
//   - before/after are clamped to 20 when over.
func TestServer_ListActionsAround_DefaultsAndCaps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ids := seedActionsForListAround(t, database, 50)
	target := ids[25]

	s, err := New(Options{DB: database, ServerName: "test", ServerVersion: "0"})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}

	// Default: before=5 + 1 target + after=5 = 11 actions.
	parsed := callTool(t, s, "list_actions_around", map[string]any{"action_id": target})
	if got := len(parsed["actions"].([]any)); got != 11 {
		t.Errorf("default window: got %d, want 11", got)
	}
	if int(parsed["before"].(float64)) != 5 || int(parsed["after"].(float64)) != 5 {
		t.Errorf("defaults not applied: %v", parsed)
	}

	// Cap: ask for 1000 + 1000 → clamped to 20 + 20 = 41 actions (with target).
	parsed = callTool(t, s, "list_actions_around", map[string]any{
		"action_id": target,
		"before":    1000,
		"after":     1000,
	})
	if got := len(parsed["actions"].([]any)); got != 41 {
		t.Errorf("capped window: got %d, want 41", got)
	}
}
