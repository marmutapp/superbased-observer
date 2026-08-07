package primeagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCRLFAndBlankLinesAdvanceTheCursorExactly is the
// [[feedback-jsonl-parser-cursor]] pin. bufio.Scanner strips the line
// terminator, so `len(line)+1` arithmetic silently under-counts by one
// byte on every CRLF line and stalls forever on a blank one. The parser
// uses bufio.Reader.ReadString('\n') and adds the FULL returned length,
// so a CRLF transcript's cursor must still land exactly on the file size
// and must produce byte-identical events to the LF form.
//
// This is not hypothetical for prime-agent: Prime Agent ships a Windows
// build (docs/windows.md) writing to %USERPROFILE%\.prime, and a WSL2
// observer reads exactly those files over /mnt/c.
func TestCRLFAndBlankLinesAdvanceTheCursorExactly(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "primeagent", "session-flat.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// LF → CRLF, plus a blank CRLF line spliced into the middle.
	lines := strings.SplitAfter(string(body), "\n")
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

	root := filepath.Join(t.TempDir(), ".prime", "agent", "sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	crlfPath := filepath.Join(root, "019f0000-1111-7222-8333-444444444444.jsonl")
	if err := os.WriteFile(crlfPath, []byte(crlf), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	lfPath := filepath.Join(root, "019f0000-1111-7222-8333-555555555555.jsonl")
	if err := os.WriteFile(lfPath, body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	a := NewWithOptions(nil, root)
	crlfRes, err := a.ParseSessionFile(context.Background(), crlfPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile(crlf): %v", err)
	}
	if crlfRes.NewOffset != int64(len(crlf)) {
		t.Errorf("NewOffset = %d, want the CRLF file size %d — the cursor drifted by %d bytes",
			crlfRes.NewOffset, len(crlf), int64(len(crlf))-crlfRes.NewOffset)
	}

	lfRes, err := a.ParseSessionFile(context.Background(), lfPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile(lf): %v", err)
	}
	if len(crlfRes.ToolEvents) != len(lfRes.ToolEvents) {
		t.Fatalf("CRLF produced %d tool events, LF produced %d", len(crlfRes.ToolEvents), len(lfRes.ToolEvents))
	}
	if len(crlfRes.TokenEvents) != len(lfRes.TokenEvents) {
		t.Fatalf("CRLF produced %d token events, LF produced %d", len(crlfRes.TokenEvents), len(lfRes.TokenEvents))
	}
	for i := range lfRes.ToolEvents {
		lf, cr := lfRes.ToolEvents[i], crlfRes.ToolEvents[i]
		if lf.ActionType != cr.ActionType || lf.Target != cr.Target || lf.Success != cr.Success {
			t.Errorf("event %d differs across line endings:\n  lf   = %s | %s | %v\n  crlf = %s | %s | %v",
				i, lf.ActionType, lf.Target, lf.Success, cr.ActionType, cr.Target, cr.Success)
		}
	}
}
