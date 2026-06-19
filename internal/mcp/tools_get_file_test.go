package mcp

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/mcp/audit"
)

// getFileFixture wires a minimal MCP server with the get_file tool
// registered + an audit writer that records into a real mcp_audit
// table. Returns the server, root dir, audit DB handle, and a cleanup
// function (managed via t.Cleanup).
type getFileFixture struct {
	s       *Server
	root    string
	auditDB *sql.DB
}

func newGetFileFixture(t *testing.T, opts GetFileOptions) *getFileFixture {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "obs.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	auditW := audit.NewSQLWriter(database, slog.New(slog.NewTextHandler(io.Discard, nil)), audit.SQLWriterOptions{
		FlushInterval: 10 * time.Millisecond,
		BatchSize:     8,
	})
	t.Cleanup(func() { _ = auditW.Close() })

	root := t.TempDir()
	s, err := New(Options{
		DB:             database,
		ServerName:     "test",
		ServerVersion:  "0",
		GetFile:        opts,
		GetFileEnabled: true,
		AuditWriter:    auditW,
	})
	if err != nil {
		t.Fatalf("mcp.New: %v", err)
	}
	return &getFileFixture{s: s, root: root, auditDB: database}
}

func (f *getFileFixture) writeFile(t *testing.T, rel, content string) string {
	t.Helper()
	abs := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return abs
}

// closeAuditAndCount waits for the buffered audit writer to flush
// (mcp.audit's writer doesn't expose a Sync; we close it indirectly
// via t.Cleanup. For mid-test assertions, sleep briefly so the
// flush-interval tick fires).
//
// Returns the FIRST non-zero count it observes. Tests that expect
// a specific number > 1 must use auditRowCountAtLeast — otherwise
// the early-exit-on-n>0 here returns the partial count of the
// first flush tick and races against subsequent flushes.
func auditRowCount(t *testing.T, database *sql.DB, where string, args ...any) int {
	t.Helper()
	// Force flush by waiting one flush-interval cycle.
	for i := 0; i < 20; i++ {
		var n int
		q := "SELECT COUNT(*) FROM mcp_audit"
		if where != "" {
			q += " WHERE " + where
		}
		if err := database.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return n
		}
		// 20ms enough for the 10ms flush interval to tick at least twice
		time.Sleep(20 * time.Millisecond)
	}
	return 0
}

// auditRowCountAtLeast polls until the count reaches `want` OR the
// timeout window closes. Returns the highest count observed. Tests
// asserting on a specific N > 1 must use this variant — the plain
// auditRowCount returns the first non-zero count, which races
// against multi-row flushes when the async writer hasn't drained
// all batched rows yet (the cause of the pre-2026-06-09
// TestGetSymbols_AuditWrittenPerRequest flake under tree-wide -race
// load).
//
// Window is the same 400ms as auditRowCount (20 ticks × 20 ms).
// Returns early as soon as want is reached so happy-path tests
// stay fast.
func auditRowCountAtLeast(t *testing.T, database *sql.DB, want int, where string, args ...any) int {
	t.Helper()
	var best int
	for i := 0; i < 20; i++ {
		var n int
		q := "SELECT COUNT(*) FROM mcp_audit"
		if where != "" {
			q += " WHERE " + where
		}
		if err := database.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > best {
			best = n
		}
		if best >= want {
			return best
		}
		time.Sleep(20 * time.Millisecond)
	}
	return best
}

func TestGetFile_ReadsFullFile(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: []string{"ts"},
		MaxResponseKB:   100,
	})
	f.writeFile(t, "src/foo.ts", "line1\nline2\nline3\n")

	res := callTool(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "src/foo.ts",
	})
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if body, _ := res["body"].(string); body != "line1\nline2\nline3\n" {
		t.Errorf("body mismatch: %q", body)
	}
	lines := res["lines"].(map[string]any)
	if int(lines["start"].(float64)) != 1 || int(lines["end"].(float64)) != 3 ||
		int(lines["total"].(float64)) != 3 {
		t.Errorf("lines: %+v", lines)
	}
	if rel, _ := res["project_relative_path"].(string); rel != "src/foo.ts" {
		t.Errorf("project_relative_path: %q", rel)
	}
	if trunc, _ := res["truncated"].(bool); trunc {
		t.Errorf("did not expect truncation")
	}
}

func TestGetFile_LineSlice(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: []string{"ts"}, MaxResponseKB: 100,
	})
	var sb strings.Builder
	for i := 1; i <= 20; i++ {
		sb.WriteString("L")
		sb.WriteString(itoaSmall(i))
		sb.WriteString("\n")
	}
	f.writeFile(t, "src/big.ts", sb.String())

	res := callTool(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "src/big.ts",
		"start_line":   5,
		"end_line":     8,
	})
	body := res["body"].(string)
	want := "L5\nL6\nL7\nL8\n"
	if body != want {
		t.Errorf("slice body: got %q, want %q", body, want)
	}
	lines := res["lines"].(map[string]any)
	if int(lines["start"].(float64)) != 5 || int(lines["end"].(float64)) != 8 ||
		int(lines["total"].(float64)) != 20 {
		t.Errorf("lines: %+v", lines)
	}
}

func TestGetFile_Truncation(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: []string{"ts"},
		MaxResponseKB:   1, // ~1024 bytes cap
	})
	// Build a 2KB file so we definitely trip the cap.
	line := strings.Repeat("x", 100) + "\n" // 101 bytes
	var sb strings.Builder
	for i := 0; i < 25; i++ {
		sb.WriteString(line)
	}
	f.writeFile(t, "big.ts", sb.String())

	res := callTool(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "big.ts",
	})
	if trunc, _ := res["truncated"].(bool); !trunc {
		t.Errorf("expected truncated=true; res=%+v", res)
	}
	sz := int(res["size_bytes"].(float64))
	if sz > 1024 {
		t.Errorf("size_bytes %d exceeds 1KB cap", sz)
	}
	if sz == 0 {
		t.Errorf("expected some body to be returned, got 0 bytes")
	}
}

