package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWireAIClients_BatchClaudeCode pins the batch-init seam newInitCmd
// delegates to: a claude-code-only dry-run wire reports all three
// registration sides (hook / mcp / route) without writing anything,
// and no proxy hint fires because the route write wasn't skipped.
// OnlyClaudeCode bypasses tool detection entirely, so the test is
// deterministic regardless of crossmount *-windows detection.
func TestWireAIClients_BatchClaudeCode(t *testing.T) {
	home := interactiveHome(t)
	lines, claudeHint, codexHint, codexHooksHint, err := wireAIClients(WireAIClientsOptions{
		ProxyPort:      18820,
		DryRun:         true,
		HomeDir:        home,
		OnlyClaudeCode: true,
	})
	if err != nil {
		t.Fatalf("wireAIClients: %v", err)
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"hook", "mcp", "route", "would register"} {
		if !strings.Contains(text, want) {
			t.Errorf("lines missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "18820") {
		t.Errorf("route line should carry the configured proxy port:\n%s", text)
	}
	if claudeHint != "" {
		t.Errorf("route not skipped — claude hint should be empty, got:\n%s", claudeHint)
	}
	if codexHint != "" || codexHooksHint {
		t.Errorf("no codex in the wire — hints should be silent (codexHint=%q codexHooksHint=%v)", codexHint, codexHooksHint)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(statErr) {
		raw, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		t.Errorf("dry run wrote settings.json:\n%s", raw)
	}
}

// TestWireAIClients_SkipProxyEmitsCodexHint pins the hint contract the
// batch path prints: with the route write skipped, the codex hint comes
// back non-empty (it is deliberately NOT dry-run-gated — matching the
// pre-dedup inline loop), no route line appears, and the dry-run-gated
// codex trust hint stays false.
func TestWireAIClients_SkipProxyEmitsCodexHint(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	lines, claudeHint, codexHint, codexHooksHint, err := wireAIClients(WireAIClientsOptions{
		DryRun:    true,
		SkipProxy: true,
		HomeDir:   home,
		OnlyCodex: true,
	})
	if err != nil {
		t.Fatalf("wireAIClients: %v", err)
	}
	text := strings.Join(lines, "\n")
	if strings.Contains(text, "route") {
		t.Errorf("route skipped but a route line appeared:\n%s", text)
	}
	if codexHint == "" {
		t.Error("codex hint should fire when the route write is skipped")
	}
	if claudeHint != "" {
		t.Errorf("claude hint is dry-run-gated and claude-code wasn't wired, got:\n%s", claudeHint)
	}
	if codexHooksHint {
		t.Error("codex trust hint is dry-run-gated — must be false under DryRun")
	}
}

// TestMCPSupportedIsRegistryDriven pins that the MCP write-eligibility
// predicate dispatches on the integration registry's MCP capability shape,
// not a hardcoded tool switch. The mcp.Registrar writes the JSON + Codex
// TOML + OpenCode JSON formats; Hermes' YAML is Implemented but written by
// runHermesInit, so it (and every watcher-only adapter) must be excluded.
// cline carries a native JSON target AND a cross-OS "cline-windows" bridge
// (MCP.CrossOSBridge) — both supported; "cline-cli" stays out (no MCP file).
func TestMCPSupportedIsRegistryDriven(t *testing.T) {
	in := []string{"claude-code", "cursor", "codex", "opencode", "cline", "cline-windows"}
	out := []string{"hermes", "cline-cli", "copilot", "antigravity", "pi", "kilo-code", "definitely-not-a-tool", ""}
	for _, tool := range in {
		if !mcpSupported(tool) {
			t.Errorf("mcpSupported(%q) = false, want true (registrar-handled MCP format)", tool)
		}
	}
	for _, tool := range out {
		if mcpSupported(tool) {
			t.Errorf("mcpSupported(%q) = true, want false (no registrar-handled MCP writer)", tool)
		}
	}
}

// TestHookSupportedIsRegistryDriven pins that hook write-eligibility
// dispatches on the integration registry's HookMechanism + CrossOSBridge,
// not a hardcoded tool switch. The hook.Registry handles claude-code /
// cursor / codex (and the -windows bridge for claude-code/cursor); Hermes'
// embedded plugin (runHermesInit) and cline-cli's manual hooks.jsonl tailer
// are excluded. Behaviour-identical to the pre-Phase-2 switch.
func TestHookSupportedIsRegistryDriven(t *testing.T) {
	in := []string{"claude-code", "claude-code-windows", "cursor", "cursor-windows", "codex"}
	out := []string{"codex-windows", "hermes", "cline-cli", "opencode", "cline", "copilot", "antigravity", "pi", "definitely-not-a-tool", ""}
	for _, tool := range in {
		if !hookSupported(tool) {
			t.Errorf("hookSupported(%q) = false, want true", tool)
		}
	}
	for _, tool := range out {
		if hookSupported(tool) {
			t.Errorf("hookSupported(%q) = true, want false", tool)
		}
	}
}

// TestRouteSupportedIsRegistryDriven pins that proxy-route write-eligibility
// dispatches on the integration registry's RouteKind, not a tool switch.
// Only the persisted kinds (RouteEnvSettings claude-code, RouteConfigFile
// codex) are writable by init; opencode's RouteLauncher kind is applied by
// the `observer opencode` launcher (false here), and proxy-exempt tools
// (Proxy==nil) are false. Behaviour-identical to the pre-Phase-3 predicate.
func TestRouteSupportedIsRegistryDriven(t *testing.T) {
	in := []string{"claude-code", "codex"}
	out := []string{"opencode", "cursor", "cline", "copilot", "hermes", "antigravity", "pi", "kilo-code-cli", "definitely-not-a-tool", ""}
	for _, tool := range in {
		if !routeSupported(tool) {
			t.Errorf("routeSupported(%q) = false, want true (persisted route kind)", tool)
		}
	}
	for _, tool := range out {
		if routeSupported(tool) {
			t.Errorf("routeSupported(%q) = true, want false (launcher-only or proxy-exempt)", tool)
		}
	}
}
