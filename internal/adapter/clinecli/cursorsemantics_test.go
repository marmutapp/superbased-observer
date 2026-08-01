package clinecli

import (
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// TestCursorSemanticsFor pins cline-cli's per-path split: the SQLite
// store is watermark-scanned, hooks.jsonl is a real byte-offset tail.
func TestCursorSemanticsFor(t *testing.T) {
	a := New()
	root := a.WatchPaths()[0]

	tests := []struct {
		name string
		path string
		want adapter.CursorKind
	}{
		{"sessions.db is a watermark", filepath.Join(root, "data", "db", "sessions.db"), adapter.CursorWatermark},
		{"wal sidecar is a watermark", filepath.Join(root, "data", "db", "sessions.db-wal"), adapter.CursorWatermark},
		{"shm sidecar is a watermark", filepath.Join(root, "data", "db", "sessions.db-shm"), adapter.CursorWatermark},
		{"hooks.jsonl is a byte offset", filepath.Join(root, "data", "hooks.jsonl"), adapter.CursorByteOffset},
		// Decoy: a path this adapter does not claim must not receive a
		// declaration at all.
		{"unclaimed path", filepath.Join(root, "not-a-session.txt"), adapter.CursorByteOffset},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := a.CursorSemanticsFor(tc.path)
			if got.Kind != tc.want {
				t.Errorf("Kind = %v, want %v (IsSessionFile=%v)", got.Kind, tc.want, a.IsSessionFile(tc.path))
			}
		})
	}
}
