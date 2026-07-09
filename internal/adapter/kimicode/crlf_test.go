package kimicode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCRLFAndEmptyLines verifies the byte cursor advances by the exact
// terminator length across CRLF line endings and blank lines, so a
// Windows-authored wire trace parses identically to a Unix one and the
// committed offset equals the file size.
func TestCRLFAndEmptyLines(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sessions", "wd_crlf_0011", "session_crlf-0000-4000-8000-000000000000", "agents", "main")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type": "metadata", "protocol_version": "1.4", "created_at": 1783570000000}`,
		``, // blank line between records
		`{"type": "turn.prompt", "input": [{"type": "text", "text": "hi"}], "origin": {"kind": "user"}, "time": 1783570000100}`,
		`{"type": "usage.record", "model": "openai/gpt-4o", "usage": {"inputOther": 10, "output": 2, "inputCacheRead": 0, "inputCacheCreation": 0}, "usageScope": "turn", "time": 1783570000200}`,
	}
	body := ""
	for _, l := range lines {
		body += l + "\r\n" // CRLF terminators
	}
	wire := filepath.Join(sessDir, "wire.jsonl")
	if err := os.WriteFile(wire, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, filepath.Join(dir, "sessions"))
	res, err := a.ParseSessionFile(context.Background(), wire, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(wire)
	if res.NewOffset != fi.Size() {
		t.Fatalf("offset %d != file size %d — cursor mis-advanced on CRLF/empty lines", res.NewOffset, fi.Size())
	}
	if len(res.TokenEvents) != 1 {
		t.Fatalf("token events = %d, want 1", len(res.TokenEvents))
	}
	if toolByAction(res, "user_prompt") == nil {
		t.Fatal("prompt not parsed under CRLF")
	}
}
