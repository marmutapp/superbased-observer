package diag

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/toolresolve"
)

// swapLaunchResolve replaces the launchResolve seam for the duration of the
// test and restores it on cleanup.
func swapLaunchResolve(t *testing.T, fn func(integration.BinaryResolveSpec) toolresolve.Resolution) {
	t.Helper()
	orig := launchResolve
	launchResolve = fn
	t.Cleanup(func() { launchResolve = orig })
}

// swapAdapterDetected replaces the adapterDetected seam for the duration of
// the test and restores it on cleanup — keeps the checkAdapters bucket tests
// hermetic (independent of whatever adapter config dirs happen to exist on
// the host running the suite).
func swapAdapterDetected(t *testing.T, fn func(adapter.Adapter) bool) {
	t.Helper()
	orig := adapterDetected
	adapterDetected = fn
	t.Cleanup(func() { adapterDetected = orig })
}

// TestCheckAdapter_LaunchResolutionVerdicts pins the per-verdict doctor note
// CheckAdapter appends for a tool with a registered BinaryResolveSpec
// (Phase 4 — launch-resolution probe). "opencode" is used as the fixture tool
// because it carries a non-nil Binary row in the registry.
func TestCheckAdapter_LaunchResolutionVerdicts(t *testing.T) {
	tests := []struct {
		name       string
		res        toolresolve.Resolution
		wantStatus Status
		wantSubs   []string
	}{
		{
			name:       "ok",
			res:        toolresolve.Resolution{Verdict: toolresolve.VerdictOK, Bin: "/usr/local/bin/opencode"},
			wantStatus: StatusOK,
			wantSubs:   []string{"launcher binary: /usr/local/bin/opencode"},
		},
		{
			name: "ok_off_path",
			res: toolresolve.Resolution{
				Verdict: toolresolve.VerdictOKOffPath,
				Bin:     "/home/u/.opencode/bin/opencode",
				Notes:   []string{"opencode resolved from /home/u/.opencode/bin, which is not on PATH — add it to PATH to silence this note"},
			},
			wantStatus: StatusWarn,
			wantSubs: []string{
				"launcher binary found off PATH: /home/u/.opencode/bin/opencode",
				"which is not on PATH",
			},
		},
		{
			name: "ok_off_path no hygiene note",
			res: toolresolve.Resolution{
				Verdict: toolresolve.VerdictOKOffPath,
				Bin:     "/home/u/.opencode/bin/opencode",
			},
			wantStatus: StatusWarn,
			wantSubs:   []string{"launcher binary found off PATH: /home/u/.opencode/bin/opencode"},
		},
		{
			name: "shadowed",
			res: toolresolve.Resolution{
				Verdict:   toolresolve.VerdictShadowed,
				Bin:       "/usr/local/bin/opencode",
				Shadowing: []toolresolve.Candidate{{Path: "/mnt/c/Users/u/AppData/Roaming/npm/opencode"}},
			},
			wantStatus: StatusWarn,
			wantSubs: []string{
				"shadowed by a Windows interop shim",
				"/mnt/c/Users/u/AppData/Roaming/npm/opencode",
				"using the native binary at /usr/local/bin/opencode instead",
			},
		},
		{
			name: "foreign_only with install hint",
			res: toolresolve.Resolution{
				Verdict:    toolresolve.VerdictForeignOnly,
				Considered: []toolresolve.Candidate{{Path: "/mnt/c/Users/u/AppData/Roaming/npm/opencode.cmd", Foreign: true}},
				Installs:   []integration.InstallHint{{Display: "npm install -g opencode-ai@latest"}},
			},
			wantStatus: StatusWarn,
			wantSubs: []string{
				"installed on Windows, not in WSL",
				"/mnt/c/Users/u/AppData/Roaming/npm/opencode.cmd",
				"the daemon cannot launch it",
				"Install natively: npm install -g opencode-ai@latest",
				"cross-OS launch is a planned follow-up",
			},
		},
		{
			name: "foreign_only no grounded install",
			res: toolresolve.Resolution{
				Verdict:    toolresolve.VerdictForeignOnly,
				Considered: []toolresolve.Candidate{{Path: "/mnt/c/Users/u/AppData/Roaming/npm/opencode.cmd", Foreign: true}},
			},
			wantStatus: StatusWarn,
			wantSubs: []string{
				"installed on Windows, not in WSL",
				"no grounded install command — see the vendor's docs",
			},
		},
		{
			name:       "not_found with install hint",
			res:        toolresolve.Resolution{Verdict: toolresolve.VerdictNotFound, Installs: []integration.InstallHint{{Display: "npm install -g opencode-ai@latest"}}},
			wantStatus: StatusWarn,
			wantSubs:   []string{"launcher binary not found — install: npm install -g opencode-ai@latest"},
		},
		{
			name:       "not_found no grounded install",
			res:        toolresolve.Resolution{Verdict: toolresolve.VerdictNotFound},
			wantStatus: StatusWarn,
			wantSubs:   []string{"launcher binary not found — install: no grounded install command — see the vendor's docs"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			swapLaunchResolve(t, func(integration.BinaryResolveSpec) toolresolve.Resolution { return tc.res })

			c, ok := CheckAdapter("opencode", config.Config{})
			if !ok {
				t.Fatal("opencode should be a known adapter")
			}
			joined := strings.Join(c.Details, "\n")
			for _, sub := range tc.wantSubs {
				if !strings.Contains(joined, sub) {
					t.Errorf("Details missing %q\nDetails: %v", sub, c.Details)
				}
			}
			// Check.Status is a worst-of-all-notes rollup across every note
			// CheckAdapter emits (watch path, proxy reachability, launch
			// resolution, ...), so a Warn verdict here is asserted directly
			// (Warn can only make the rollup >= Warn); StatusOK is NOT
			// asserted for the "ok" verdict case because other notes (e.g.
			// proxy reachability) legitimately vary by host/environment.
			if tc.wantStatus == StatusWarn && c.Status != StatusWarn {
				t.Errorf("Status = %v, want StatusWarn (worst-status rollup)", c.Status)
			}
		})
	}
}

