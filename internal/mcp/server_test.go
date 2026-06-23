package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/stash"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// testServer wires an MCP server against a temp SQLite with some seeded
// projects/sessions/actions/excerpts.
func testServer(t *testing.T) (*Server, *sql.DB, string) {
	t.Helper()
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
	return s, database, dir
}

// seed populates enough state to exercise the 4 starter tools. Returns the
// project root path.
func seed(t *testing.T, database *sql.DB) string {
	t.Helper()
	st := store.New(database)
	ctx := context.Background()
	dir := t.TempDir()
	projectRoot := dir
	// A real file so check_file_freshness can hash it.
	fpath := filepath.Join(dir, "handler.go")
	if err := os.WriteFile(fpath, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, projectRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC)
	if err := st.UpsertSession(ctx, models.Session{
		ID: "sess-A", ProjectID: pid, Tool: models.ToolClaudeCode,
		Model: "claude-sonnet-4", StartedAt: start,
	}); err != nil {
		t.Fatal(err)
	}
	// Insert a read action on handler.go (stored as project-relative).
	rel, _ := filepath.Rel(projectRoot, fpath)
	events := []models.ToolEvent{{
		SourceFile: "A.jsonl", SourceEventID: "e1",
		SessionID: "sess-A", ProjectRoot: projectRoot,
		Timestamp: start.Add(time.Second), Tool: models.ToolClaudeCode,
		ActionType: models.ActionReadFile, Target: rel,
		Success: true, RawToolName: "Read",
		ToolOutput: "file contents: PASS all 3 tests",
	}, {
		SourceFile: "A.jsonl", SourceEventID: "e2",
		SessionID: "sess-A", ProjectRoot: projectRoot,
		Timestamp: start.Add(2 * time.Second), Tool: models.ToolClaudeCode,
		ActionType: models.ActionRunCommand, Target: "go test ./...",
		Success: false, ErrorMessage: "FAIL TestExample: want 3 got 4",
		RawToolName: "Bash",
		ToolOutput:  "TestExample FAIL want 3 got 4 exit 1",
	}}
	indexer := indexing.New(database, 0)
	_, err = st.Ingest(ctx, events, nil, store.IngestOptions{
		Indexer: indexer,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	return projectRoot
}

// rpcCall sends one JSON-RPC message through Run and returns the decoded
// response. For notifications (id = nil) it returns nil.
func rpcCall(t *testing.T, s *Server, method string, id int, params any) map[string]any {
	t.Helper()
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id,omitempty"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	req.JSONRPC = "2.0"
	if id >= 0 {
		req.ID = id
	}
	req.Method = method
	req.Params = params
	body, _ := json.Marshal(req)

	in := bytes.NewReader(append(body, '\n'))
	var out bytes.Buffer
	if err := s.Run(context.Background(), in, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id < 0 {
		return nil
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", out.String(), err)
	}
	return resp
}

// callTool is a tools/call convenience that parses the inner JSON content.
func callTool(t *testing.T, s *Server, name string, args any) map[string]any {
	t.Helper()
	resp := rpcCall(t, s, "tools/call", 1, map[string]any{
		"name":      name,
		"arguments": args,
	})
	if e, ok := resp["error"]; ok && e != nil {
		t.Fatalf("%s returned error: %v", name, e)
	}
	result := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		content := result["content"].([]any)
		t.Fatalf("%s isError=true: %v", name, content)
	}
	content := result["content"].([]any)[0].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content["text"].(string)), &parsed); err != nil {
		t.Fatalf("parse tool output: %v", err)
	}
	return parsed
}

