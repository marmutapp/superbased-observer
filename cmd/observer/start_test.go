package main

import (
	"strings"
	"testing"
)

// TestAutoRegisterRemediation pins the per-tool remediation hint
// autoRegisterHooks attaches to a failure WARN (F3,
// docs/audits/cursor-windows-capture-diagnosis-2026-08-07.md §4).
// Table-driven over hook.Registry's Installed()/Register()
// vocabulary; an unrecognised tool still gets a usable generic hint
// rather than a malformed command string.
func TestAutoRegisterRemediation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tool     string
		wantFlag string // "" means no --tool flag expected in the hint
	}{
		{"claude-code", "--claude-code"},
		{"claude-code-windows", "--claude-code"},
		{"cursor", "--cursor"},
		{"cursor-windows", "--cursor"},
		{"codex", "--codex"},
		{"some-future-tool", ""},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			t.Parallel()
			got := autoRegisterRemediation(c.tool)
			if !strings.Contains(got, "observer init") {
				t.Errorf("autoRegisterRemediation(%q) = %q, want it to mention `observer init`", c.tool, got)
			}
			if !strings.Contains(got, "--force") {
				t.Errorf("autoRegisterRemediation(%q) = %q, want it to mention --force", c.tool, got)
			}
			if c.wantFlag != "" && !strings.Contains(got, c.wantFlag) {
				t.Errorf("autoRegisterRemediation(%q) = %q, want it to mention %q", c.tool, got, c.wantFlag)
			}
			if strings.Contains(got, "  ") {
				t.Errorf("autoRegisterRemediation(%q) = %q, want no double-space artifact from an empty flag", c.tool, got)
			}
		})
	}
}
