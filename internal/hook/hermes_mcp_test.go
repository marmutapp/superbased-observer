package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRegisterHermesMCP_FreshConfig pins the install-into-empty
// case. No config.yaml exists; RegisterHermesMCP creates one with
// just the superbased-observer entry.
func TestRegisterHermesMCP_FreshConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	got, err := RegisterHermesMCP(HermesOptions{HermesHome: home})
	if err != nil {
		t.Fatalf("RegisterHermesMCP: %v", err)
	}
	if got != filepath.Join(home, "config.yaml") {
		t.Errorf("config path = %q", got)
	}
	out := loadYAML(t, got)
	mcp := out["mcp_servers"].(map[string]any)
	entry := mcp["superbased-observer"].(map[string]any)
	if entry["command"] != "observer" {
		t.Errorf("command = %v", entry["command"])
	}
}

// TestRegisterHermesMCP_MergesIntoExistingConfig pins the merge
// semantics. A pre-existing config with `inference:` block and one
// non-observer mcp_server gets the superbased-observer entry added
// without touching the rest.
func TestRegisterHermesMCP_MergesIntoExistingConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	before := `inference:
  provider: anthropic
  model: claude-sonnet-4.6
mcp_servers:
  filesystem:
    command: mcp-fs
    args:
    - serve
plugins:
  disabled: []
`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := RegisterHermesMCP(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("RegisterHermesMCP: %v", err)
	}
	out := loadYAML(t, path)
	// inference block preserved.
	inf := out["inference"].(map[string]any)
	if inf["model"] != "claude-sonnet-4.6" {
		t.Errorf("inference.model lost: %v", inf["model"])
	}
	// Both MCP servers exist.
	mcp := out["mcp_servers"].(map[string]any)
	if _, ok := mcp["filesystem"]; !ok {
		t.Error("pre-existing filesystem MCP entry was dropped")
	}
	if _, ok := mcp["superbased-observer"]; !ok {
		t.Error("superbased-observer not added")
	}
	// plugins block preserved.
	if _, ok := out["plugins"]; !ok {
		t.Error("plugins block dropped")
	}
}

// TestRegisterHermesMCP_BinaryPathLandsInCommand pins the BinaryPath
// option: when set, it overrides "observer" as the command — needed
// when observer isn't on $PATH for the agent.
func TestRegisterHermesMCP_BinaryPathLandsInCommand(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, err := RegisterHermesMCP(HermesOptions{
		HermesHome: home,
		BinaryPath: "/usr/local/bin/observer-v1.8.3",
	}); err != nil {
		t.Fatalf("RegisterHermesMCP: %v", err)
	}
	out := loadYAML(t, filepath.Join(home, "config.yaml"))
	entry := out["mcp_servers"].(map[string]any)["superbased-observer"].(map[string]any)
	if entry["command"] != "/usr/local/bin/observer-v1.8.3" {
		t.Errorf("command = %v, want absolute BinaryPath", entry["command"])
	}
}

// TestUnregisterHermesMCP_RemovesOnlyOurEntry pins the surgical
// removal. Other mcp_servers siblings must survive.
func TestUnregisterHermesMCP_RemovesOnlyOurEntry(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	before := `mcp_servers:
  filesystem:
    command: mcp-fs
    args:
    - serve
  superbased-observer:
    command: observer
    args:
    - serve
`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := UnregisterHermesMCP(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("UnregisterHermesMCP: %v", err)
	}
	out := loadYAML(t, path)
	mcp := out["mcp_servers"].(map[string]any)
	if _, ok := mcp["superbased-observer"]; ok {
		t.Error("superbased-observer not removed")
	}
	if _, ok := mcp["filesystem"]; !ok {
		t.Error("sibling filesystem entry was removed")
	}
}

