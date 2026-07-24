package hook

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// forceHookCrossmount pins the hook package's crossmount seams (restored on
// cleanup) so the owned-vs-unowned auto-detect branches of detectWindowsHome
// are deterministic without a real /mnt/c mount or a cmd.exe interop shell.
// owned maps a Windows USER home path to its ownership verdict.
func forceHookCrossmount(t *testing.T, homes []crossmount.HomeRoot, owned map[string]bool) {
	t.Helper()
	prevHomes, prevOwned := allHomes, homeOwnedByCurrentWindowsUser
	allHomes = func() []crossmount.HomeRoot { return homes }
	homeOwnedByCurrentWindowsUser = func(home string) bool { return owned[home] }
	t.Cleanup(func() {
		allHomes = prevHomes
		homeOwnedByCurrentWindowsUser = prevOwned
	})
}

// mkWinConfigHome creates <home>/<subdir> and returns the home dir.
func mkWinConfigHome(t *testing.T, subdir string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, subdir), 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestDetectWindowsHome_OwnedAutoDetected: a single auto-detected Windows home
// carrying the config dir, PROVEN to belong to the current Windows user, is
// accepted — and surfaces in Installed() (F1: the hook registrar now honours
// the same R1 ownership guard as the proxy-route writer).
func TestDetectWindowsHome_OwnedAutoDetected(t *testing.T) {
	winHome := mkWinConfigHome(t, ".claude")
	forceHookCrossmount(
		t,
		[]crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}},
		map[string]bool{winHome: true},
	)
	r, err := NewRegistry(Options{BinaryPath: "/bin/observer", HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.detectWindowsClaudeHome(); got != filepath.Join(winHome, ".claude") {
		t.Errorf("detectWindowsClaudeHome() = %q, want owned home", got)
	}
	if !containsString(r.Installed(), "claude-code-windows") {
		t.Errorf("Installed() = %v, want it to include claude-code-windows for the owned home", r.Installed())
	}
}

// TestDetectWindowsHome_UnownedRefused: an auto-detected Windows home whose
// ownership cannot be proven (interop off, or another user's home) is REFUSED —
// detect returns "" and the virtual target does NOT appear in Installed(). This
// is the F1 fix: one `observer init` run must not install hooks into another
// user's .claude just because it is the only one mounted.
func TestDetectWindowsHome_UnownedRefused(t *testing.T) {
	winHome := mkWinConfigHome(t, ".claude")
	forceHookCrossmount(
		t,
		[]crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}},
		map[string]bool{winHome: false}, // ownership unverifiable / another user
	)
	r, err := NewRegistry(Options{BinaryPath: "/bin/observer", HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.detectWindowsClaudeHome(); got != "" {
		t.Errorf("detectWindowsClaudeHome() = %q, want empty (unowned refused)", got)
	}
	if containsString(r.Installed(), "claude-code-windows") {
		t.Errorf("Installed() = %v, must NOT include claude-code-windows for an unowned home", r.Installed())
	}
}

// TestDetectWindowsHome_AmbiguousRefused: two owned candidates (a shape the
// ownership match makes near-impossible, but guarded anyway) refuse to
// auto-pick — detect returns "" rather than guessing, mirroring the
// proxy-route writer.
func TestDetectWindowsHome_AmbiguousRefused(t *testing.T) {
	homeA := mkWinConfigHome(t, ".cursor")
	homeB := mkWinConfigHome(t, ".cursor")
	forceHookCrossmount(
		t,
		[]crossmount.HomeRoot{
			{OS: crossmount.OSWindows, Path: homeA},
			{OS: crossmount.OSWindows, Path: homeB},
		},
		map[string]bool{homeA: true, homeB: true},
	)
	r, err := NewRegistry(Options{BinaryPath: "/bin/observer", HomeDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.detectWindowsCursorHome(); got != "" {
		t.Errorf("detectWindowsCursorHome() = %q, want empty (ambiguous refused)", got)
	}
}

// TestDetectWindowsHome_OverrideWinsUnconditionally: an explicit override is
// authoritative even when the ownership seam would refuse every auto-detected
// home — the operator named the home, so the guard is moot.
func TestDetectWindowsHome_OverrideWinsUnconditionally(t *testing.T) {
	override := t.TempDir()
	forceHookCrossmount(t, nil, map[string]bool{}) // nothing owned
	r, err := NewRegistry(Options{
		BinaryPath:        "/bin/observer",
		HomeDir:           t.TempDir(),
		WindowsClaudeHome: override,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.detectWindowsClaudeHome(); got != filepath.Join(override, ".claude") {
		t.Errorf("detectWindowsClaudeHome() = %q, want the override home", got)
	}
	if !containsString(r.Installed(), "claude-code-windows") {
		t.Errorf("Installed() = %v, want claude-code-windows for the explicit override", r.Installed())
	}
}
