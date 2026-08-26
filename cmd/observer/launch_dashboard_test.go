package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
	"github.com/marmutapp/superbased-observer/internal/toolresolve"
)

// recordingLauncher captures the fully server-derived LaunchRequest termsvc
// hands the Launcher, so a test can assert what project root actually reached
// the spawn boundary.
type recordingLauncher struct{ last termsvc.LaunchRequest }

func (r *recordingLauncher) Spawn(req termsvc.LaunchRequest) (string, error) {
	r.last = req
	return "H-1", nil
}

// TestCreateFreshTranslatesForeignProjectRoot pins the fresh-cwd fix
// (tool-binary-resolution arc §5): launchManagerAdapter.CreateFresh runs the
// client-influenced project root through crossmount.TranslateForeignPath BEFORE
// termsvc validates + spawns, so a Windows-drive-shaped root typed into the New
// Terminal dialog (`C:\Users\u\proj`) becomes its WSL-reachable `/mnt/c/...`
// form. A native absolute path is an identity no-op and must still reach the
// spawner unchanged.
func TestCreateFreshTranslatesForeignProjectRoot(t *testing.T) {
	dir := t.TempDir()
	lau := &recordingLauncher{}
	svc := termsvc.New(termsvc.Options{
		Recorder: assembledRecorder{},
		Launcher: lau,
		Policy: termsvc.Policy{
			AllowFresh:          true,
			AllowedTools:        []string{"claude-code"},
			AllowedProjectRoots: []string{dir},
		},
	})
	a := &launchManagerAdapter{svc: svc}

	// Native absolute path: translation is a no-op, so the canonical dir reaches
	// termsvc.Spawn unchanged (the regression guard that the translate call did
	// not break native launches).
	if _, err := a.CreateFresh(dashboard.FreshLaunchSpec{
		Tool: "claude-code", Subcommand: "claude", ProjectRoot: dir,
	}); err != nil {
		t.Fatalf("CreateFresh native: %v", err)
	}
	wantCanon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if lau.last.Dir != wantCanon {
		t.Fatalf("native root not passed through: got %q want %q", lau.last.Dir, wantCanon)
	}

	// Pin the exact transform CreateFresh applies to a Windows drive root.
	const winRoot = `C:\Users\u\proj`
	if got := crossmount.TranslateForeignPath(winRoot); got != "/mnt/c/Users/u/proj" {
		t.Fatalf("transform pin: TranslateForeignPath(%q) = %q, want /mnt/c/Users/u/proj", winRoot, got)
	}

	// CreateFresh feeds the TRANSLATED path to termsvc; here it is neither
	// allow-listed nor existent, so validation denies it — proving the raw
	// backslash path was transformed + validated, never spawned verbatim.
	lau.last = termsvc.LaunchRequest{}
	if _, err := a.CreateFresh(dashboard.FreshLaunchSpec{
		Tool: "claude-code", Subcommand: "claude", ProjectRoot: winRoot,
	}); !errors.Is(err, dashboard.ErrLaunchProjectRootDenied) {
		t.Fatalf("CreateFresh windows root err = %v, want ErrLaunchProjectRootDenied", err)
	}
	if lau.last.Dir != "" {
		t.Fatalf("denied launch must not reach the spawner: got dir %q", lau.last.Dir)
	}
}

// TestToolPreflightSeam_ConfigOverrideParity pins F1: the dashboard preflight
// seam applies the SAME [launch.tools.<tool>].path override the actual launcher
// (resolveToolBin step 2) honors, so a preflight never diverges from the launch
// it predicts — in BOTH directions.
func TestToolPreflightSeam_ConfigOverrideParity(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCfg := func(t *testing.T, path string) string {
		t.Helper()
		cfgPath := filepath.Join(t.TempDir(), "config.toml")
		body := "[launch.tools.claude-code]\npath = " + strconv.Quote(path) + "\n"
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return cfgPath
	}

	// Direction 1: a configured, EXISTING path → verdict ok, Bin = that path,
	// note names the config key (parity with the launcher's configured-hit path).
	t.Run("configured_present_ok", func(t *testing.T) {
		seam := toolPreflightSeam(writeCfg(t, binPath), func() bool { return true })
		pf, ok := seam("claude-code")
		if !ok {
			t.Fatal("expected ok=true for a launchable tool")
		}
		if pf.Verdict != string(toolresolve.VerdictOK) || pf.Bin != binPath {
			t.Errorf("configured-present: got verdict=%q bin=%q, want ok / %q", pf.Verdict, pf.Bin, binPath)
		}
		if len(pf.Notes) == 0 || !strings.Contains(pf.Notes[0], "[launch.tools.claude-code].path") {
			t.Errorf("expected a note naming the config key, got %v", pf.Notes)
		}
	})

	// Direction 2: a configured, MISSING path → verdict not_found naming the
	// stale key, with NO silent fall-through to the ladder (which on this host
	// could resolve a real claude-code and dishonestly report ok).
	t.Run("configured_missing_not_found", func(t *testing.T) {
		missing := filepath.Join(binDir, "does-not-exist")
		seam := toolPreflightSeam(writeCfg(t, missing), func() bool { return true })
		pf, ok := seam("claude-code")
		if !ok {
			t.Fatal("expected ok=true (tool is launchable; the verdict carries the failure)")
		}
		if pf.Verdict != string(toolresolve.VerdictNotFound) {
			t.Errorf("configured-missing: verdict = %q, want not_found", pf.Verdict)
		}
		if pf.Bin != "" {
			t.Errorf("configured-missing: Bin should be empty, got %q", pf.Bin)
		}
		if len(pf.Notes) == 0 || !strings.Contains(pf.Notes[0], "[launch.tools.claude-code].path") {
			t.Errorf("expected a note naming the stale config key, got %v", pf.Notes)
		}
	})
}