// TestLaunchResolutionNote pins launchResolutionNote directly (pure
// function, no adapter/config plumbing) — including the StatusOK case,
// which TestCheckAdapter_LaunchResolutionVerdicts intentionally does not
// assert on Check.Status (that's a worst-of-all-notes rollup that also
// depends on host-specific notes like proxy reachability).
func TestLaunchResolutionNote(t *testing.T) {
	status, msg := launchResolutionNote("opencode", toolresolve.Resolution{
		Verdict: toolresolve.VerdictOK,
		Bin:     "/usr/local/bin/opencode",
	})
	if status != StatusOK {
		t.Errorf("status = %v, want StatusOK", status)
	}
	if msg != "launcher binary: /usr/local/bin/opencode" {
		t.Errorf("msg = %q", msg)
	}
}

// TestCheckAdapter_NoBinarySpec pins the honest floor: an adapter with no
// registered BinaryResolveSpec (e.g. an IDE-extension adapter with no
// standalone launcher) gets no launch-resolution note at all, and the
// launchResolve seam is never even called for it. "cline" (the VS Code
// extension adapter, wraps its own IDE host — no standalone binary) is the
// fixture tool.
func TestCheckAdapter_NoBinarySpec(t *testing.T) {
	ic, ok := integration.For("cline")
	if !ok || ic.Binary != nil {
		t.Fatalf("fixture assumption broken: cline.Binary = %+v (want nil for this test)", ic.Binary)
	}

	swapLaunchResolve(t, func(integration.BinaryResolveSpec) toolresolve.Resolution {
		t.Fatal("launchResolve should not be called for an adapter with no Binary spec")
		return toolresolve.Resolution{}
	})

	c, ok := CheckAdapter("cline", config.Config{})
	if !ok {
		t.Fatal("cline should be a known adapter")
	}
	if joinedHas(c.Details, "launcher binary") {
		t.Errorf("cline should carry no launch-resolution note: %v", c.Details)
	}
}

