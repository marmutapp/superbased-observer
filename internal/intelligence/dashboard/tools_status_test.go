package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// toolsStatusTestServer builds a server with an injected two-tool
// catalog: "fake-detected" (watch path exists) and "fake-missing"
// (watch path doesn't). Neither has hook/MCP/proxy integrations, so
// the test stays independent of the developer machine's real
// ~/.claude / ~/.codex state.
func toolsStatusTestServer(t *testing.T) *Server {
	t.Helper()
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[observer.watch]\nenabled_adapters = [\"fake-detected\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detectedDir := filepath.Join(tdir, "fake-tool-home")
	if err := os.MkdirAll(detectedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	// One captured action for a tool OUTSIDE the catalog — pins the
	// union behavior (DB activity always gets a row).
	st := store.New(database)
	ctx := context.Background()
	pid, err := st.UpsertProject(ctx, tdir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSession(ctx, models.Session{
		ID: "s1", ProjectID: pid, Tool: "legacy-tool",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertActions(ctx, []models.Action{{
		Tool:          "legacy-tool",
		SessionID:     "s1",
		ProjectID:     pid,
		ActionType:    "command_run",
		Target:        "echo hi",
		Timestamp:     time.Now().UTC(),
		SourceFile:    "f",
		SourceEventID: "e1",
	}}); err != nil {
		t.Fatal(err)
	}

	server, err := New(Options{
		DB:         database,
		ConfigPath: cfgPath,
		ToolCatalog: []ToolCatalogEntry{
			{Tool: "fake-detected", WatchPaths: []string{detectedDir}},
			{Tool: "fake-missing", WatchPaths: []string{filepath.Join(tdir, "nope")}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

type toolsStatusWire struct {
	Tools []struct {
		Tool         string `json:"tool"`
		Detected     bool   `json:"detected"`
		DetectedPath string `json:"detected_path"`
		Enabled      bool   `json:"enabled"`
		ActionCount  int64  `json:"action_count"`
		LastSeenAt   string `json:"last_seen_at"`
	} `json:"tools"`
}

// TestToolsStatusMatrix pins the composition rules: catalog detection
// via watch-path existence, the EnabledAdapters allow-list flag, and
// the union row for DB-only tools.
func TestToolsStatusMatrix(t *testing.T) {
	server := toolsStatusTestServer(t)
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/tools/status", nil))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got toolsStatusWire
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	rows := map[string]int{}
	for i, r := range got.Tools {
		rows[r.Tool] = i
	}
	for _, want := range []string{"fake-detected", "fake-missing", "legacy-tool"} {
		if _, ok := rows[want]; !ok {
			t.Fatalf("missing row %q in %v", want, rows)
		}
	}

	det := got.Tools[rows["fake-detected"]]
	if !det.Detected || det.DetectedPath == "" {
		t.Errorf("fake-detected: detected=%v path=%q", det.Detected, det.DetectedPath)
	}
	if !det.Enabled {
		t.Errorf("fake-detected should be in the enabled allow-list")
	}

	miss := got.Tools[rows["fake-missing"]]
	if miss.Detected || miss.Enabled {
		t.Errorf("fake-missing: detected=%v enabled=%v", miss.Detected, miss.Enabled)
	}

	legacy := got.Tools[rows["legacy-tool"]]
	if legacy.ActionCount != 1 || legacy.LastSeenAt == "" {
		t.Errorf("legacy-tool activity: count=%d last=%q", legacy.ActionCount, legacy.LastSeenAt)
	}
	if legacy.Detected {
		t.Errorf("legacy-tool has no catalog entry; detected must be false")
	}

	// Method guard.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/tools/status", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d want 405", rr.Code)
	}
}