func TestDashboardInstallPlanPrefersExactOS(t *testing.T) {
	hints := []integration.InstallHint{
		{Channel: "npm", Argv: []string{"npm", "install", "-g", "pkg"}, Display: "npm install -g pkg"},
		{OS: "linux", Channel: "brew", Argv: []string{"brew", "install", "pkg"}, Display: "brew install pkg"},
		{OS: "linux", Channel: "script", Argv: []string{"bash", "-lc", "vendor installer"}, Display: "vendor installer"},
	}
	got, ok := dashboardInstallPlanFor(hints, "linux", "/home/u")
	if !ok {
		t.Fatal("expected an install plan")
	}
	if got.Display != "vendor installer" || strings.Join(got.Argv, "|") != "bash|-lc|vendor installer" {
		t.Fatalf("exact-OS plan did not win: %+v", got)
	}
}

func TestDashboardInstallPlanUsesUserLocalNPMPrefix(t *testing.T) {
	hints := []integration.InstallHint{{
		Channel: "npm",
		Argv:    []string{"npm", "install", "-g", "--ignore-scripts", "pkg"},
		Display: "npm install -g --ignore-scripts pkg",
	}}
	got, ok := dashboardInstallPlanFor(hints, "linux", "/home/azureuser")
	if !ok {
		t.Fatal("expected an install plan")
	}
	want := "npm|install|--global|--prefix|/home/azureuser/.local|--ignore-scripts|pkg"
	if strings.Join(got.Argv, "|") != want {
		t.Fatalf("argv = %v, want %s", got.Argv, want)
	}
	if strings.Contains(got.Display, " -g ") || !strings.Contains(got.Display, "--prefix /home/azureuser/.local") {
		t.Fatalf("display does not match permission-safe argv: %q", got.Display)
	}

	windows, ok := dashboardInstallPlanFor(hints, "windows", `C:\Users\u`)
	if !ok || strings.Join(windows.Argv, "|") != "npm|install|-g|--ignore-scripts|pkg" {
		t.Fatalf("Windows npm plan must remain native: %+v ok=%v", windows, ok)
	}
}

func TestDashboardOpenCodeInstallUsesOfficialLinuxInstaller(t *testing.T) {
	row, ok := integration.For("opencode")
	if !ok || row.Binary == nil {
		t.Fatal("opencode binary registry row missing")
	}
	got, ok := dashboardInstallPlanFor(row.Binary.Installs, "linux", "/home/u")
	if !ok {
		t.Fatal("opencode Linux install plan missing")
	}
	if got.Display != "curl -fsSL https://opencode.ai/install | bash" {
		t.Fatalf("opencode plan = %q, want official user installer", got.Display)
	}
}

// TestDashResolveEnvTTL pins F7: the dashboard's toolresolve.Env is a TTL cache,
// not a forever-memo — it rebuilds via host env capture once the cached value
// ages past the TTL, so a fresh install becomes visible without a restart.
func TestDashResolveEnvTTL(t *testing.T) {
	prevNow, prevBuild, prevTTL := dashResolveEnvNow, dashResolveEnvBuild, dashResolveEnvTTL
	prevHave, prevAt, prevVal := dashResolveEnvHave, dashResolveEnvAt, dashResolveEnvVal
	t.Cleanup(func() {
		dashResolveEnvNow, dashResolveEnvBuild, dashResolveEnvTTL = prevNow, prevBuild, prevTTL
		dashResolveEnvHave, dashResolveEnvAt, dashResolveEnvVal = prevHave, prevAt, prevVal
	})

	var builds int
	dashResolveEnvBuild = func() toolresolve.Env { builds++; return toolresolve.Env{GOOS: "linux"} }
	now := time.Unix(1000, 0)
	dashResolveEnvNow = func() time.Time { return now }
	dashResolveEnvTTL = 30 * time.Second
	dashResolveEnvHave = false // cold cache

	dashResolveEnv()
	dashResolveEnv()
	if builds != 1 {
		t.Fatalf("expected exactly 1 build within TTL, got %d", builds)
	}
	now = now.Add(29 * time.Second) // still inside the TTL window
	dashResolveEnv()
	if builds != 1 {
		t.Fatalf("expected no rebuild before TTL expiry, got %d builds", builds)
	}
	now = now.Add(2 * time.Second) // 31s since the build → past TTL
	dashResolveEnv()
	if builds != 2 {
		t.Fatalf("expected a rebuild past TTL, got %d builds", builds)
	}
}

