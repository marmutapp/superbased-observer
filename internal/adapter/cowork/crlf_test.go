package cowork

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSessionFile_CRLF_CursorReachesEOF pins the v1.4.54
// watcher-poll-loop regression: Cowork's Windows-side audit.jsonl
// writer uses CRLF line endings, and the original bufio.Scanner +
// `len(raw)+1` byte accounting undercounted each line by 1, leaving
// the parse_cursors row N-1 bytes short of file size. The watcher's
// poll loop then re-fired on every tick, logging "caught up dropped
// writes" forever without progress.
//
// This test builds a synthetic CRLF audit.jsonl, parses it, and
// asserts NewOffset == file size. With the bug it would be
// file size - 2 (number of \r\n endings).
func TestParseSessionFile_CRLF_CursorReachesEOF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Build the local-instance layout the adapter expects:
	// <dir>/local_test/audit.jsonl + <dir>/local_test.json
	instDir := filepath.Join(dir, "local_test")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := `{"sessionId":"local_test","cwd":"/tmp/cw","userSelectedFolders":["/tmp/cw"]}`
	if err := os.WriteFile(filepath.Join(dir, "local_test.json"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	// Three valid records joined with CRLF. Includes a `system` record
	// (no event emitted), a `user` record (emits an action), and a
	// `result` record (emits an action) so the parser exercises real
	// dispatch paths.
	records := []string{
		`{"type":"system","subtype":"init","cwd":"/tmp/cw","session_id":"s1","_audit_timestamp":"2026-05-15T10:00:00.000Z"}`,
		`{"type":"user","uuid":"u1","session_id":"s1","message":{"role":"user","content":"hi"},"_audit_timestamp":"2026-05-15T10:00:01.000Z"}`,
		`{"type":"result","uuid":"r1","session_id":"s1","subtype":"success","duration_ms":1000,"result":"done","total_cost_usd":0.01,"_audit_timestamp":"2026-05-15T10:00:02.000Z"}`,
	}
	body := strings.Join(records, "\r\n") + "\r\n"
	auditPath := filepath.Join(instDir, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	fi, err := os.Stat(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewOffset != fi.Size() {
		t.Fatalf("NewOffset=%d, want %d (file size). Off by %d — the v1.4.54 CRLF cursor stall regression.",
			res.NewOffset, fi.Size(), fi.Size()-res.NewOffset)
	}
	// Sanity: parser still emitted user_prompt + result events.
	if len(res.ToolEvents) < 2 {
		t.Fatalf("expected >= 2 ToolEvents, got %d", len(res.ToolEvents))
	}
}

// TestParseSessionFile_TrailingPartialLine_DoesNotAdvance pins that
// when the file has a partial trailing line (no \n), the parser
// stops short and leaves the cursor before the partial bytes.
// Cowork is actively writing to these files; advancing past a
// half-written record would skip its content on the next poll.
func TestParseSessionFile_TrailingPartialLine_DoesNotAdvance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	instDir := filepath.Join(dir, "local_partial")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One complete record then a partial line missing its \n.
	full := `{"type":"user","uuid":"u1","session_id":"s1","message":{"role":"user","content":"a"},"_audit_timestamp":"2026-05-15T10:00:00.000Z"}` + "\r\n"
	partial := `{"type":"user","uuid":"u2","session_id":"s1","mess` // no terminator
	body := full + partial
	auditPath := filepath.Join(instDir, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// Expected cursor: end of the FIRST line only (length of `full`).
	// The partial trailing line is held back for the next poll.
	if got, want := res.NewOffset, int64(len(full)); got != want {
		t.Fatalf("NewOffset=%d, want %d (boundary before partial trailing line)", got, want)
	}
}

// TestParseSessionFile_TrailingEmptyLine_AdvancesPastIt pins the
// other half of the cursor-stall fix: an empty line (just \r\n) at
// EOF must advance the cursor past those bytes. Pre-fix the
// `len(raw) == 0 { continue }` branch skipped the NewOffset update.
func TestParseSessionFile_TrailingEmptyLine_AdvancesPastIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	instDir := filepath.Join(dir, "local_empty")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","uuid":"u1","session_id":"s1","message":{"role":"user","content":"a"},"_audit_timestamp":"2026-05-15T10:00:00.000Z"}` + "\r\n\r\n"
	auditPath := filepath.Join(instDir, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), auditPath, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	fi, _ := os.Stat(auditPath)
	if res.NewOffset != fi.Size() {
		t.Fatalf("NewOffset=%d, want %d. The trailing empty CRLF line must advance the cursor.",
			res.NewOffset, fi.Size())
	}
}
