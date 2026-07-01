package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// fileStateFixture builds a dashboard server seeded with a small
// mixture of read / edit / stale-read actions hitting the same target,
// plus a non-matching action so the COUNT(*) assertions are sharp.
// Uses InsertActions directly (rather than Ingest) so we can set the
// Freshness column explicitly — the freshness engine wouldn't tag a
// fresh tempdir file as "stale" naturally.
func fileStateFixture(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	ctx := context.Background()

	root := t.TempDir()
	pid, err := st.UpsertProject(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range []struct {
		id, tool string
	}{
		{"sA", models.ToolClaudeCode},
		{"sB", models.ToolCursor},
		{"sC", models.ToolCodex},
	} {
		if err := st.UpsertSession(ctx, models.Session{
			ID: sess.id, ProjectID: pid, Tool: sess.tool,
			StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	target := "/abs/path/to/file.go"
	now := time.Now().UTC()
	actions := []models.Action{
		// Two reads (one stale) by claude-code 30/25 min ago.
		{
			SessionID: "sA", ProjectID: pid, Timestamp: now.Add(-30 * time.Minute),
			ActionType: models.ActionReadFile, Target: target, Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e1",
			Freshness: models.FreshnessFresh,
		},
		{
			SessionID: "sA", ProjectID: pid, Timestamp: now.Add(-25 * time.Minute),
			ActionType: models.ActionReadFile, Target: target, Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e2",
			Freshness: models.FreshnessStale,
		},
		// One edit by cursor 10 min ago.
		{
			SessionID: "sB", ProjectID: pid, Timestamp: now.Add(-10 * time.Minute),
			ActionType: models.ActionEditFile, Target: target, Success: true,
			Tool: models.ToolCursor, SourceFile: "f", SourceEventID: "e3",
		},
		// One write by codex 5 min ago — the most-recent edit-class
		// action (but not the most-recent read; last_read_by should
		// still be claude-code).
		{
			SessionID: "sC", ProjectID: pid, Timestamp: now.Add(-5 * time.Minute),
			ActionType: models.ActionWriteFile, Target: target, Success: true,
			Tool: models.ToolCodex, SourceFile: "f", SourceEventID: "e4",
		},
		// Non-matching action hitting a DIFFERENT path; must NOT count.
		{
			SessionID: "sA", ProjectID: pid, Timestamp: now.Add(-5 * time.Minute),
			ActionType: models.ActionReadFile, Target: "/abs/path/to/different.go",
			Success: true, Tool: models.ToolClaudeCode,
			SourceFile: "f", SourceEventID: "e5",
			Freshness: models.FreshnessStale,
		},
		// Action older than the 24h window; must NOT count toward
		// edit_count_24h or stale_rereads_24h.
		{
			SessionID: "sA", ProjectID: pid, Timestamp: now.Add(-72 * time.Hour),
			ActionType: models.ActionEditFile, Target: target, Success: true,
			Tool: models.ToolClaudeCode, SourceFile: "f", SourceEventID: "e6",
		},
	}
	if _, err := st.InsertActions(ctx, actions); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	return s, target
}

func TestFileState_HappyPath(t *testing.T) {
	s, target := fileStateFixture(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/file/state?path="+target, nil)
	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		body, _ := io.ReadAll(rr.Body)
		t.Fatalf("status %d: %s", rr.Code, body)
	}
	var out fileStateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Path != target {
		t.Errorf("path = %q want %q", out.Path, target)
	}
	if out.LastReadBy != models.ToolClaudeCode {
		t.Errorf("last_read_by = %q want %q (write/edit by other tools must not affect last_read_by)", out.LastReadBy, models.ToolClaudeCode)
	}
	if out.EditCount24h != 2 {
		t.Errorf("edit_count_24h = %d want 2 (one cursor edit + one codex write within 24h; the 72h-old edit must be excluded)", out.EditCount24h)
	}
	if out.StaleRereads24h != 1 {
		t.Errorf("stale_rereads_24h = %d want 1 (only the stale claude-code read on this target; the stale read on a different path must not count)", out.StaleRereads24h)
	}
	wantTools := []string{
		models.ToolClaudeCode,
		models.ToolCodex,
		models.ToolCursor,
	}
	if len(out.ToolsTouched) != len(wantTools) {
		t.Fatalf("tools_touched len = %d want %d (%v)", len(out.ToolsTouched), len(wantTools), out.ToolsTouched)
	}
	for i, want := range wantTools {
		if out.ToolsTouched[i] != want {
			t.Errorf("tools_touched[%d] = %q want %q (must be sorted alphabetically)", i, out.ToolsTouched[i], want)
		}
	}
	if out.LastReadAt == "" {
		t.Errorf("last_read_at should be set when a read exists")
	}
}

func TestFileState_MissingPath_400(t *testing.T) {
	s, _ := fileStateFixture(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/file/state", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing path: status %d want 400", rr.Code)
	}
}

func TestFileState_UnknownPath_ZeroValues(t *testing.T) {
	s, _ := fileStateFixture(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/file/state?path=/abs/nothing-here", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unknown path: status %d want 200", rr.Code)
	}
	var out fileStateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.EditCount24h != 0 || out.StaleRereads24h != 0 {
		t.Errorf("expected zero counts for unknown path, got edit=%d stale=%d", out.EditCount24h, out.StaleRereads24h)
	}
	if len(out.ToolsTouched) != 0 {
		t.Errorf("expected empty tools_touched, got %v", out.ToolsTouched)
	}
	// The JSON must include an empty array, not null, so the TS client
	// can iterate without a null-check.
	if !contains(rr.Body.String(), `"tools_touched":[]`) {
		t.Errorf("expected tools_touched to be an empty array in JSON; got: %s", rr.Body.String())
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
