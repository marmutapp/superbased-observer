package hermes

import (
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// TestCursorSemanticsFor pins that every file hermes claims declares a
// watermark cursor — state.db AND its fsnotify sidecars, which is what
// stopped three live rows reading "behind" for 16.5 days.
func TestCursorSemanticsFor(t *testing.T) {
	a := New()
	root := a.WatchPaths()[0]

	tests := []struct {
		name string
		path string
		want adapter.CursorKind
	}{
		{"state.db", filepath.Join(root, "state.db"), adapter.CursorWatermark},
		{"state.db-wal", filepath.Join(root, "state.db-wal"), adapter.CursorWatermark},
		{"state.db-shm", filepath.Join(root, "state.db-shm"), adapter.CursorWatermark},
		// Decoy: an unclaimed path gets no declaration.
		{"unclaimed", filepath.Join(root, "other.db"), adapter.CursorByteOffset},
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
