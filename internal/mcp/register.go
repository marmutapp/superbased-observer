package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"
)

// ServerName is the registration entry name written into each AI tool's
// MCP config. Stable so re-running init is idempotent.
const ServerName = "observer"

// RegistrationResult summarizes one MCP registration.
type RegistrationResult struct {
	Tool       string // claude-code | cursor | codex
	ConfigPath string
	Added      bool
	AlreadySet bool
	DryRun     bool
	Error      error
}

// RegisterOptions parameterizes Registrar.
type RegisterOptions struct {
	// BinaryPath is the absolute path to the running observer binary that
	// the AI tool will invoke as `<binary> serve`. Required.
	BinaryPath string
	// DryRun computes the result without touching files.
	DryRun bool
	// Force overwrites an existing entry that points to a different binary.
	// When false, conflicts are reported as errors.
	Force bool
	// HomeDir overrides $HOME (used by tests).
	HomeDir string
}

// Registrar dispatches MCP registrations per tool.
type Registrar struct{ opts RegisterOptions }

// NewRegistrar validates opts and returns a Registrar.
func NewRegistrar(opts RegisterOptions) (*Registrar, error) {
	if opts.BinaryPath == "" {
		return nil, errors.New("mcp.NewRegistrar: BinaryPath is required")
	}
	if opts.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("mcp.NewRegistrar: UserHomeDir: %w", err)
		}
		opts.HomeDir = home
	}
	return &Registrar{opts: opts}, nil
}

// Installed reports which supported tools have a config directory present.
// Mirrors hook.Registry.Installed so the CLI can detect both hook-capable and
// MCP-capable tools off the same probe.
func (r *Registrar) Installed() []string {
	var tools []string
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".claude")) {
		tools = append(tools, "claude-code")
	}
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".cursor")) {
		tools = append(tools, "cursor")
	}
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".codex")) {
		tools = append(tools, "codex")
	}
	return tools
}

func (r *Registrar) dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Register writes (or verifies) the MCP entry for tool. Supported values:
// "claude-code", "cursor", "codex".
func (r *Registrar) Register(tool string) RegistrationResult {
	switch tool {
	case "claude-code":
		return r.registerJSONMCP(
			tool,
			filepath.Join(r.opts.HomeDir, ".claude.json"),
		)
	case "cursor":
		return r.registerJSONMCP(
			tool,
			filepath.Join(r.opts.HomeDir, ".cursor", "mcp.json"),
		)
	case "codex":
		return r.registerCodexTOML()
	default:
		return RegistrationResult{
			Tool:   tool,
			Error:  fmt.Errorf("mcp.Register: tool %q not supported", tool),
			DryRun: r.opts.DryRun,
		}
	}
}

// mcpServerEntry is the canonical shape both Claude Code and Cursor accept
// for a stdio server. Optional fields (env, working_dir) are omitted unless
// the user opts in via a separate API.
type mcpServerEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// registerJSONMCP handles the shared {"mcpServers": {...}} JSON shape used
// by Claude Code (~/.claude.json) and Cursor (~/.cursor/mcp.json). Other
// top-level fields are preserved verbatim so we don't clobber user config.
func (r *Registrar) registerJSONMCP(tool, path string) RegistrationResult {
	res := RegistrationResult{Tool: tool, ConfigPath: path, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("mcp.register %s: read: %w", tool, err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			res.Error = fmt.Errorf("mcp.register %s: parse %s: %w", tool, path, err)
			return res
		}
	}
	servers := map[string]mcpServerEntry{}
	if existing, ok := settings["mcpServers"]; ok {
		if err := json.Unmarshal(existing, &servers); err != nil {
			res.Error = fmt.Errorf("mcp.register %s: parse mcpServers: %w", tool, err)
			return res
		}
	}

	desired := mcpServerEntry{
		Command: r.opts.BinaryPath,
		Args:    []string{"serve"},
	}
	if existing, ok := servers[ServerName]; ok {
		if existing.Command == desired.Command {
			res.AlreadySet = true
			return res
		}
		if !r.opts.Force {
			res.Error = fmt.Errorf("mcp.register %s: %s already points at %q; pass --force to overwrite",
				tool, ServerName, existing.Command)
			return res
		}
	}
	servers[ServerName] = desired

	patched, err := json.Marshal(servers)
	if err != nil {
		res.Error = fmt.Errorf("mcp.register %s: marshal: %w", tool, err)
		return res
	}
	settings["mcpServers"] = patched

	if r.opts.DryRun {
		res.Added = true
		return res
	}
	if err := writeJSON(path, settings); err != nil {
		res.Error = err
		return res
	}
	res.Added = true
	return res
}