// TestCheckAdapters_ForeignOnlyBucket pins the checkAdapters registry-driven
// summary's new foreign-only bucket (Phase 4): a tool that is not locally
// detected AND resolves foreign_only lands in "installed on Windows only",
// not "enabled, no data yet". adapterDetected is stubbed to always report
// "not detected" so the assertion is independent of whatever the host
// running the suite happens to have installed.
func TestCheckAdapters_ForeignOnlyBucket(t *testing.T) {
	swapAdapterDetected(t, func(adapter.Adapter) bool { return false })
	swapLaunchResolve(t, func(integration.BinaryResolveSpec) toolresolve.Resolution {
		return toolresolve.Resolution{Verdict: toolresolve.VerdictForeignOnly}
	})

	cfg := config.Config{}
	// opencode carries a Binary spec; cline does not (IDE-extension
	// adapter, no standalone launcher).
	cfg.Observer.Watch.EnabledAdapters = []string{"opencode", "cline"}

	check := checkAdapters(cfg)
	var idleLine, foreignLine string
	for _, d := range check.Details {
		switch {
		case strings.HasPrefix(d, "enabled, no data yet"):
			idleLine = d
		case strings.HasPrefix(d, "installed on Windows only"):
			foreignLine = d
		}
	}
	if foreignLine != "installed on Windows only (not launchable from WSL): opencode" {
		t.Errorf("foreign-only bucket = %q, want opencode listed", foreignLine)
	}
	if idleLine != "enabled, no data yet: cline" {
		t.Errorf("idle bucket = %q, want cline listed (no Binary spec — never resolved)", idleLine)
	}
}

// TestCheckAdapters_ForeignOnlyBucket_DetectedToolsUnaffected pins that a
// LOCALLY DETECTED tool never enters the foreign-only bucket even if its
// Binary happens to resolve foreign_only — detection is checked first, and
// launchResolve should not even run for it.
func TestCheckAdapters_ForeignOnlyBucket_DetectedToolsUnaffected(t *testing.T) {
	swapAdapterDetected(t, func(adapter.Adapter) bool { return true })
	swapLaunchResolve(t, func(integration.BinaryResolveSpec) toolresolve.Resolution {
		t.Fatal("launchResolve should not run for an already-detected adapter")
		return toolresolve.Resolution{}
	})

	cfg := config.Config{}
	cfg.Observer.Watch.EnabledAdapters = []string{"opencode"}

	check := checkAdapters(cfg)
	joined := strings.Join(check.Details, "\n")
	if !strings.Contains(joined, "detected (local data dir present): opencode") {
		t.Errorf("expected opencode detected: %v", check.Details)
	}
	if strings.Contains(joined, "installed on Windows only") {
		t.Errorf("a detected tool must never land in the foreign-only bucket: %v", check.Details)
	}
}

// TestCheckAdapters_ForeignOnlyBucket_NoBinaryToolsUnaffected pins that
// tools without a Binary spec never enter the foreign-only bucket, even
// though launchResolve is stubbed to always return foreign_only (it's never
// called for them — no Binary spec to resolve).
func TestCheckAdapters_ForeignOnlyBucket_NoBinaryToolsUnaffected(t *testing.T) {
	swapAdapterDetected(t, func(adapter.Adapter) bool { return false })
	swapLaunchResolve(t, func(integration.BinaryResolveSpec) toolresolve.Resolution {
		return toolresolve.Resolution{Verdict: toolresolve.VerdictForeignOnly}
	})

	cfg := config.Config{}
	cfg.Observer.Watch.EnabledAdapters = []string{"cline"}

	check := checkAdapters(cfg)
	joined := strings.Join(check.Details, "\n")
	if strings.Contains(joined, "installed on Windows only") {
		t.Errorf("cline has no Binary spec — should never land in the foreign-only bucket: %v", check.Details)
	}
}
