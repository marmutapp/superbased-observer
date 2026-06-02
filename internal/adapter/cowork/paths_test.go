package cowork

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// TestCandidateRoots_Windows pins the per-Windows-home expansion:
//   - MSIX-globbed path under Local/Packages/Claude_*/
//   - Non-MSIX path under Roaming/Claude/
//
// We materialize a fake Windows home tree in a t.TempDir so the
// filepath.Glob in candidateRoots has something to match.
func TestCandidateRoots_Windows(t *testing.T) {
	t.Parallel()
	home := t.TempDir()

	// Build the MSIX subtree: <home>/AppData/Local/Packages/Claude_<hash>/LocalCache/Roaming/Claude/local-agent-mode-sessions
	msixA := filepath.Join(home, "AppData", "Local", "Packages", "Claude_aaaa", "LocalCache", "Roaming", "Claude", "local-agent-mode-sessions")
	msixB := filepath.Join(home, "AppData", "Local", "Packages", "Claude_bbbb", "LocalCache", "Roaming", "Claude", "local-agent-mode-sessions")
	for _, d := range []string{msixA, msixB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	got := candidateRoots(crossmount.HomeRoot{Path: home, OS: crossmount.OSWindows})

	// Expect: 2 MSIX matches + 1 non-MSIX fallback path (unconditional, even though
	// it doesn't exist — registry filters non-existent paths later).
	if len(got) != 3 {
		t.Fatalf("candidateRoots(windows)=%v want 3 entries", got)
	}
	// Both MSIX paths should be present.
	foundA, foundB := false, false
	for _, r := range got {
		switch r {
		case msixA:
			foundA = true
		case msixB:
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Fatalf("MSIX matches missing: foundA=%v foundB=%v got=%v", foundA, foundB, got)
	}
	// Non-MSIX fallback is the last entry.
	wantNonMSIX := filepath.Join(home, "AppData", "Roaming", "Claude", "local-agent-mode-sessions")
	if got[len(got)-1] != wantNonMSIX {
		t.Fatalf("non-MSIX fallback expected last; got[-1]=%q want %q", got[len(got)-1], wantNonMSIX)
	}
}

// TestCandidateRoots_WindowsNoMSIX pins the non-MSIX-only case: no
// Claude_* package directories exist, only the non-MSIX fallback
// is returned (unconditionally, since the watcher filters by stat
// at register time).
func TestCandidateRoots_WindowsNoMSIX(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	got := candidateRoots(crossmount.HomeRoot{Path: home, OS: crossmount.OSWindows})
	if len(got) != 1 {
		t.Fatalf("candidateRoots(windows-empty)=%v want 1 (non-MSIX fallback)", got)
	}
	wantSuffix := filepath.Join("AppData", "Roaming", "Claude", "local-agent-mode-sessions")
	if !strings.HasSuffix(got[0], wantSuffix) {
		t.Fatalf("got %q lacks non-MSIX suffix %q", got[0], wantSuffix)
	}
}

// TestCandidateRoots_Darwin pins the macOS path.
func TestCandidateRoots_Darwin(t *testing.T) {
	t.Parallel()
	home := "/Users/test"
	got := candidateRoots(crossmount.HomeRoot{Path: home, OS: crossmount.OSDarwin})
	if len(got) != 1 {
		t.Fatalf("candidateRoots(darwin)=%v want 1 entry", got)
	}
	want := filepath.Join(home, "Library", "Application Support", "Claude", "local-agent-mode-sessions")
	if got[0] != want {
		t.Fatalf("candidateRoots(darwin)[0]=%q want %q", got[0], want)
	}
}

// TestCandidateRoots_Linux returns nil (no known Cowork install path).
func TestCandidateRoots_Linux(t *testing.T) {
	t.Parallel()
	got := candidateRoots(crossmount.HomeRoot{Path: "/home/test", OS: crossmount.OSLinux})
	if got != nil {
		t.Fatalf("candidateRoots(linux)=%v want nil", got)
	}
}

// TestWatchPaths_OverrideWins pins that an explicit watchRoots passed
// to NewWithOptions bypasses crossmount discovery entirely. Used by
// tests pointing at testdata fixtures.
func TestWatchPaths_OverrideWins(t *testing.T) {
	t.Parallel()
	override := []string{"/synthetic/override"}
	a := NewWithOptions(nil, override...)
	got := a.WatchPaths()
	if len(got) != 1 || got[0] != override[0] {
		t.Fatalf("WatchPaths=%v want %v", got, override)
	}
}
