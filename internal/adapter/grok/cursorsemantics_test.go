package grok

import (
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
)

// TestCursorSemanticsFor pins that the global unified.jsonl token log
// declares itself action-free while per-session updates.jsonl keeps
// ordinary byte-offset semantics.
func TestCursorSemanticsFor(t *testing.T) {
	a := New()
	// roots are [<home>/.grok/sessions, <home>/.grok/logs, ...] per
	// cross-mount home; derive the .grok base from the first.
	base := filepath.Dir(a.WatchPaths()[0])

	tests := []struct {
		name string
		path string
		want adapter.CursorKind
	}{
		{"unified.jsonl emits no actions", filepath.Join(base, "logs", "unified.jsonl"), adapter.CursorNoActions},
		{"session updates.jsonl is a byte offset", filepath.Join(base, "sessions", "s1", "updates.jsonl"), adapter.CursorByteOffset},
		// Decoy: unclaimed shape stays undeclared.
		{"unclaimed", filepath.Join(base, "logs", "other.jsonl"), adapter.CursorByteOffset},
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
