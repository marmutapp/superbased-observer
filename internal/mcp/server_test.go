package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
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

// TestServer_ParseError covers the malformed-JSON path.
func TestServer_ParseError(t *testing.T) {
	s, _, _ := testServer(t)
	in := strings.NewReader("{not json\n")
	var out bytes.Buffer
	if err := s.Run(context.Background(), in, &out); err != nil && err != io.EOF {
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
