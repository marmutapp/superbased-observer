package integration

import "testing"

func TestForProxyRoutableTools(t *testing.T) {
	tests := []struct {
		tool       string
		wantOK     bool
		wantProxy  bool
		wantEnvVar string
		wantSuffix string
		wantConfig bool // routes via config file (EnvVar == "", Note set)
	}{
		{"claude-code", true, true, "ANTHROPIC_BASE_URL", "", false},
		{"opencode", true, true, "OPENAI_BASE_URL", "/v1", false},
		{"codex", true, true, "", "", true},
		// Registered now (discovery spike), but NOT proxy-routable: own
		// backend, no base-URL knob → Proxy == nil. ok=true, wantProxy=false.
		{"cursor", true, false, "", "", false},
		{"antigravity", true, false, "", "", false},
		// Never registered → zero-value Capability that still echoes Tool.
		{"definitely-not-a-tool", false, false, "", "", false},
		{"", false, false, "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			c, ok := For(tc.tool)
			if ok != tc.wantOK {
				t.Fatalf("For(%q) ok = %v, want %v", tc.tool, ok, tc.wantOK)
			}
			if c.Tool != tc.tool {
				t.Errorf("For(%q).Tool = %q, want %q (must echo the tool name)", tc.tool, c.Tool, tc.tool)
			}
			hasProxy := c.Proxy != nil
			if hasProxy != tc.wantProxy {
				t.Fatalf("For(%q) hasProxy = %v, want %v", tc.tool, hasProxy, tc.wantProxy)
			}
			if !hasProxy {
				return
			}
			if c.Proxy.EnvVar != tc.wantEnvVar {
				t.Errorf("For(%q).Proxy.EnvVar = %q, want %q", tc.tool, c.Proxy.EnvVar, tc.wantEnvVar)
			}
			if c.Proxy.Suffix != tc.wantSuffix {
				t.Errorf("For(%q).Proxy.Suffix = %q, want %q", tc.tool, c.Proxy.Suffix, tc.wantSuffix)
			}
			if tc.wantConfig && (c.Proxy.EnvVar != "" || c.Proxy.Note == "") {
				t.Errorf("For(%q): expected config-file route (EnvVar empty + Note set), got EnvVar=%q Note=%q", tc.tool, c.Proxy.EnvVar, c.Proxy.Note)
			}
			if c.Proxy.Launcher == "" {
				t.Errorf("For(%q).Proxy.Launcher must be set", tc.tool)
			}
		})
	}
}

// TestProxyRouteKinds pins the route-application kind for each routable
// adapter — the capability the init proxy-route step dispatches on. Only
// the persisted kinds are written by init; opencode is launcher-applied.
func TestProxyRouteKinds(t *testing.T) {
	want := map[string]RouteKind{
		"claude-code": RouteEnvSettings,
		"codex":       RouteConfigFile,
		"opencode":    RouteLauncher,
		"gemini-cli":  RouteLauncher,     // Phase E bridge, live-verified 2026-06-27
		"copilot-cli": RouteLauncher,     // BYOK COPILOT_PROVIDER_BASE_URL, live-verified 2026-06-27
		"pi":          RouteProviderJSON, // models.json custom provider, live-verified 2026-06-27
		"cline-cli":   RouteProviderJSON, // openai-compatible settings.baseUrl, live-verified 2026-06-27
		"hermes":      RouteProviderJSON, // user-config provider + key_env (Approach B), live-verified 2026-06-27
	}
	for tool, kind := range want {
		c, ok := For(tool)
		if !ok || c.Proxy == nil {
			t.Fatalf("For(%q): expected a routable capability", tool)
		}
		if c.Proxy.Kind != kind {
			t.Errorf("For(%q).Proxy.Kind = %q, want %q", tool, c.Proxy.Kind, kind)
		}
	}
	// Every other registered adapter must be proxy-exempt (Proxy == nil).
	for _, c := range Capabilities() {
		if _, routable := want[c.Tool]; routable {
			continue
		}
		if c.Proxy != nil {
			t.Errorf("%s: Proxy = %+v, want nil (proxy-exempt)", c.Tool, c.Proxy)
		}
	}
}

// TestRoutabilityClassifiedForEveryAdapter pins the honesty rule for the
// surface-specific reclassification (Phase 0): every adapter declares a
// routability bucket — no row may be left RouteStatusUnknown — and the
// bucket is consistent with the Proxy field (a route observer drives today
// implies the surface is routable). This is the guardrail that forces a new
// adapter to be classified, not silently defaulted to "impossible".
func TestRoutabilityClassifiedForEveryAdapter(t *testing.T) {
	for _, c := range Capabilities() {
		if c.Routability == RouteStatusUnknown {
			t.Errorf("%s: Routability is unclassified (RouteStatusUnknown) — assign a surface bucket", c.Tool)
		}
		if c.Proxy != nil && c.Routability != RouteStatusRoutableNow {
			t.Errorf("%s: Proxy is non-nil (route applied today) but Routability = %q, want RouteStatusRoutableNow",
				c.Tool, c.Routability)
		}
	}
}

