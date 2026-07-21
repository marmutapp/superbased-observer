package kirocli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFlatBundleCRLFSafe asserts the flat-bundle stream parser tolerates
// Windows CRLF line endings and interspersed blank lines — Kiro's
// `.jsonl` is written natively on Windows too, so a WSL2 observer
// reading /mnt/c may see CRLF.
func TestFlatBundleCRLFSafe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".kiro", "sessions", "cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "sess-crlf"
	lines := []string{
		`{"version":"v1","kind":"Prompt","data":{"message_id":"u1","content":[{"kind":"text","data":"hi"}],"meta":{"timestamp":1783600000}}}`,
		``, // blank line between records
		`{"version":"v1","kind":"AssistantMessage","data":{"message_id":"a1","content":[{"kind":"text","data":"yo"}]}}`,
	}
	body := strings.Join(lines, "\r\n") + "\r\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), filepath.Join(dir, id+".jsonl"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 2 {
		t.Fatalf("CRLF parse produced %d events, want 2; warnings=%v", len(res.ToolEvents), res.Warnings)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("CRLF/blank lines should not warn: %v", res.Warnings)
	}
}