// TestTerminalLaunchPolicyAllowShell pins terminalLaunchPolicy's AllowShell
// wiring: it requires BOTH [terminal].enabled and [terminal.launch].allow_shell,
// and is independent of allow_fresh_agent — mirroring AllowFresh's own
// enabled-AND-opt-in gate, but as a SEPARATE opt-in (CLAUDE.md's "additive, not
// invasive" — a bare shell must not ride in on the AI-tool fresh-launch flag).
func TestTerminalLaunchPolicyAllowShell(t *testing.T) {
	tests := []struct {
		name            string
		enabled         bool
		allowFreshAgent bool
		allowShell      bool
		wantAllowFresh  bool
		wantAllowShell  bool
	}{
		{"terminal disabled denies both regardless of opt-ins", false, true, true, false, false},
		{"enabled, neither opt-in set", true, false, false, false, false},
		{"enabled, fresh-agent only", true, true, false, true, false},
		{"enabled, shell only", true, false, true, false, true},
		{"enabled, both opt-ins", true, true, true, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc2 := config.TerminalConfig{Enabled: tc.enabled}
			tc2.Launch.AllowFreshAgent = tc.allowFreshAgent
			tc2.Launch.AllowShell = tc.allowShell
			got := terminalLaunchPolicy(tc2)
			if got.AllowFresh != tc.wantAllowFresh {
				t.Errorf("AllowFresh = %v, want %v", got.AllowFresh, tc.wantAllowFresh)
			}
			if got.AllowShell != tc.wantAllowShell {
				t.Errorf("AllowShell = %v, want %v", got.AllowShell, tc.wantAllowShell)
			}
		})
	}
}

// TestMapFreshErrShellDisabled pins the error-translation seam: termsvc's
// ErrShellLaunchDisabled maps onto the dashboard's own sentinel, the same way
// every other termsvc fresh-launch authorization error does, so
// handleTerminalLaunch's switch (which only knows the dashboard sentinels)
// renders it as an honest 403 naming the missing config knob.
func TestMapFreshErrShellDisabled(t *testing.T) {
	got := mapFreshErr(termsvc.ErrShellLaunchDisabled)
	if !errors.Is(got, dashboard.ErrLaunchShellDisabled) {
		t.Fatalf("mapFreshErr(ErrShellLaunchDisabled) = %v, want ErrLaunchShellDisabled", got)
	}
}

// TestCreateFreshShellRequest pins launchManagerAdapter.CreateFresh's Shell
// plumbing end to end: a shell request is authorized by AllowShell alone (no
// AllowedTools entry needed — termsvc.ShellTool is deliberately never a member
// of that list), and the resulting LaunchRequest reaching the Launcher carries
// IsShell=true with the reserved pseudo-tool as its Tool label.
func TestCreateFreshShellRequest(t *testing.T) {
	lau := &recordingLauncher{}
	svc := termsvc.New(termsvc.Options{
		Recorder: assembledRecorder{},
		Launcher: lau,
		Policy: termsvc.Policy{
			AllowShell: true,
			// Deliberately no AllowFresh / AllowedTools — proves shell
			// authorization does not need the AI-tool fresh-launch gate.
		},
	})
	a := &launchManagerAdapter{svc: svc}

	if _, err := a.CreateFresh(dashboard.FreshLaunchSpec{
		Tool:  termsvc.ShellTool,
		Shell: true,
	}); err != nil {
		t.Fatalf("CreateFresh shell: %v", err)
	}
	if !lau.last.IsShell {
		t.Errorf("LaunchRequest.IsShell = false, want true: %+v", lau.last)
	}
	if lau.last.Tool != termsvc.ShellTool {
		t.Errorf("LaunchRequest.Tool = %q, want %q", lau.last.Tool, termsvc.ShellTool)
	}

	// Without AllowShell, the same request is denied even though it needs no
	// tool allow-list entry.
	svc2 := termsvc.New(termsvc.Options{Recorder: assembledRecorder{}, Launcher: lau})
	a2 := &launchManagerAdapter{svc: svc2}
	if _, err := a2.CreateFresh(dashboard.FreshLaunchSpec{Tool: termsvc.ShellTool, Shell: true}); !errors.Is(err, dashboard.ErrLaunchShellDisabled) {
		t.Fatalf("CreateFresh shell (disabled) err = %v, want ErrLaunchShellDisabled", err)
	}
}
