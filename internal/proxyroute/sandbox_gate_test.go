package proxyroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// TestSandboxGate_PinnedHomeSuppressesCrossOSRoutes is the proxy-route half of
// the 2026-07-31 incident fix (see crossmount.AutoDetectSuppressed). The
// cross-OS route writers resolve their target through crossmount, NOT through
// HomeDir — so a caller that pinned HomeDir (a test, a sandboxed generator)
// could have its `settings.json` env write land in the operator's REAL
// Windows-side .claude. Under a pinned home with no Windows override the
// writers must SKIP (benign, no error, no ConfigPath) and the virtual targets
// must not surface.
func TestSandboxGate_PinnedHomeSuppressesCrossOSRoutes(t *testing.T) {
	forceWSL(t, true)
	winClaude, winCodex := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(winClaude, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(winCodex, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{
		{OS: crossmount.OSWindows, Path: winClaude},
		{OS: crossmount.OSWindows, Path: winCodex},
	})
	// Own BOTH homes: without the gate these would resolve cleanly and be
	// written — exactly the incident shape.
	prevOwn := windowsUserName
	windowsUserName = func() string { return filepath.Base(winClaude) }
	t.Cleanup(func() { windowsUserName = prevOwn })

	r, err := NewRegistrar(RegisterOptions{
		ProxyPort: 8820,
		HomeDir:   t.TempDir(), // the sandbox pin
		Force:     true,        // --force must NOT lift the gate
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range []RegistrationResult{r.RegisterClaudeCodeWindows(), r.RegisterCodexWindows()} {
		if res.Error != nil {
			t.Errorf("%s: want a benign skip, got error %v", res.Tool, res.Error)
		}
		if !res.ConfigMissing || res.Added {
			t.Errorf("%s: want a clean skip, got %+v", res.Tool, res)
		}
		if res.ConfigPath != "" {
			t.Errorf("%s: exposed a write path %q — a sandboxed caller must get none", res.Tool, res.ConfigPath)
		}
		if !strings.Contains(res.SkipReason, "2026-07-31") {
			t.Errorf("%s: SkipReason should name the incident, got %q", res.Tool, res.SkipReason)
		}
	}
	if tgts := r.WindowsRouteTargets(); len(tgts) != 0 {
		t.Errorf("WindowsRouteTargets under a pinned home = %v, want none", tgts)
	}
	if cands := r.WindowsRouteCandidates(); len(cands) != 0 {
		t.Errorf("WindowsRouteCandidates under a pinned home = %v, want none", cands)
	}
	// Nothing was written into either would-be target.
	if _, err := os.Stat(filepath.Join(winClaude, ".claude", "settings.json")); err == nil {
		t.Error("settings.json written into the auto-detected Windows home")
	}
	if _, err := os.Stat(filepath.Join(winCodex, ".codex", "config.toml")); err == nil {
		t.Error("config.toml written into the auto-detected Windows home")
	}
}

// TestSandboxGate_RouteProductionPathUnchanged is the production-parity pin:
// with NO home override (what `observer init` does), the cross-OS route target
// resolves through crossmount exactly as before the gate.
func TestSandboxGate_RouteProductionPathUnchanged(t *testing.T) {
	forceWSL(t, true)
	winHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(winHome, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	forceHomes(t, []crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}})
	forceWindowsUser(t, filepath.Base(winHome))

	// HomeDir "" is the production shape: NewRegistrar fills in the real
	// $HOME and homeOverride stays empty, so the gate is inert.
	r, err := NewRegistrar(RegisterOptions{ProxyPort: 8820})
	if err != nil {
		t.Fatal(err)
	}
	dir, refuse := r.resolveWindowsHomeFor("", ".claude")
	if want := filepath.Join(winHome, ".claude"); dir != want || refuse != nil {
		t.Errorf("production resolve = (%q, %v), want (%q, nil) — auto-detect must be unchanged", dir, refuse, want)
	}
	tgts := r.WindowsRouteTargets()
	if len(tgts) != 1 || tgts[0] != "claude-code-windows" {
		t.Errorf("production WindowsRouteTargets = %v, want [claude-code-windows]", tgts)
	}
}

// TestSandboxGate_UncontainedRouteOverrideSkips is the proxy-route half of the
// codex NO-SHIP scenario: a sandboxed caller whose Windows-home override
// points at an INDEPENDENT root must be refused. The first cut honoured any
// non-empty override, so `HomeDir=/tmp/sandbox` +
// `WindowsClaudeHome=/mnt/c/Users/operator` still rewrote the operator's real
// settings.json.
func TestSandboxGate_UncontainedRouteOverrideSkips(t *testing.T) {
	forceWSL(t, true)
	forceHomes(t, nil) // auto-detect finds nothing
	outside := t.TempDir()
	for _, sub := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(outside, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r, err := NewRegistrar(RegisterOptions{
		ProxyPort:         8820,
		HomeDir:           t.TempDir(), // a DIFFERENT root
		WindowsClaudeHome: outside,
		WindowsCodexHome:  outside,
		Force:             true, // --force must NOT lift containment
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range []RegistrationResult{r.RegisterClaudeCodeWindows(), r.RegisterCodexWindows()} {
		if res.Error != nil || !res.ConfigMissing || res.Added || res.ConfigPath != "" {
			t.Errorf("%s: uncontained override must skip cleanly, got %+v", res.Tool, res)
		}
		if !strings.Contains(res.SkipReason, "OUTSIDE") {
			t.Errorf("%s: SkipReason should name the uncontained override, got %q", res.Tool, res.SkipReason)
		}
	}
	if tgts := r.WindowsRouteTargets(); len(tgts) != 0 {
		t.Errorf("WindowsRouteTargets with an uncontained override = %v, want none", tgts)
	}
	if _, err := os.Stat(filepath.Join(outside, ".claude", "settings.json")); err == nil {
		t.Error("settings.json written into the uncontained override home")
	}
	if _, err := os.Stat(filepath.Join(outside, ".codex", "config.toml")); err == nil {
		t.Error("config.toml written into the uncontained override home")
	}
}
