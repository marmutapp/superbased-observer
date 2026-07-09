package hook

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// HermesMCPServerName is the key we write under config.yaml's
// `mcp_servers` block. Matches the dashboard's expected server-name
// convention for observer-installed servers.
const HermesMCPServerName = "superbased-observer"

// RegisterHermesPluginEnabled adds the superbased-observer plugin
// name to the `plugins.enabled` allow-list in
// <hermes-home>/config.yaml. Hermes's plugin loader auto-discovers
// plugin directories on startup BUT honours an explicit allow-list
// when present: a plugin not listed is skipped with
// "Skipping '<name>' (not in plugins.enabled)" in the plugin debug
// log.
//
// Validation 2026-06-06 caught this — without the enabled-list
// entry, the dropped plugin files at ~/.hermes/plugins/superbased-
// observer/ are parsed but never loaded, so no hooks fire and the
// real-time capture path silently does nothing.
//
// Safe against pre-existing `plugins:` blocks: merges into the
// existing list, no-op when the entry is already present. Creates
// the `plugins:` and `plugins.enabled:` keys when absent.
func RegisterHermesPluginEnabled(opts HermesOptions) (string, error) {
	configPath, err := hermesConfigPath(opts)
	if err != nil {
		return "", err
	}
	doc, err := readYAMLMap(configPath)
	if err != nil {
		return configPath, fmt.Errorf("hook.RegisterHermesPluginEnabled: read: %w", err)
	}
	plugins, _ := doc["plugins"].(map[string]any)
	if plugins == nil {
		plugins = map[string]any{}
	}
	enabledRaw, _ := plugins["enabled"].([]any)
	enabled := append([]any(nil), enabledRaw...)
	for _, v := range enabled {
		if s, ok := v.(string); ok && s == HermesPluginDirName {
			// Already enabled — nothing to do.
			return configPath, nil
		}
	}
	enabled = append(enabled, HermesPluginDirName)
	plugins["enabled"] = enabled
	doc["plugins"] = plugins

	if opts.DryRun {
		return configPath, nil
	}
	if err := writeYAMLMap(configPath, doc); err != nil {
		return configPath, fmt.Errorf("hook.RegisterHermesPluginEnabled: write: %w", err)
	}
	return configPath, nil
}

// UnregisterHermesPluginEnabled removes the superbased-observer
// entry from `plugins.enabled` in config.yaml. Safe against absent
// blocks or already-clean state. Removes the `enabled` key when it
// would otherwise be empty; removes the `plugins` key when IT would
// otherwise be empty.
func UnregisterHermesPluginEnabled(opts HermesOptions) (string, error) {
	configPath, err := hermesConfigPath(opts)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return configPath, nil
	} else if err != nil {
		return configPath, fmt.Errorf("hook.UnregisterHermesPluginEnabled: stat: %w", err)
	}
	doc, err := readYAMLMap(configPath)
	if err != nil {
		return configPath, fmt.Errorf("hook.UnregisterHermesPluginEnabled: read: %w", err)
	}
	plugins, _ := doc["plugins"].(map[string]any)
	if plugins == nil {
		return configPath, nil
	}
	enabledRaw, _ := plugins["enabled"].([]any)
	out := enabledRaw[:0]
	found := false
	for _, v := range enabledRaw {
		if s, ok := v.(string); ok && s == HermesPluginDirName {
			found = true
			continue
		}
		out = append(out, v)
	}
	if !found {
		return configPath, nil
	}
	if len(out) == 0 {
		delete(plugins, "enabled")
	} else {
		plugins["enabled"] = out
	}
	if len(plugins) == 0 {
		delete(doc, "plugins")
	} else {
		doc["plugins"] = plugins
	}
	if opts.DryRun {
		return configPath, nil
	}
	if err := writeYAMLMap(configPath, doc); err != nil {
		return configPath, fmt.Errorf("hook.UnregisterHermesPluginEnabled: write: %w", err)
	}
	return configPath, nil
}

