package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/claudeplugin"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
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

// TestProbeFromResultReportsPluginWiring is codex finding M4: a
// registrar result whose Skipped flag is set means the tool is wired
// through its OWN plugin surface. Connected Tools must render that as a
// THIRD state — wired, but not by anything `observer init` wrote —
// rather than as "registered=false / would register 0 events" (hooks) or
// "not registered" (MCP).
func TestProbeFromResultReportsPluginWiring(t *testing.T) {
	// The expected reason is NOT hand-typed — it is exactly what the
	// PRODUCTION detector (internal/claudeplugin) emits, built by feeding a
	// real settings.json through claudeplugin.DetectInClaudeDir the same
	// way internal/hook/register.go and internal/mcp/register.go do before
	// stuffing the result into RegistrationResult.SkipReason. This closes
	// the codex finding: a hand-typed constant only pins the literal
	// "wired via the Claude Code plugin — " PREFIX the dashboard code
	// writes, so a bug that dropped, truncated, or otherwise corrupted the
	// propagation of res.SkipReason into Detail would still satisfy a
	// prefix-only check. Asserting the FULL detail against the reason the
	// live API actually computes makes that corruption visible.
	claudeDir := t.TempDir()
	settingsPath := filepath.Join(claudeDir, "settings.json")
	settingsBody := fmt.Sprintf(`{"enabledPlugins": {%q: true}}`, claudeplugin.EnabledKey)
	if err := os.WriteFile(settingsPath, []byte(settingsBody), 0o644); err != nil {
		t.Fatal(err)
	}
	detection := claudeplugin.DetectInClaudeDir(claudeDir)
	if !detection.Active {
		t.Fatalf("fixture setup: expected claudeplugin.DetectInClaudeDir to report Active for the settings.json we just wrote")
	}
	reason := detection.Reason()
	if reason == "" {
		t.Fatal("fixture setup: Detection.Reason() returned empty for an Active detection")
	}
	wantWiredDetail := "wired via the Claude Code plugin — " + reason

	t.Run("hooks", func(t *testing.T) {
		cases := []struct {
			name           string
			res            hook.RegistrationResult
			wantRegistered bool
			wantViaPlugin  bool
			wantDetail     string
		}{
			{
				name:           "skipped — wired via the plugin",
				res:            hook.RegistrationResult{Skipped: true, SkipReason: reason},
				wantRegistered: true,
				wantViaPlugin:  true,
				wantDetail:     wantWiredDetail,
			},
			{
				name:           "all events already registered by init",
				res:            hook.RegistrationResult{AlreadySet: []string{"Stop", "PreToolUse"}},
				wantRegistered: true,
				wantDetail:     "2 events registered",
			},
			{
				name:       "nothing registered",
				res:        hook.RegistrationResult{HooksAdded: []string{"Stop"}},
				wantDetail: "would register 1 events",
			},
			{
				name:       "partial",
				res:        hook.RegistrationResult{AlreadySet: []string{"Stop"}, HooksAdded: []string{"PreToolUse"}},
				wantDetail: "1 registered, 1 missing",
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				p := hookProbeFromResult(c.res)
				if p.Registered != c.wantRegistered {
					t.Errorf("Registered = %v, want %v", p.Registered, c.wantRegistered)
				}
				if p.ViaPlugin != c.wantViaPlugin {
					t.Errorf("ViaPlugin = %v, want %v", p.ViaPlugin, c.wantViaPlugin)
				}
				if !strings.Contains(p.Detail, c.wantDetail) {
					t.Errorf("Detail = %q, want it to contain %q", p.Detail, c.wantDetail)
				}
				// A skipped result must never read as "would register".
				if c.res.Skipped && strings.Contains(p.Detail, "would register") {
					t.Errorf("Detail = %q claims a pending registration for a plugin-wired tool", p.Detail)
				}
			})
		}
	})

	t.Run("mcp", func(t *testing.T) {
		cases := []struct {
			name           string
			res            mcp.RegistrationResult
			wantRegistered bool
			wantViaPlugin  bool
			wantDetail     string
		}{
			{
				name:           "skipped — wired via the plugin",
				res:            mcp.RegistrationResult{Skipped: true, SkipReason: reason},
				wantRegistered: true,
				wantViaPlugin:  true,
				wantDetail:     wantWiredDetail,
			},
			{
				name:           "already set by init",
				res:            mcp.RegistrationResult{AlreadySet: true},
				wantRegistered: true,
				wantDetail:     "observer MCP server registered",
			},
			{
				name:       "absent",
				res:        mcp.RegistrationResult{},
				wantDetail: "not registered",
			},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				p := mcpProbeFromResult(c.res)
				if p.Registered != c.wantRegistered {
					t.Errorf("Registered = %v, want %v", p.Registered, c.wantRegistered)
				}
				if p.ViaPlugin != c.wantViaPlugin {
					t.Errorf("ViaPlugin = %v, want %v", p.ViaPlugin, c.wantViaPlugin)
				}
				if !strings.Contains(p.Detail, c.wantDetail) {
					t.Errorf("Detail = %q, want it to contain %q", p.Detail, c.wantDetail)
				}
				if c.res.Skipped && strings.Contains(p.Detail, "not registered") {
					t.Errorf("Detail = %q says not registered for a plugin-wired tool", p.Detail)
				}
			})
		}
	})
}