func TestCapabilitiesCoversRegistry(t *testing.T) {
	caps := Capabilities()
	if len(caps) == 0 {
		t.Fatal("Capabilities() returned empty")
	}
	for _, c := range caps {
		if c.Tool == "" {
			t.Errorf("Capabilities() entry with empty Tool: %+v", c)
		}
		if _, ok := For(c.Tool); !ok {
			t.Errorf("Capabilities() lists %q but For(%q) is not ok", c.Tool, c.Tool)
		}
	}
}

// TestHookCapabilities pins the grounded hook mechanisms from the discovery
// spike. Phase 2 will replace internal/hook/register.go's switch with a
// walk over these; the AutoWired flag keeps cline-cli honest (receiver
// exists, init does not yet register it).
func TestHookCapabilities(t *testing.T) {
	tests := []struct {
		tool          string
		wantMechanism HookMechanism
		wantBridge    bool
		wantAutoWired bool
	}{
		{"claude-code", HookClaudeSettings, true, true},
		{"cursor", HookCursor, true, true},
		{"codex", HookCodexConfig, false, true},
		{"hermes", HookHermesPlugin, false, true},
		{"cline-cli", HookClineCLIJSONL, false, false}, // receiver exists, not auto-wired
		{"opencode", HookNone, false, false},
		{"cline", HookNone, false, false},
		{"pi", HookNone, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.tool, func(t *testing.T) {
			c, ok := For(tc.tool)
			if !ok {
				t.Fatalf("For(%q) not registered", tc.tool)
			}
			if c.Hook.Mechanism != tc.wantMechanism {
				t.Errorf("Hook.Mechanism = %q, want %q", c.Hook.Mechanism, tc.wantMechanism)
			}
			if c.Hook.CrossOSBridge != tc.wantBridge {
				t.Errorf("Hook.CrossOSBridge = %v, want %v", c.Hook.CrossOSBridge, tc.wantBridge)
			}
			if c.Hook.AutoWired != tc.wantAutoWired {
				t.Errorf("Hook.AutoWired = %v, want %v", c.Hook.AutoWired, tc.wantAutoWired)
			}
		})
	}
}

// TestMCPTargets pins the five clients with a grounded, implemented MCP
// writer today (claude-code/cursor/codex/hermes/opencode). Every other
// adapter must carry MCP == nil so init only writes config where a writer
// exists.
func TestMCPTargets(t *testing.T) {
	wantImplemented := map[string]struct {
		format   MCPFormat
		pathHint string
	}{
		"claude-code": {MCPServersJSON, ".claude.json"},
		"cursor":      {MCPServersJSON, ".cursor/mcp.json"},
		"codex":       {MCPCodexTOML, ".codex/config.toml"},
		"hermes":      {MCPHermesYAML, ".hermes/config.yaml"},
		"opencode":    {MCPOpenCodeJSON, ".config/opencode/opencode.json"},
		"cline":       {MCPServersJSON, "<vscode>/User/globalStorage/saoudrizwan.claude-dev/settings/cline_mcp_settings.json"},
	}
	for _, c := range Capabilities() {
		want, expect := wantImplemented[c.Tool]
		switch {
		case expect:
			if c.MCP == nil {
				t.Errorf("%s: MCP target is nil, want implemented %s", c.Tool, want.format)
				continue
			}
			if !c.MCP.Implemented {
				t.Errorf("%s: MCP.Implemented = false, want true", c.Tool)
			}
			if c.MCP.Format != want.format {
				t.Errorf("%s: MCP.Format = %q, want %q", c.Tool, c.MCP.Format, want.format)
			}
			if c.MCP.PathHint != want.pathHint {
				t.Errorf("%s: MCP.PathHint = %q, want %q", c.Tool, c.MCP.PathHint, want.pathHint)
			}
		default:
			if c.MCP != nil {
				t.Errorf("%s: MCP target = %+v, want nil (no grounded writer)", c.Tool, c.MCP)
			}
		}
	}
}

// TestNativeRailsLedger pins the native-console ledger: only the three
// 3-rail vendors carry rails; everyone else is enrollment-only (Any()
// false). Phase 4 keeps this a ledger — no new poller is built.
func TestNativeRailsLedger(t *testing.T) {
	withRails := map[string]bool{"claude-code": true, "codex": true, "copilot": true}
	for _, c := range Capabilities() {
		got := c.Native.Any()
		want := withRails[c.Tool]
		if got != want {
			t.Errorf("%s: Native.Any() = %v, want %v", c.Tool, got, want)
		}
	}
}

// TestTokenTierGroundedForEveryAdapter ensures the discovery spike left no
// adapter with an un-set best-tier (the ledger Phase 5 measures against).
func TestTokenTierGroundedForEveryAdapter(t *testing.T) {
	for _, c := range Capabilities() {
		if c.TokenTier.Best == "" {
			t.Errorf("%s: TokenTier.Best is empty — every registered adapter must name a capture tier", c.Tool)
		}
	}
}
