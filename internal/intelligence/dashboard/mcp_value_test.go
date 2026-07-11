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

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
)

func mcpTestServer(t *testing.T, cfgBody string) (*Server, string) {
	t.Helper()
	tdir := t.TempDir()
	cfgPath := filepath.Join(tdir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	return server, cfgPath
}

// TestHandleConfigSection_MCP pins the P4.10 section: a PUT persists
// the [intelligence.mcp] tree through config.Load, and the save
// reports restart_required=false — MCP servers are spawned per AI
// session and run config.Load themselves, so the restart banner
// would lie.
func TestHandleConfigSection_MCP(t *testing.T) {
	server, cfgPath := mcpTestServer(t, "[observer]\nlog_level = \"info\"\n")

	body := `{"Features":["get_file"],"GetFile":{"Enabled":true,"AllowExtensions":[".go"],"DenyPaths":[],"MaxResponseKB":64},` +
		`"GetSymbols":{"Enabled":false,"MaxCallers":10,"MaxCallees":10},` +
		`"GetRelations":{"Enabled":false,"MaxDepth":2,"MaxResults":50},` +
		`"RetrieveStashed":{"Enabled":true,"MaxShasPerCall":25},"Audit":{"Enabled":false}}`
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPut, "/api/config/section/mcp", strings.NewReader(body)))
	if rr.Code != 200 {
		t.Fatalf("PUT mcp: status %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		RestartRequired bool `json:"restart_required"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.RestartRequired {
		t.Error("mcp saves bind at the next MCP server spawn — restart_required must be false")
	}

	reloaded, err := config.Load(config.LoadOptions{GlobalPath: cfgPath})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	mcp := reloaded.Intelligence.MCP
	if len(mcp.Features) != 1 || mcp.Features[0] != "get_file" {
		t.Errorf("features not persisted: %+v", mcp.Features)
	}
	if !mcp.GetFile.Enabled || mcp.GetFile.MaxResponseKB != 64 {
		t.Errorf("get_file not persisted: %+v", mcp.GetFile)
	}
	if mcp.GetSymbols.Enabled {
		t.Errorf("get_symbols enabled=false not persisted: %+v", mcp.GetSymbols)
	}
	if mcp.Audit.Enabled {
		t.Errorf("audit enabled=false not persisted: %+v", mcp.Audit)
	}

	// editable_sections must advertise the new section.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got struct {
		EditableSections []string `json:"editable_sections"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range got.EditableSections {
		if s == "mcp" {
			found = true
		}
	}
	if !found {
		t.Error("editable_sections missing mcp")
	}
}

func seedMCPAudit(t *testing.T, server *Server, tool string, n int, ok bool) {
	t.Helper()
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	okInt := 0
	if ok {
		okInt = 1
	}
	for i := range n {
		_, err := server.opts.DB.Exec(
			`INSERT INTO mcp_audit (ts, tool_name, request_hash, response_size_bytes, response_ok)
			 VALUES (?, ?, ?, 100, ?)`, ts, tool, fmt.Sprintf("h%d", i), okInt,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func seedTurns(t *testing.T, server *Server, n int) {
	t.Helper()
	ts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	for range n {
		_, err := server.opts.DB.Exec(
			`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens)
			 VALUES ('s', ?, 'anthropic', 'm', 1, 1)`, ts,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func getValue(t *testing.T, server *Server) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/mcp/value", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /api/mcp/value: %d body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestMCPValueMeter pins the meter's verdict honesty: active above
// the advisor threshold, low_use below it, unused at zero calls —
// and no_data (never "unused") when the audit log is disabled,
// because silence then means invisibility, not absence.
func TestMCPValueMeter(t *testing.T) {
	t.Run("active above threshold", func(t *testing.T) {
		server, _ := mcpTestServer(t, "[observer]\nlog_level = \"info\"\n")
		seedTurns(t, server, 100)
		seedMCPAudit(t, server, "get_file", 5, true)
		got := getValue(t, server)
		if got["verdict"] != "active" {
			t.Errorf("verdict: got %v want active (resp=%v)", got["verdict"], got)
		}
		if got["calls"].(float64) != 5 || got["turns_estimate"].(float64) != 100 {
			t.Errorf("counts: %v", got)
		}
		byTool := got["by_tool"].([]any)
		if len(byTool) != 1 {
			t.Errorf("by_tool: %v", byTool)
		}
	})
	t.Run("low_use below threshold", func(t *testing.T) {
		server, _ := mcpTestServer(t, "[observer]\nlog_level = \"info\"\n")
		seedTurns(t, server, 100)
		seedMCPAudit(t, server, "get_file", 1, true)
		got := getValue(t, server)
		if got["verdict"] != "low_use" {
			t.Errorf("verdict: got %v want low_use", got["verdict"])
		}
	})
	t.Run("unused at zero calls", func(t *testing.T) {
		server, _ := mcpTestServer(t, "[observer]\nlog_level = \"info\"\n")
		seedTurns(t, server, 50)
		got := getValue(t, server)
		if got["verdict"] != "unused" {
			t.Errorf("verdict: got %v want unused", got["verdict"])
		}
	})
	t.Run("audit off reads no_data not unused", func(t *testing.T) {
		server, _ := mcpTestServer(t,
			"[observer]\nlog_level = \"info\"\n\n[intelligence.mcp.audit]\nenabled = false\n")
		seedTurns(t, server, 50)
		got := getValue(t, server)
		if got["verdict"] != "no_data" {
			t.Errorf("verdict: got %v want no_data (audit off)", got["verdict"])
		}
		if got["audit_enabled"] != false {
			t.Errorf("audit_enabled: got %v want false", got["audit_enabled"])
		}
	})
	t.Run("no turns reads no_data", func(t *testing.T) {
		server, _ := mcpTestServer(t, "[observer]\nlog_level = \"info\"\n")
		got := getValue(t, server)
		if got["verdict"] != "no_data" {
			t.Errorf("verdict: got %v want no_data", got["verdict"])
		}
	})
}