// TestUnregisterHermesMCP_DropsEmptyMCPServersKey pins the post-
// removal cleanup: when removing the last mcp_servers entry, the
// whole mcp_servers key is removed too (avoids leaving an
// empty-mapping yaml.v3 marshals as `mcp_servers: {}`).
func TestUnregisterHermesMCP_DropsEmptyMCPServersKey(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, err := RegisterHermesMCP(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("seed via RegisterHermesMCP: %v", err)
	}
	if _, err := UnregisterHermesMCP(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("UnregisterHermesMCP: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(data), "mcp_servers") {
		t.Errorf("mcp_servers key still present after removing last entry:\n%s", data)
	}
}

// TestUnregisterHermesMCP_AbsentConfigIsNoop pins the idempotent
// uninstall on a system that never had MCP wired.
func TestUnregisterHermesMCP_AbsentConfigIsNoop(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, err := UnregisterHermesMCP(HermesOptions{HermesHome: home}); err != nil {
		t.Errorf("UnregisterHermesMCP on absent config: %v", err)
	}
	// Config file should still not exist.
	if _, err := os.Stat(filepath.Join(home, "config.yaml")); !os.IsNotExist(err) {
		t.Errorf("UnregisterHermesMCP created a config file: %v", err)
	}
}

// TestRegisterHermesPluginEnabled_FreshConfig pins the auto-enable
// path on a system without any pre-existing plugins block. The
// validation 2026-06-06 finding: without this entry, Hermes parses
// the plugin manifest but skips loading the plugin — silent capture
// gap.
func TestRegisterHermesPluginEnabled_FreshConfig(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, err := RegisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("RegisterHermesPluginEnabled: %v", err)
	}
	out := loadYAML(t, filepath.Join(home, "config.yaml"))
	plugins := out["plugins"].(map[string]any)
	enabled := plugins["enabled"].([]any)
	if len(enabled) != 1 || enabled[0] != HermesPluginDirName {
		t.Errorf("enabled = %v, want [%q]", enabled, HermesPluginDirName)
	}
}

// TestRegisterHermesPluginEnabled_IdempotentReadd confirms re-running
// against an already-enabled plugin doesn't duplicate the entry.
func TestRegisterHermesPluginEnabled_IdempotentReadd(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, err := RegisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := RegisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("second: %v", err)
	}
	out := loadYAML(t, filepath.Join(home, "config.yaml"))
	enabled := out["plugins"].(map[string]any)["enabled"].([]any)
	if len(enabled) != 1 {
		t.Errorf("enabled = %v, want exactly 1 entry", enabled)
	}
}

// TestRegisterHermesPluginEnabled_PreservesSiblings confirms we
// merge into an existing enabled-list rather than overwriting.
func TestRegisterHermesPluginEnabled_PreservesSiblings(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte("plugins:\n  enabled:\n    - web-tavily\n    - fal\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := RegisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("RegisterHermesPluginEnabled: %v", err)
	}
	enabled := loadYAML(t, path)["plugins"].(map[string]any)["enabled"].([]any)
	if len(enabled) != 3 {
		t.Fatalf("enabled = %v, want 3 entries", enabled)
	}
	want := map[string]bool{"web-tavily": false, "fal": false, HermesPluginDirName: false}
	for _, v := range enabled {
		if s, ok := v.(string); ok {
			want[s] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("entry %q missing from final enabled list", k)
		}
	}
}

// TestUnregisterHermesPluginEnabled_RemovesOnlyOurEntry confirms
// surgical removal — sibling plugins on the enabled-list survive.
func TestUnregisterHermesPluginEnabled_RemovesOnlyOurEntry(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte("plugins:\n  enabled:\n    - web-tavily\n    - superbased-observer\n    - fal\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := UnregisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("UnregisterHermesPluginEnabled: %v", err)
	}
	enabled := loadYAML(t, path)["plugins"].(map[string]any)["enabled"].([]any)
	if len(enabled) != 2 {
		t.Errorf("enabled = %v, want 2 (siblings only)", enabled)
	}
	for _, v := range enabled {
		if s, _ := v.(string); s == HermesPluginDirName {
			t.Error("superbased-observer not removed")
		}
	}
}

// TestUnregisterHermesPluginEnabled_DropsEmptyKeys confirms cleanup
// when our entry was the only one — both `plugins.enabled` and the
// `plugins:` parent get removed to avoid leaving `plugins: {}` YAML.
func TestUnregisterHermesPluginEnabled_DropsEmptyKeys(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	if _, err := RegisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := UnregisterHermesPluginEnabled(HermesOptions{HermesHome: home}); err != nil {
		t.Fatalf("UnregisterHermesPluginEnabled: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(home, "config.yaml"))
	if strings.Contains(string(data), "plugins") {
		t.Errorf("plugins key still present after removing only entry:\n%s", data)
	}
}

// loadYAML is a test helper that parses path as a YAML map.
func loadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("yaml.Unmarshal: %v\nbody:\n%s", err, data)
	}
	return m
}