// RegisterHermesMCP merges the SuperBased Observer MCP entry into
// `<hermes-home>/config.yaml` under the `mcp_servers` key. The merge
// is point-mutating — we read the file, set / overwrite the one
// `mcp_servers.superbased-observer` entry, and write back. Other
// top-level keys (inference, plugins, gateway, …) and other
// mcp_servers.* siblings are preserved verbatim.
//
// Important: yaml.v3's unmarshal-into-map round-trip DOES lose
// comments and the original key order. Most Hermes operators don't
// hand-edit comments into config.yaml (the file is mostly generated
// from `hermes config set` and `hermes setup`), but operators with
// extensively-commented configs will lose that polish. Upgrade path:
// switch to yaml.Node for preservation in a follow-up. Tracked in
// docs/plans/hermes-adapter-implementation-plan-2026-06-05.md.
//
// Returns the path of the config file written and any error.
func RegisterHermesMCP(opts HermesOptions) (string, error) {
	configPath, err := hermesConfigPath(opts)
	if err != nil {
		return "", err
	}

	doc, err := readYAMLMap(configPath)
	if err != nil {
		return configPath, fmt.Errorf("hook.RegisterHermesMCP: read: %w", err)
	}

	entry := map[string]any{
		"command": "observer",
		"args":    []any{"serve"},
	}
	if opts.BinaryPath != "" {
		entry["command"] = opts.BinaryPath
	}

	mcpServers, _ := doc["mcp_servers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
	}
	mcpServers[HermesMCPServerName] = entry
	doc["mcp_servers"] = mcpServers

	if opts.DryRun {
		return configPath, nil
	}
	if err := writeYAMLMap(configPath, doc); err != nil {
		return configPath, fmt.Errorf("hook.RegisterHermesMCP: write: %w", err)
	}
	return configPath, nil
}

// UnregisterHermesMCP removes the SuperBased Observer entry from
// config.yaml's mcp_servers block. Safe against an absent file or an
// already-clean configuration (returns nil + the path that would
// have been written).
//
// Other mcp_servers.* siblings are left untouched; the `mcp_servers`
// key is removed entirely only when it would otherwise be empty.
func UnregisterHermesMCP(opts HermesOptions) (string, error) {
	configPath, err := hermesConfigPath(opts)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return configPath, nil
	} else if err != nil {
		return configPath, fmt.Errorf("hook.UnregisterHermesMCP: stat: %w", err)
	}

	doc, err := readYAMLMap(configPath)
	if err != nil {
		return configPath, fmt.Errorf("hook.UnregisterHermesMCP: read: %w", err)
	}

	mcpServers, _ := doc["mcp_servers"].(map[string]any)
	if mcpServers == nil {
		return configPath, nil
	}
	if _, exists := mcpServers[HermesMCPServerName]; !exists {
		return configPath, nil
	}
	delete(mcpServers, HermesMCPServerName)
	if len(mcpServers) == 0 {
		delete(doc, "mcp_servers")
	} else {
		doc["mcp_servers"] = mcpServers
	}

	if opts.DryRun {
		return configPath, nil
	}
	if err := writeYAMLMap(configPath, doc); err != nil {
		return configPath, fmt.Errorf("hook.UnregisterHermesMCP: write: %w", err)
	}
	return configPath, nil
}

// hermesConfigPath returns the full path to <hermes-home>/config.yaml.
func hermesConfigPath(opts HermesOptions) (string, error) {
	home, err := resolveHermesHome(opts)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, HermesConfigFile), nil
}

// readYAMLMap reads path as a YAML mapping. Returns an empty map when
// the file is absent so callers can unconditionally mutate-and-write.
// An empty file yields an empty map too.
func readYAMLMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("readYAMLMap: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// writeYAMLMap writes m back to path with 2-space indent (Hermes
// convention) and 0o644 mode. Creates the parent directory if
// missing.
func writeYAMLMap(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf []byte
	out, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	buf = out
	return os.WriteFile(path, buf, 0o600)
}