func TestGetFile_Denial_OutsideRoot(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: []string{"ts"}, MaxResponseKB: 100,
	})
	other := t.TempDir()
	outside := filepath.Join(other, "secret.ts")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := callToolExpectError(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         outside,
	})
	if !strings.Contains(res, "outside project_root") {
		t.Errorf("expected 'outside project_root' in error, got %q", res)
	}
	if got := auditRowCount(t, f.auditDB, "response_ok = 0"); got != 1 {
		t.Errorf("expected 1 audit row, got %d", got)
	}
}

func TestGetFile_Denial_ExtensionNotAllowed(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: []string{"ts", "tsx"}, MaxResponseKB: 100,
	})
	f.writeFile(t, "main.py", "print('hi')\n")

	res := callToolExpectError(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "main.py",
	})
	if !strings.Contains(res, "extension") || !strings.Contains(res, "py") {
		t.Errorf("expected extension denial mentioning 'py', got %q", res)
	}
}

func TestGetFile_Denial_DenyGlob(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: nil, // disable extension check to isolate deny-glob
		DenyPaths:       []string{".env*", ".git/**"},
		MaxResponseKB:   100,
	})
	f.writeFile(t, ".env.local", "SECRET=1\n")
	f.writeFile(t, ".git/HEAD", "ref: refs/heads/main\n")

	for _, p := range []string{".env.local", ".git/HEAD"} {
		res := callToolExpectError(t, f.s, "get_file", map[string]any{
			"project_root": f.root,
			"path":         p,
		})
		if !strings.Contains(res, "deny pattern") {
			t.Errorf("path %q: expected deny-pattern error, got %q", p, res)
		}
	}
}

func TestGetFile_Denial_FileNotFound(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{AllowExtensions: []string{"ts"}})
	res := callToolExpectError(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "src/no-such.ts",
	})
	if !strings.Contains(res, "file not found") {
		t.Errorf("expected 'file not found', got %q", res)
	}
}

func TestGetFile_Denial_NotARegularFile(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{AllowExtensions: nil})
	// A directory in place of a file.
	dirPath := filepath.Join(f.root, "subdir")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	res := callToolExpectError(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "subdir",
	})
	if !strings.Contains(res, "not a regular file") {
		t.Errorf("expected 'not a regular file', got %q", res)
	}
}

func TestGetFile_Denial_InvalidLineRange(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{AllowExtensions: []string{"ts"}})
	f.writeFile(t, "x.ts", "a\nb\nc\n")
	res := callToolExpectError(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "x.ts",
		"start_line":   5,
		"end_line":     2,
	})
	if !strings.Contains(res, "invalid line range") {
		t.Errorf("expected 'invalid line range', got %q", res)
	}
}

func TestGetFile_Denial_StartPastEOF(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{AllowExtensions: []string{"ts"}})
	f.writeFile(t, "x.ts", "a\nb\nc\n")
	res := callToolExpectError(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "x.ts",
		"start_line":   10,
	})
	if !strings.Contains(res, "exceeds file") {
		t.Errorf("expected 'exceeds file', got %q", res)
	}
}

func TestGetFile_AuditWrittenOnSuccess(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{
		AllowExtensions: []string{"ts"}, MaxResponseKB: 100,
	})
	f.writeFile(t, "x.ts", "hello\n")
	_ = callTool(t, f.s, "get_file", map[string]any{
		"project_root": f.root,
		"path":         "x.ts",
		"session_id":   "sess-success",
	})
	if got := auditRowCount(t, f.auditDB,
		"tool_name = 'get_file' AND response_ok = 1 AND session_id = ?",
		"sess-success"); got != 1 {
		t.Errorf("expected 1 success audit row for sess-success, got %d", got)
	}
}

func TestGetFile_NotRegisteredWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	database, _ := db.Open(context.Background(), db.Options{Path: filepath.Join(dir, "obs.db")})
	t.Cleanup(func() { database.Close() })
	s, _ := New(Options{
		DB: database, ServerName: "test", ServerVersion: "0",
		GetFileEnabled: false,
	})
	resp := rpcCall(t, s, "tools/list", 1, nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		if raw.(map[string]any)["name"] == "get_file" {
			t.Errorf("get_file registered even though GetFileEnabled=false")
		}
	}
}

func TestGetFile_RegisteredWhenEnabled(t *testing.T) {
	f := newGetFileFixture(t, GetFileOptions{})
	resp := rpcCall(t, f.s, "tools/list", 1, nil)
	tools := resp["result"].(map[string]any)["tools"].([]any)
	found := false
	for _, raw := range tools {
		if raw.(map[string]any)["name"] == "get_file" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("get_file missing from tools/list when GetFileEnabled=true")
	}
}

// itoaSmall is a tiny strconv.Itoa replacement so the test doesn't
// pull strconv just for a couple of numbers.
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// callToolExpectError invokes the named tool and expects the in-band
// `isError: true` envelope. Returns the text of the error message so
// the caller can substring-assert on the deny reason.
func callToolExpectError(t *testing.T, s *Server, name string, args map[string]any) string {
	t.Helper()
	resp := rpcCall(t, s, "tools/call", 1, map[string]any{
		"name": name, "arguments": args,
	})
	result, _ := resp["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("expected isError=true, got result=%v", result)
	}
	content := result["content"].([]any)
	first := content[0].(map[string]any)
	return first["text"].(string)
}
