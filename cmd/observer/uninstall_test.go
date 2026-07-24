package main

import (
	"slices"
	"testing"
)

// TestSelectTools_UninstallKeepsBaseOnlyScope pins F3: the cross-OS
// "<base>-windows" union is init-ONLY. `observer uninstall --claude-code`
// resolves through the shared selectTools (base-only), so it must NOT widen to
// claude-code-windows — silently stripping the Windows-side profile on an
// uninstall would be a destructive scope change. The init path
// (selectToolsForInit) still unions the detected Windows target.
func TestSelectTools_UninstallKeepsBaseOnlyScope(t *testing.T) {
	// Both the base tool and its detected cross-OS virtual target are present.
	installed := []string{"claude-code", "claude-code-windows"}

	// Uninstall path: explicit --claude-code, base-only.
	got := selectTools(false, true, false, false, false, installed)
	if slices.Contains(got, "claude-code-windows") {
		t.Errorf("selectTools(--claude-code) = %v, must NOT include claude-code-windows (uninstall scope)", got)
	}
	if !slices.Contains(got, "claude-code") {
		t.Errorf("selectTools(--claude-code) = %v, want it to include claude-code", got)
	}

	// Init path: the same explicit selector DOES union the Windows target.
	gotInit := selectToolsForInit(false, true, false, false, false, installed)
	if !slices.Contains(gotInit, "claude-code-windows") {
		t.Errorf("selectToolsForInit(--claude-code) = %v, want it to include claude-code-windows", gotInit)
	}
	if !slices.Contains(gotInit, "claude-code") {
		t.Errorf("selectToolsForInit(--claude-code) = %v, want it to include claude-code", gotInit)
	}
}
