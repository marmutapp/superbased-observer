package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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
