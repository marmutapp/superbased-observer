package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/proxyroute"
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

// TestExtensionSupportedIsRegistryDriven pins the 4th consent step's
// predicate: extensionSupported is true exactly for the browser-chatbot
// rails whose Hook.Mechanism is HookBrowserExtension (registry-driven, not a
// tool switch), and false for every AI-CLI tool + unknown names. It also
// pins that hookSupported stays FALSE for the *-web rails (the browser step
// is a distinct predicate — the extension attaches via native-messaging, not
// the AI-tool hook registrar).
func TestExtensionSupportedIsRegistryDriven(t *testing.T) {
	in := []string{"chatgpt-web", "claude-web", "perplexity-web", "gemini-web", "copilot-web"}
	out := []string{"claude-code", "codex", "cursor", "hermes", "copilot", "copilot-cli", "definitely-not-a-tool", ""}
	for _, tool := range in {
		if !extensionSupported(tool) {
			t.Errorf("extensionSupported(%q) = false, want true", tool)
		}
		if hookSupported(tool) {
			t.Errorf("hookSupported(%q) = true, want false (browser rail is not an AI-tool hook)", tool)
		}
	}
	for _, tool := range out {
		if extensionSupported(tool) {
			t.Errorf("extensionSupported(%q) = true, want false", tool)
		}
	}
	if !anyExtensionSupported() {
		t.Errorf("anyExtensionSupported() = false, want true (5 *-web rails registered)")
	}
}

// TestRunBrowserExtensionStep pins the 4th-step writer: with a Chromium
// profile dir present it writes the native-messaging manifest (idempotent on
// re-run), and with none it writes nothing.
func TestRunBrowserExtensionStep(t *testing.T) {
	home := t.TempDir()
	// No browser → no output.
	var empty strings.Builder
	runBrowserExtensionStep(&empty, home, false)
	if empty.Len() != 0 {
		t.Errorf("no-browser run wrote %q, want nothing", empty.String())
	}
	// Create a Chrome profile dir → the step writes the manifest.
	if err := os.MkdirAll(filepath.Join(home, ".config", "google-chrome"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var first strings.Builder
	runBrowserExtensionStep(&first, home, false)
	if !strings.Contains(first.String(), "wrote native-messaging host") {
		t.Errorf("first run = %q, want a 'wrote' line", first.String())
	}
	var second strings.Builder
	runBrowserExtensionStep(&second, home, false)
	if !strings.Contains(second.String(), "already set") {
		t.Errorf("second run = %q, want 'already set' (idempotent)", second.String())
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

// TestRouteProbeSupportedIsRegistryDriven pins the SEPARATE probe-route
// gate: it is true exactly for the tools carrying a registry ProxyProbe
// (guarded config-lane writer exists, route not yet live-verified) and
// false for the verified routes (which routeSupported handles) and every
// proxy-exempt tool. The set derives from the registry, not a hand-list,
// and probeRouteWriter must resolve a writer for each probe tool.
func TestRouteProbeSupportedIsRegistryDriven(t *testing.T) {
	reg, err := proxyroute.NewRegistrar(proxyroute.RegisterOptions{ProxyPort: 18820, HomeDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRegistrar: %v", err)
	}
	in := []string{"kimi-code", "crush", "qwen-code"}
	out := []string{"claude-code", "codex", "opencode", "cursor", "hermes", "pi", "definitely-not-a-tool", ""}
	for _, tool := range in {
		if !routeProbeSupported(tool) {
			t.Errorf("routeProbeSupported(%q) = false, want true (registry ProxyProbe)", tool)
		}
		if _, ok := probeRouteWriter(reg, tool); !ok {
			t.Errorf("probeRouteWriter(%q) has no writer — every probe tool needs one", tool)
		}
	}
	for _, tool := range out {
		if routeProbeSupported(tool) {
			t.Errorf("routeProbeSupported(%q) = true, want false", tool)
		}
	}
	// probeRouteTools derives from the registry and includes exactly the probe set.
	got := map[string]bool{}
	for _, tool := range probeRouteTools() {
		got[tool] = true
	}
	for _, tool := range in {
		if !got[tool] {
			t.Errorf("probeRouteTools() missing %q", tool)
		}
	}
}
