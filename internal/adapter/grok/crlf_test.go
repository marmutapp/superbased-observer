package grok

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestParseUpdatesCRLF verifies the byte cursor accounts for CRLF line
// terminators (bufio.Reader.ReadString keeps the '\r\n', so len(line)
// arithmetic stays correct on Windows-written bundles) and that an empty
// interior line advances the offset instead of stalling the poll loop.
func TestParseUpdatesCRLF(t *testing.T) {
	dir := t.TempDir()
	// Minimal CRLF updates.jsonl: a user_message_chunk, an empty line, and
	// a partial (unterminated) trailing line that must be deferred.
	body := "" +
		`{"timestamp":1783555342,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hi"},"_meta":{"modelId":"grok-4.5"}},"_meta":{"eventId":"s1-1","agentTimestampMs":1783555342000}}}` + "\r\n" +
		"\r\n" +
		`{"timestamp":1783555343,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"`

	sub := filepath.Join(dir, ".grok", "sessions", "enc", "s1")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "updates.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, filepath.Join(dir, ".grok", "sessions"))
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// The offset must stop at the start of the unterminated final line, so a
	// later parse resumes there once the line is completed.
	firstTwoLines := len(body) - len(`{"timestamp":1783555343,"method":"session/update","params":{"sessionId":"s1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"`)
	if res.NewOffset != int64(firstTwoLines) {
		t.Errorf("NewOffset = %d, want %d (deferred partial line)", res.NewOffset, firstTwoLines)
	}
	// The one complete user_message_chunk yields a session_start + a prompt.
	if len(res.ToolEvents) == 0 {
		t.Fatal("expected events from the terminated CRLF line")
	}
}
