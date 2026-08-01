package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

// TestSandboxGate_PinnedHomeSuppressesCrossOSAutoDetect is the structural half
// of the 2026-07-31 incident fix (see crossmount.AutoDetectSuppressed): a
// caller that pinned Options.HomeDir and named NO Windows-side home must get
// NO crossmount auto-detection — the "-windows" virtual targets disappear from
// Installed() and Register() reports an explicit SKIP with an empty
// ConfigPath, so there is no path outside the sandbox it could ever write.
//
// The crossmount seams are faked, so the "would have been detected" home is a
// temp dir standing in for /mnt/c/Users/<u> — this test never touches a real
// mount.
func TestSandboxGate_PinnedHomeSuppressesCrossOSAutoDetect(t *testing.T) {
	winClaude := mkWinConfigHome(t, ".claude")
	winCursor := mkWinConfigHome(t, ".cursor")
	forceHookCrossmount(
		t,
		[]crossmount.HomeRoot{
			{OS: crossmount.OSWindows, Path: winClaude},
			{OS: crossmount.OSWindows, Path: winCursor},
		},
		map[string]bool{winClaude: true, winCursor: true},
	)

	sandbox := t.TempDir()
	for _, force := range []bool{false, true} {
		r, err := NewRegistry(Options{
			BinaryPath:    "/bin/observer",
			HomeDir:       sandbox,
			ChecksumsPath: filepath.Join(sandbox, ".observer", "hook_checksums.json"),
			WSLDistro:     "Ubuntu",
			Force:         force, // --force must NOT lift the gate
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.detectWindowsClaudeHome(); got != "" {
			t.Errorf("force=%v detectWindowsClaudeHome() = %q, want \"\" (sandbox gate)", force, got)
		}
		if got := r.detectWindowsCursorHome(); got != "" {
			t.Errorf("force=%v detectWindowsCursorHome() = %q, want \"\" (sandbox gate)", force, got)
		}
		for _, tool := range []string{"claude-code-windows", "cursor-windows"} {
			if containsString(r.Installed(), tool) {
				t.Errorf("force=%v Installed() = %v, must not surface %s under a pinned home", force, r.Installed(), tool)
			}
			res := r.Register(tool)
			if res.Error != nil {
				t.Errorf("force=%v Register(%s) errored instead of skipping: %v", force, tool, res.Error)
			}
			if !res.Skipped {
				t.Errorf("force=%v Register(%s) = %+v, want Skipped", force, tool, res)
			}
			if res.ConfigPath != "" {
				t.Errorf("force=%v Register(%s) exposed a write path %q — a sandboxed caller must get none", force, tool, res.ConfigPath)
			}
			if len(res.HooksAdded) != 0 {
				t.Errorf("force=%v Register(%s) added hooks %v", force, tool, res.HooksAdded)
			}
			if !strings.Contains(res.SkipReason, "2026-07-31") {
				t.Errorf("force=%v SkipReason should name the incident, got %q", force, res.SkipReason)
			}
			if res.SkipAdvice == "" {
				t.Errorf("force=%v Register(%s) skip carries no advice — the plugin wording would be printed instead", force, tool)
			}
		}
	}
}

// TestSandboxGate_ProductionPathUnchanged is the production-parity pin: with
// NO home override (what every real `observer init` / `observer start` does),
// crossmount auto-detection resolves exactly as it did before the gate — the
// owned Windows home is found and the virtual target surfaces. Injected seams
// only; no real /mnt/c, and nothing is registered (detection only).
func TestSandboxGate_ProductionPathUnchanged(t *testing.T) {
	winHome := mkWinConfigHome(t, ".claude")
	forceHookCrossmount(
		t,
		[]crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: winHome}},
		map[string]bool{winHome: true},
	)
	// HomeDir "" is the production shape: NewRegistry fills in the real
	// $HOME and homeOverride stays empty, so the gate is inert.
	r, err := NewRegistry(Options{BinaryPath: "/bin/observer"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(winHome, ".claude")
	if got := r.detectWindowsClaudeHome(); got != want {
		t.Errorf("production detectWindowsClaudeHome() = %q, want %q (auto-detect must be unchanged)", got, want)
	}
	if !containsString(r.Installed(), "claude-code-windows") {
		t.Errorf("production Installed() = %v, want claude-code-windows", r.Installed())
	}
}

// TestSandboxGate_ExplicitOverrideStillWires pins the escape hatch in its
// HONEST layout: a pinned home PLUS a Windows-side home NESTED UNDER it still
// resolves and still writes — the gate only removes the target it cannot prove
// is contained, never a genuinely sandboxed one.
func TestSandboxGate_ExplicitOverrideStillWires(t *testing.T) {
	realish := mkWinConfigHome(t, ".claude") // what auto-detect would have found
	forceHookCrossmount(
		t,
		[]crossmount.HomeRoot{{OS: crossmount.OSWindows, Path: realish}},
		map[string]bool{realish: true},
	)
	sandbox := t.TempDir()
	winHome := nestedWinHome(t, sandbox) // the caller's OWN win home, INSIDE the sandbox
	r, err := NewRegistry(Options{
		BinaryPath:        "/bin/observer",
		HomeDir:           sandbox,
		ChecksumsPath:     filepath.Join(sandbox, ".observer", "hook_checksums.json"),
		WindowsClaudeHome: winHome,
		WSLDistro:         "Ubuntu",
	})
	if err != nil {
		t.Fatal(err)
	}
	res := r.Register("claude-code-windows")
	if res.Error != nil || res.Skipped {
		t.Fatalf("contained override should still wire, got %+v", res)
	}
	want := filepath.Join(winHome, ".claude", "settings.json")
	if res.ConfigPath != want {
		t.Errorf("ConfigPath = %q, want %q", res.ConfigPath, want)
	}
	if !strings.HasPrefix(res.ConfigPath, sandbox+string(filepath.Separator)) {
		t.Fatalf("write path %q escaped the sandbox %q", res.ConfigPath, sandbox)
	}
	if strings.HasPrefix(res.ConfigPath, realish) {
		t.Fatalf("wrote into the auto-detected home %q instead of the override", realish)
	}
}

// TestSandboxGate_UncontainedOverrideSkips is the codex NO-SHIP scenario from
// the 2026-07-31 follow-up round: the first cut honoured ANY non-empty
// override, so a sandboxed caller pointing WindowsClaudeHome at an
// INDEPENDENT root (in the wild: /mnt/c/Users/<operator>) still wrote the real
// config — the escape hatch re-opened the hole. An override that does not
// resolve under the pinned HomeDir must now SKIP, with a reason naming the
// uncontained override, and touch nothing.
func TestSandboxGate_UncontainedOverrideSkips(t *testing.T) {
	forceHookCrossmount(t, nil, map[string]bool{}) // auto-detect finds nothing
	sandbox := t.TempDir()
	outside := t.TempDir() // a SECOND, independent root — the operator's home stand-in
	if err := os.MkdirAll(filepath.Join(outside, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		tool, option, subdir string
		apply                func(*Options)
	}{
		{"claude-code-windows", "WindowsClaudeHome", ".claude", func(o *Options) { o.WindowsClaudeHome = outside }},
		{"cursor-windows", "WindowsCursorHome", ".cursor", func(o *Options) { o.WindowsCursorHome = outside }},
		// A ".." escape must not smuggle a path out of the sandbox either.
		{"claude-code-windows", "WindowsClaudeHome", ".claude", func(o *Options) {
			o.WindowsClaudeHome = filepath.Join(sandbox, "..", filepath.Base(outside))
		}},
	}
	for _, c := range cases {
		opts := Options{
			BinaryPath:    "/bin/observer",
			HomeDir:       sandbox,
			ChecksumsPath: filepath.Join(sandbox, ".observer", "hook_checksums.json"),
			WSLDistro:     "Ubuntu",
			Force:         true, // --force must NOT lift containment either
		}
		c.apply(&opts)
		r, err := NewRegistry(opts)
		if err != nil {
			t.Fatal(err)
		}
		res := r.Register(c.tool)
		if res.Error != nil {
			t.Errorf("%s: uncontained override should skip, got error %v", c.tool, res.Error)
		}
		if !res.Skipped || res.ConfigPath != "" {
			t.Errorf("%s: want a clean skip with no path, got %+v", c.tool, res)
		}
		if !strings.Contains(res.SkipReason, c.option) || !strings.Contains(res.SkipReason, "OUTSIDE") {
			t.Errorf("%s: SkipReason should name the uncontained %s, got %q", c.tool, c.option, res.SkipReason)
		}
		if containsString(r.Installed(), c.tool) {
			t.Errorf("%s: must not surface in Installed() with an uncontained override: %v", c.tool, r.Installed())
		}
	}
	// Nothing was written into the outside root.
	if _, err := os.Stat(filepath.Join(outside, ".claude", "settings.json")); err == nil {
		t.Error("settings.json written into the uncontained override home")
	}
	if _, err := os.Stat(filepath.Join(outside, ".cursor", "hooks.json")); err == nil {
		t.Error("hooks.json written into the uncontained override home")
	}
}

// nestedWinHome creates a Windows-side USER home (the stand-in for
// /mnt/c/Users/<u>) NESTED UNDER the caller's sandbox home, and returns it.
//
// Nesting is not cosmetic: once a test pins Options.HomeDir, a Windows-home
// override is honoured ONLY if it resolves under that pinned home
// (crossmount.AutoDetectSuppressed → PathUnder). Two independent t.TempDir()
// roots are exactly the shape the first cut of the fix wrongly accepted — it
// let `HomeDir=/tmp/sandbox` + `WindowsClaudeHome=/mnt/c/Users/operator`
// through, which is the incident itself with extra steps.
func nestedWinHome(t *testing.T, sandboxHome string) string {
	t.Helper()
	dir := filepath.Join(sandboxHome, "mnt", "c", "Users", "tester")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
