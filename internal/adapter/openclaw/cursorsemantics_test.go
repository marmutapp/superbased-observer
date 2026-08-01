package openclaw

import (
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// TestCursorSemanticsFor pins openclaw's three-shape split: the task
// SQLite store and the rewritten-in-place sessions.json index are
// watermark-scanned; the trajectory trace carries token usage only;
// the per-session message log stays a plain byte-offset tail.
func TestCursorSemanticsFor(t *testing.T) {
	a := New()
	root := a.WatchPaths()[0]
	sessions := filepath.Join(root, "agents", "main", "sessions")

	tests := []struct {
		name string
		path string
		want adapter.CursorKind
	}{
		{"runs.sqlite is a watermark", filepath.Join(root, "tasks", "runs.sqlite"), adapter.CursorWatermark},
		{"runs.sqlite-wal is a watermark", filepath.Join(root, "tasks", "runs.sqlite-wal"), adapter.CursorWatermark},
		{"sessions.json is a watermark", filepath.Join(sessions, "sessions.json"), adapter.CursorWatermark},
		{"trajectory emits no actions", filepath.Join(sessions, "abc.trajectory.jsonl"), adapter.CursorNoActions},
		{"message log is a byte offset", filepath.Join(sessions, "abc.jsonl"), adapter.CursorByteOffset},
		// Decoy: unclaimed shapes stay undeclared.
		{"unclaimed", filepath.Join(sessions, "notes.txt"), adapter.CursorByteOffset},
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