// codexEntry mirrors the TOML [mcp_servers.<name>] table.
type codexEntry struct {
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
}

// registerCodexTOML patches ~/.codex/config.toml's [mcp_servers] table.
// Unknown TOML sections are preserved by round-tripping through a generic
// map; the order of unrelated keys may change but content is preserved.
// Comments will not survive — this is documented in the user-facing init
// output.
func (r *Registrar) registerCodexTOML() RegistrationResult {
	tool := "codex"
	dir := filepath.Join(r.opts.HomeDir, ".codex")
	path := filepath.Join(dir, "config.toml")
	res := RegistrationResult{Tool: tool, ConfigPath: path, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("mcp.register codex: read: %w", err)
		return res
	}
	root := map[string]any{}
	if len(raw) > 0 {
		if err := toml.Unmarshal(raw, &root); err != nil {
			res.Error = fmt.Errorf("mcp.register codex: parse %s: %w", path, err)
			return res
		}
	}

	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	desired := map[string]any{
		"command": r.opts.BinaryPath,
		"args":    []string{"serve"},
	}
	if existing, ok := servers[ServerName].(map[string]any); ok {
		if cmd, _ := existing["command"].(string); cmd == r.opts.BinaryPath {
			res.AlreadySet = true
			return res
		}
		if !r.opts.Force {
			res.Error = fmt.Errorf("mcp.register codex: %s already points at %v; pass --force to overwrite",
				ServerName, existing["command"])
			return res
		}
	}
	servers[ServerName] = desired
	root["mcp_servers"] = servers

	if r.opts.DryRun {
		res.Added = true
		return res
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		res.Error = fmt.Errorf("mcp.register codex: encode: %w", err)
		return res
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Error = fmt.Errorf("mcp.register codex: mkdir: %w", err)
		return res
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		res.Error = fmt.Errorf("mcp.register codex: write: %w", err)
		return res
	}
	if err := os.Rename(tmp, path); err != nil {
		res.Error = fmt.Errorf("mcp.register codex: rename: %w", err)
		return res
	}
	res.Added = true
	return res
}

// UnregistrationResult summarizes one MCP unregistration.
type UnregistrationResult struct {
	Tool       string // claude-code | cursor | codex
	ConfigPath string
	Removed    bool // observer entry was present and has been deleted
	Skipped    bool // config file missing or the observer entry was already absent
	DryRun     bool
	Error      error
}

// Unregister removes the "observer" MCP entry from tool's config file.
// Other MCP servers and all other top-level config keys are preserved
// verbatim. Supported tools: "claude-code", "cursor", "codex".
func (r *Registrar) Unregister(tool string) UnregistrationResult {
	switch tool {
	case "claude-code":
		return r.unregisterJSONMCP(tool, filepath.Join(r.opts.HomeDir, ".claude.json"))
	case "cursor":
		return r.unregisterJSONMCP(tool, filepath.Join(r.opts.HomeDir, ".cursor", "mcp.json"))
	case "codex":
		return r.unregisterCodexTOML()
	default:
		return UnregistrationResult{
			Tool:   tool,
			Error:  fmt.Errorf("mcp.Unregister: tool %q not supported", tool),
			DryRun: r.opts.DryRun,
		}
	}
}