func TestServer_InitializeAndToolsList(t *testing.T) {
	s, _, _ := testServer(t)

	resp := rpcCall(t, s, "initialize", 0, map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if resp["error"] != nil {
		t.Fatalf("initialize error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if _, ok := result["capabilities"]; !ok {
		t.Errorf("initialize missing capabilities: %v", result)
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "test" {
		t.Errorf("serverInfo.name: %v", info["name"])
	}

	resp = rpcCall(t, s, "tools/list", 1, nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		tm := raw.(map[string]any)
		names[tm["name"].(string)] = true
		if _, ok := tm["description"]; !ok {
			t.Errorf("tool %s missing description", tm["name"])
		}
		if _, ok := tm["inputSchema"]; !ok {
			t.Errorf("tool %s missing inputSchema", tm["name"])
		}
	}
	expected := []string{"check_file_freshness", "get_file_history", "get_session_summary", "search_past_outputs"}
	for _, want := range expected {
		if !names[want] {
			t.Errorf("tool %s not registered: have %v", want, names)
		}
	}
}

func TestServer_UnknownMethod(t *testing.T) {
	s, _, _ := testServer(t)
	resp := rpcCall(t, s, "does/not/exist", 42, nil)
	if resp["error"] == nil {
		t.Fatalf("expected error for unknown method: %v", resp)
	}
	errObj := resp["error"].(map[string]any)
	if int(errObj["code"].(float64)) != errMethodNotFound {
		t.Errorf("code: %v", errObj["code"])
	}
}

func TestServer_Notification_NoResponse(t *testing.T) {
	s, _, _ := testServer(t)
	// Pass id=-1 to construct a request without an ID (a notification).
	// The response buffer should be empty.
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}
	req.JSONRPC = "2.0"
	req.Method = "notifications/initialized"
	body, _ := json.Marshal(req)

	in := bytes.NewReader(append(body, '\n'))
	var out bytes.Buffer
	if err := s.Run(context.Background(), in, &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("notification produced response: %q", out.String())
	}
}

func TestTool_CheckFileFreshness(t *testing.T) {
	s, db, _ := testServer(t)
	root := seed(t, db)
	// Use the absolute file path so classifier can hash it.
	parsed := callTool(t, s, "check_file_freshness", map[string]any{
		"project_root": root,
		"file_path":    "handler.go",
	})
	if parsed["file"] == nil {
		t.Errorf("missing file: %v", parsed)
	}
	if _, ok := parsed["current_hash"].(string); !ok {
		t.Errorf("missing current_hash: %v", parsed)
	}
	// Freshness is expected to be non-empty.
	if fr, _ := parsed["freshness"].(string); fr == "" {
		t.Errorf("empty freshness: %v", parsed)
	}
}

func TestTool_GetFileHistory(t *testing.T) {
	s, db, _ := testServer(t)
	root := seed(t, db)

	parsed := callTool(t, s, "get_file_history", map[string]any{
		"project_root": root,
		"file_path":    "handler.go",
	})
	if int(parsed["count"].(float64)) < 1 {
		t.Errorf("no entries returned: %v", parsed)
	}
	entries := parsed["entries"].([]any)
	if len(entries) < 1 {
		t.Fatal("no entries")
	}
	first := entries[0].(map[string]any)
	if first["action_type"] != "read_file" {
		t.Errorf("action_type: %v", first["action_type"])
	}
	if first["tool"] != "claude-code" {
		t.Errorf("tool: %v", first["tool"])
	}
}

func TestTool_GetSessionSummary(t *testing.T) {
	s, db, _ := testServer(t)
	root := seed(t, db)

	parsed := callTool(t, s, "get_session_summary", map[string]any{
		"project_root": root,
	})
	sessions := parsed["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("session count: %d", len(sessions))
	}
	sess := sessions[0].(map[string]any)
	if sess["session_id"] != "sess-A" {
		t.Errorf("session_id: %v", sess["session_id"])
	}
	if int(sess["action_count"].(float64)) != 2 {
		t.Errorf("action_count: %v (want 2)", sess["action_count"])
	}
	if int(sess["failure_count"].(float64)) != 1 {
		t.Errorf("failure_count: %v (want 1)", sess["failure_count"])
	}
}

func TestTool_SearchPastOutputs(t *testing.T) {
	s, db, _ := testServer(t)
	_ = seed(t, db)

	parsed := callTool(t, s, "search_past_outputs", map[string]any{
		"query": "FAIL",
	})
	hits := parsed["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit for 'FAIL': %v", parsed)
	}
	first := hits[0].(map[string]any)
	if !strings.Contains(first["excerpt"].(string), "FAIL") {
		t.Errorf("excerpt missing FAIL: %v", first["excerpt"])
	}
}

func TestServer_ToolErrorPath(t *testing.T) {
	s, db, _ := testServer(t)
	_ = seed(t, db)

	// Missing required argument should produce an in-band isError=true, not
	// a transport-layer RPC error. Per MCP spec, tool errors stay in-band so
	// the model can react.
	resp := rpcCall(t, s, "tools/call", 7, map[string]any{
		"name":      "search_past_outputs",
		"arguments": map[string]any{},
	})
	if resp["error"] != nil {
		t.Fatalf("expected result, not transport error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true, got %v", result)
	}
}

// listToolNames returns the set of tool names exposed by tools/list.
func listToolNames(t *testing.T, s *Server) map[string]bool {
	t.Helper()
	resp := rpcCall(t, s, "tools/list", 1, nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	out := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tm := raw.(map[string]any)
		out[tm["name"].(string)] = true
	}
	return out
}

// v7_12ToolNames are the four retrieval tools the V7-16 features
// filter scopes. Pinned here so the tests below can iterate uniformly.
var v7_12ToolNames = []string{"get_file", "get_symbols", "get_relations", "retrieve_stashed"}

// v7_12FixtureOpts builds an Options that enables every V7-12 tool
// so the filter behavior can be observed in isolation.
func v7_12FixtureOpts(t *testing.T, database *sql.DB, features []string) Options {
	t.Helper()
	dir := t.TempDir()
	st, err := stash.New(stash.Options{Dir: dir})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	if _, err := st.Write([]byte("seed")); err != nil {
		t.Fatalf("stash seed: %v", err)
	}
	return Options{
		DB:                  database,
		ServerName:          "test",
		ServerVersion:       "0",
		Stash:               st,
		GetFileEnabled:      true,
		GetFile:             GetFileOptions{AllowExtensions: []string{"go"}, MaxResponseKB: 100},
		GetSymbolsEnabled:   true,
		GetSymbols:          GetSymbolsOptions{MaxCallers: 20, MaxCallees: 20},
		GetRelationsEnabled: true,
		GetRelations:        GetRelationsOptions{MaxDepth: 5, MaxResults: 100},
		Features:            features,
	}
}

// TestServer_FeaturesFilter_EmptyMeansAll: nil and empty-slice both
// register every V7-12 tool whose per-tool flag is true. Equivalent
// to v1.7.10 behavior — no observable change for operators not
// setting the features list.
func TestServer_FeaturesFilter_EmptyMeansAll(t *testing.T) {
	for _, name := range []string{"nil-features", "empty-features"} {
		var feat []string
		if name == "empty-features" {
			feat = []string{}
		}
		_, database, _ := testServer(t)
		s, err := New(v7_12FixtureOpts(t, database, feat))
		if err != nil {
			t.Fatalf("%s: New: %v", name, err)
		}
		got := listToolNames(t, s)
		for _, n := range v7_12ToolNames {
			if !got[n] {
				t.Errorf("%s: %s missing from tools/list", name, n)
			}
		}
	}
}

// TestServer_FeaturesFilter_ExplicitAllowList: a non-empty features
// list keeps only the listed V7-12 tools; the others drop out.
func TestServer_FeaturesFilter_ExplicitAllowList(t *testing.T) {
	_, database, _ := testServer(t)
	s, err := New(v7_12FixtureOpts(t, database, []string{"retrieve_stashed", "get_file"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := listToolNames(t, s)
	for _, n := range []string{"retrieve_stashed", "get_file"} {
		if !got[n] {
			t.Errorf("explicit allow-list dropped %s", n)
		}
	}
	for _, n := range []string{"get_symbols", "get_relations"} {
		if got[n] {
			t.Errorf("explicit allow-list left %s registered (not in list)", n)
		}
	}
}

// TestServer_FeaturesFilter_PerToolEnabledWins: per-tool enabled=false
// keeps the tool unregistered even when the features list names it.
// Verifies the V7-16 precedence rule "per-tool always wins over filter."
func TestServer_FeaturesFilter_PerToolEnabledWins(t *testing.T) {
	_, database, _ := testServer(t)
	opts := v7_12FixtureOpts(t, database, []string{"get_file", "get_symbols"})
	opts.GetFileEnabled = false // explicit per-tool kill switch
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := listToolNames(t, s)
	if got["get_file"] {
		t.Errorf("per-tool GetFileEnabled=false should win over features list, but get_file registered")
	}
	if !got["get_symbols"] {
		t.Errorf("get_symbols should still register (in list, per-tool enabled true)")
	}
}

// TestServer_FeaturesFilter_BuiltinsAlwaysOn: the 13 built-in
// observability tools register regardless of features list — only the
// four V7-12 retrieval tools are subject to the filter (V7-16 scope
// decision per D-3 in the v1.7.11 plan doc).
func TestServer_FeaturesFilter_BuiltinsAlwaysOn(t *testing.T) {
	_, database, _ := testServer(t)
	// Aggressively restrictive: name a V7-12 tool we KNOW is not
	// registered (no per-tool enabled flag set, no Stash), so no
	// V7-12 tools pass. Built-ins must still be present.
	s, err := New(Options{
		DB:            database,
		ServerName:    "test",
		ServerVersion: "0",
		Features:      []string{"retrieve_stashed"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := listToolNames(t, s)
	for _, builtin := range []string{
		"check_command_freshness", "check_file_freshness",
		"get_action_details", "get_cost_summary",
		"get_failure_context", "get_file_history",
		"get_last_test_result", "get_project_patterns",
		"get_redundancy_report", "get_session_recovery_context",
		"get_session_summary", "list_actions_around",
		"search_past_outputs",
	} {
		if !got[builtin] {
			t.Errorf("built-in %s missing under strict features filter (should be unaffected)", builtin)
		}
	}
}

// TestServer_RetrieveStashedDisabled: Stash is set but
// RetrieveStashedDisabled=true keeps the tool unregistered. Operator
// path for "proxy compression yes, agent retrieval no" (asymmetric
// trust scenarios).
func TestServer_RetrieveStashedDisabled(t *testing.T) {
	_, database, _ := testServer(t)
	dir := t.TempDir()
	st, _ := stash.New(stash.Options{Dir: dir})
	s, err := New(Options{
		DB:                      database,
		ServerName:              "test",
		ServerVersion:           "0",
		Stash:                   st,
		RetrieveStashedDisabled: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if listToolNames(t, s)["retrieve_stashed"] {
		t.Errorf("RetrieveStashedDisabled=true should prevent registration")
	}
}

// TestServer_ParseError covers the malformed-JSON path.
func TestServer_ParseError(t *testing.T) {
	s, _, _ := testServer(t)
	in := strings.NewReader("{not json\n")
	var out bytes.Buffer
	if err := s.Run(context.Background(), in, &out); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Run: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v: %q", err, out.String())
	}
	if resp["error"] == nil {
		t.Errorf("expected parse error: %v", resp)
	}
}
