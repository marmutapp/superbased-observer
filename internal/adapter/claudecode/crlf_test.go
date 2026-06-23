package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseSessionFile_CRLF_CursorReachesEOF pins the v1.4.54-era
// watcher-poll-loop regression class on the claudecode path,
// mirroring the cowork fix shape: Windows-side / cross-mount JSONL
// writers may emit CRLF line endings, and the original bufio.Scanner +
// `len(raw)+1` byte accounting undercounted each line by 1, leaving
// the parse_cursors row N-1 bytes short of file size. The watcher's
// poll loop then re-fired forever without progress.
//
// Builds a synthetic CRLF claudecode session file, parses it, asserts
// NewOffset == file size. With the bug it would be file size - N
// (one byte short per CRLF terminator).
func TestParseSessionFile_CRLF_CursorReachesEOF(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	records := []string{
		`{"type":"user","sessionId":"s","cwd":"/tmp","uuid":"u-1","timestamp":"2026-05-15T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"assistant","sessionId":"s","cwd":"/tmp","uuid":"u-2","timestamp":"2026-05-15T10:00:01Z","message":{"id":"msg_a","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"/x.go"}}]}}`,
		`{"type":"user","sessionId":"s","cwd":"/tmp","uuid":"u-3","timestamp":"2026-05-15T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok","is_error":false}]}}`,
	}
	body := strings.Join(records, "\r\n") + "\r\n"
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := New().ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.NewOffset != fi.Size() {
		t.Fatalf("NewOffset=%d, want %d (file size). Off by %d — CRLF cursor stall regression.",
			res.NewOffset, fi.Size(), fi.Size()-res.NewOffset)
	}
	// Sanity: the parser still emitted the user_prompt + tool_use rows.
	if len(res.ToolEvents) < 2 {
		t.Fatalf("expected >= 2 ToolEvents, got %d", len(res.ToolEvents))
	}
}

// TestParseSessionFile_TrailingPartialLine_DoesNotAdvance pins that
// when the file has a partial trailing line (no terminator), the
// parser stops short and leaves the cursor before the partial bytes.
// Claude Code may still be writing — advancing past a half-written
// record would skip its content on the next poll.
func TestParseSessionFile_TrailingPartialLine_DoesNotAdvance(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	full := `{"type":"user","sessionId":"s","cwd":"/tmp","uuid":"u-1","timestamp":"2026-05-15T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"a"}]}}` + "\r\n"
	partial := `{"type":"user","sessionId":"s","cwd":"/tmp","uuid":"u-2","timestamp":"2026-05-15T10:00:01Z","mess` // no terminator
	body := full + partial
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := New().ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got, want := res.NewOffset, int64(len(full)); got != want {
		t.Fatalf("NewOffset=%d, want %d (boundary before partial trailing line)", got, want)
	}
}

// TestParseSessionFile_TrailingEmptyLine_AdvancesPastIt pins the
// other half of the cursor-stall fix: an empty line (just \r\n or
// \n) at EOF must advance the cursor past those bytes. Pre-fix the
// `if len(raw) == 0 { continue }` branch skipped the NewOffset
// update, leaving the cursor stranded and the watcher repolling
// forever.
func TestParseSessionFile_TrailingEmptyLine_AdvancesPastIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := `{"type":"user","sessionId":"s","cwd":"/tmp","uuid":"u-1","timestamp":"2026-05-15T10:00:00Z","message":{"role":"user","content":[{"type":"text","text":"a"}]}}` + "\r\n\r\n"
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := New().ParseSessionFile(context.Background(), p, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	fi, _ := os.Stat(p)
	if res.NewOffset != fi.Size() {
		t.Fatalf("NewOffset=%d, want %d. The trailing empty CRLF line must advance the cursor.",
			res.NewOffset, fi.Size())
	}
}