func (r *Registrar) unregisterJSONMCP(tool, path string) UnregistrationResult {
	res := UnregistrationResult{Tool: tool, ConfigPath: path, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("mcp.unregister %s: read: %w", tool, err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		res.Error = fmt.Errorf("mcp.unregister %s: parse %s: %w", tool, path, err)
		return res
	}
	servers := map[string]mcpServerEntry{}
	if existing, ok := settings["mcpServers"]; ok {
		if err := json.Unmarshal(existing, &servers); err != nil {
			res.Error = fmt.Errorf("mcp.unregister %s: parse mcpServers: %w", tool, err)
			return res
		}
	}
	if _, ok := servers[ServerName]; !ok {
		res.Skipped = true
		return res
	}
	delete(servers, ServerName)
	res.Removed = true

	if len(servers) == 0 {
		delete(settings, "mcpServers")
	} else {
		patched, err := json.Marshal(servers)
		if err != nil {
			res.Error = fmt.Errorf("mcp.unregister %s: marshal: %w", tool, err)
			return res
		}
		settings["mcpServers"] = patched
	}

	if r.opts.DryRun {
		return res
	}
	if len(settings) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Error = fmt.Errorf("mcp.unregister %s: remove %s: %w", tool, path, err)
			return res
		}
		return res
	}
	if err := writeJSON(path, settings); err != nil {
		res.Error = err
		return res
	}
	return res
}

func (r *Registrar) unregisterCodexTOML() UnregistrationResult {
	tool := "codex"
	dir := filepath.Join(r.opts.HomeDir, ".codex")
	path := filepath.Join(dir, "config.toml")
	res := UnregistrationResult{Tool: tool, ConfigPath: path, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("mcp.unregister codex: read: %w", err)
		return res
	}
	root := map[string]any{}
	if err := toml.Unmarshal(raw, &root); err != nil {
		res.Error = fmt.Errorf("mcp.unregister codex: parse %s: %w", path, err)
		return res
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		res.Skipped = true
		return res
	}
	if _, ok := servers[ServerName]; !ok {
		res.Skipped = true
		return res
	}
	delete(servers, ServerName)
	res.Removed = true

	if len(servers) == 0 {
		delete(root, "mcp_servers")
	} else {
		root["mcp_servers"] = servers
	}

	if r.opts.DryRun {
		return res
	}
	if len(root) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Error = fmt.Errorf("mcp.unregister codex: remove %s: %w", path, err)
			return res
		}
		return res
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(root); err != nil {
		res.Error = fmt.Errorf("mcp.unregister codex: encode: %w", err)
		return res
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		res.Error = fmt.Errorf("mcp.unregister codex: mkdir: %w", err)
		return res
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		res.Error = fmt.Errorf("mcp.unregister codex: write: %w", err)
		return res
	}
	if err := os.Rename(tmp, path); err != nil {
		res.Error = fmt.Errorf("mcp.unregister codex: rename: %w", err)
		return res
	}
	return res
}

// writeJSON serializes settings as 2-space JSON with stably ordered top-level
// keys so config diffs stay clean across re-runs of init.
func writeJSON(path string, settings map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mcp.write: mkdir: %w", err)
	}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	buf.WriteByte('\n')
	for i, k := range keys {
		buf.WriteString("  ")
		kk, _ := json.Marshal(k)
		buf.Write(kk)
		buf.WriteString(": ")
		var tmp any
		if err := json.Unmarshal(settings[k], &tmp); err == nil {
			pretty, _ := json.MarshalIndent(tmp, "  ", "  ")
			buf.Write(pretty)
		} else {
			buf.Write(settings[k])
		}
		if i < len(keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("mcp.write: %w", err)
	}
	return os.Rename(tmp, path)
}
