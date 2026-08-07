package muse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCRLFAndBlankLinesAdvanceTheCursorExactly is the [[feedback-jsonl-parser-cursor]]
// pin. bufio.Scanner strips the line terminator, so `len(line)+1` arithmetic
// silently under-counts by one byte on every CRLF line and loops forever on
// a blank one. The parser uses bufio.Reader.ReadString('\n') and adds the
// FULL returned length, so a CRLF file's cursor must still land exactly on
// the file size and every event must still be produced.
func TestCRLFAndBlankLinesAdvanceTheCursorExactly(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "muse", "simple-session.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	lf := string(body)
	// LF → CRLF, plus a blank CRLF line spliced into the middle.
	lines := strings.SplitAfter(lf, "\n")
	var b strings.Builder
	for i, line := range lines {
		if line == "" {
			continue
		}
		b.WriteString(strings.TrimSuffix(line, "\n"))
		b.WriteString("\r\n")
		if i == len(lines)/2 {
			b.WriteString("\r\n")
		}
	}
	crlf := b.String()

	root := filepath.Join(t.TempDir(), "muse", "sessions")
	dir := filepath.Join(root, "2026", "08", "06", "11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(dir, sessionLogName)
	if err := os.WriteFile(logPath, []byte(crlf), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := NewWithOptions(nil, root)
	res, err := a.ParseSessionFile(context.Background(), logPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if res.NewOffset != int64(len(crlf)) {
		t.Errorf("NewOffset = %d, want the CRLF file size %d — the cursor drifted by %d bytes",
			res.NewOffset, len(crlf), int64(len(crlf))-res.NewOffset)
	}

	// The CRLF parse must produce exactly the same events as the LF one.
	lfPath := filepath.Join(dir, "lf-"+sessionLogName)
	if err := os.WriteFile(lfPath, body, 0o600); err != nil {
		t.Fatalf("write lf: %v", err)
	}
	// lf-session.jsonl is deliberately NOT a session-file shape, so parse it
	// through the same code path by copying it over the real name instead.
	if err := os.WriteFile(logPath, body, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	lfRes, err := a.ParseSessionFile(context.Background(), logPath, 0)
	if err != nil {
		t.Fatalf("lf parse: %v", err)
	}
	if len(lfRes.ToolEvents) != len(res.ToolEvents) || len(lfRes.TokenEvents) != len(res.TokenEvents) {
		t.Fatalf("CRLF parse produced %d/%d events, LF produced %d/%d",
			len(res.ToolEvents), len(res.TokenEvents),
			len(lfRes.ToolEvents), len(lfRes.TokenEvents))
	}
	for i := range lfRes.ToolEvents {
		if lfRes.ToolEvents[i].SourceEventID != res.ToolEvents[i].SourceEventID {
			t.Errorf("event %d id differs between LF and CRLF: %q vs %q",
				i, lfRes.ToolEvents[i].SourceEventID, res.ToolEvents[i].SourceEventID)
		}
	}
	if len(res.ToolEvents) == 0 {
		t.Error("the CRLF parse produced nothing — the comparison is vacuous")
	}
}

// TestTrailingPartialLineIsDeferred pins that a record still being written
// (no terminating newline) is NOT consumed, so the next parse re-reads it
// whole.
func TestTrailingPartialLineIsDeferred(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "muse", "malformed.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	partial := string(body) + `{"schema_version":1,"payload_type":"runtime.session","payload":{"eve`

	root := filepath.Join(t.TempDir(), "muse", "sessions")
	dir := filepath.Join(root, "2026", "08", "06", "11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(dir, sessionLogName)
	if err := os.WriteFile(logPath, []byte(partial), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := NewWithOptions(nil, root).ParseSessionFile(context.Background(), logPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if res.NewOffset != int64(len(body)) {
		t.Errorf("NewOffset = %d, want %d (the offset BEFORE the partial line)", res.NewOffset, len(body))
	}
}
