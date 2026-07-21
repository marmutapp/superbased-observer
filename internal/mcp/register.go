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

	"github.com/marmutapp/superbased-observer/internal/mcp/locate"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
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
	// ConfigPath, when non-empty, is appended to the registered MCP launch
	// command as `--config <path>`. Used to keep the MCP server's view of
	// config aligned with the proxy's view when a non-default config is
	// in play (e.g. an A/B harness running its own observer-config.toml).
	// Without this, `observer init` registers `observer serve` with no
	// args, the MCP server reads ~/.observer/config.toml, and stash /
	// retrieve_stashed get out of sync with whichever proxy is actually
	// stashing bodies. Surfaced 2026-05-08 dogfood.
	ConfigPath string
	// WSLDistro names the WSL distribution invoked via wsl.exe for the
	// cross-OS "cline-windows" MCP target (mirrors hook.Options.WSLDistro).
	// Empty falls back to $WSL_DISTRO_NAME at registration time.
	WSLDistro string
	// WindowsClineHome overrides crossmount detection of the Windows-side
	// home that holds VS Code's globalStorage (e.g. /mnt/c/Users/<u>) for
	// the cline-windows target. Empty → first crossmount OS=windows home.
	WindowsClineHome string
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
	if r.dirExists(filepath.Join(r.opts.HomeDir, ".config", "opencode")) {
		tools = append(tools, "opencode")
	}
	// Native (same-OS) Cline: VS Code globalStorage settings dir present.
	if loc, ok := locate.ForClient("cline", r.opts.HomeDir); ok && r.dirExists(filepath.Dir(loc.Path)) {
		tools = append(tools, "cline")
	}
	// Cross-OS Cline: a Windows VS Code globalStorage reachable from a WSL
	// daemon via crossmount (the cline-windows target).
	if dir := r.detectWindowsClineSettingsDir(); dir != "" {
		tools = append(tools, "cline-windows")
	}
	return tools
}

func (r *Registrar) dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Register writes (or verifies) the MCP entry for tool. Supported values:
// "claude-code", "cursor", "codex". Config paths come from the shared
// internal/mcp/locate table — the one owner of per-client MCP config
// locations (guard/mcpsec reads the same table).
func (r *Registrar) Register(tool string) RegistrationResult {
	// cline-windows is a cross-OS virtual target: its config path is
	// resolved dynamically through crossmount (not the home-based locate
	// table) and its command is a wsl.exe bridge, so it dispatches by name
	// BEFORE the locate lookup (mirrors hook.registerClaudeCodeWindows).
	if tool == "cline-windows" {
		return r.registerClineWindows()
	}
	loc, ok := locate.ForClient(tool, r.opts.HomeDir)
	if !ok {
		return RegistrationResult{
			Tool:   tool,
			Error:  fmt.Errorf("mcp.Register: tool %q not supported", tool),
			DryRun: r.opts.DryRun,
		}
	}
	switch loc.Format {
	case locate.FormatCodexTOML:
		return r.registerCodexTOML(loc.Path)
	case locate.FormatOpenCodeJSON:
		return r.registerOpenCodeJSON(loc.Path)
	default:
		return r.registerJSONMCP(tool, loc.Path)
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
	return r.registerJSONMCPEntry(tool, path, mcpServerEntry{
		Command: r.opts.BinaryPath,
		Args:    r.serveArgs(),
	})
}

// registerJSONMCPEntry is the shared {"mcpServers": {...}} writer
// parametrized by the desired server entry. The native path (claude-code /
// cursor / native cline) passes a {binary, serve} entry; the cross-OS
// cline-windows path passes a {wsl.exe, [-d distro -- binary serve]} bridge
// entry. Other top-level fields and sibling servers are preserved verbatim.
func (r *Registrar) registerJSONMCPEntry(tool, path string, desired mcpServerEntry) RegistrationResult {
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

	if existing, ok := servers[ServerName]; ok {
		if existing.Command == desired.Command && stringSlicesEqual(existing.Args, desired.Args) {
			res.AlreadySet = true
			return res
		}
		// Args drifted but binary is ours — refresh silently (same
		// owner, just a flag change like adding/dropping --config).
		// Different binary keeps the old conflict semantics.
		if existing.Command != desired.Command && !r.opts.Force {
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

// registerClineWindows writes the observer MCP server into a Windows VS
// Code Cline config (cline_mcp_settings.json) from a WSL daemon, using a
// `wsl.exe -d <distro> -- <linux-bin> serve` bridge command so the Windows
// host spawns the WSL-resident observer over stdio. Mirrors
// hook.registerClaudeCodeWindows: dynamic crossmount path + wsl bridge,
// gated by the cline row's MCP.CrossOSBridge capability.
func (r *Registrar) registerClineWindows() RegistrationResult {
	res := RegistrationResult{Tool: "cline-windows", DryRun: r.opts.DryRun}
	path := r.detectWindowsClineSettings()
	if path == "" {
		res.Error = errors.New("mcp.registerClineWindows: no Windows-side VS Code Cline globalStorage detected (set WindowsClineHome or run where crossmount sees /mnt/c/Users/<u>/AppData/Roaming/Code/.../cline_mcp_settings.json)")
		return res
	}
	distro := r.opts.WSLDistro
	if distro == "" {
		distro = os.Getenv("WSL_DISTRO_NAME")
	}
	if distro == "" {
		res.Error = errors.New("mcp.registerClineWindows: WSL distro unknown — set WSLDistro or run inside WSL (so $WSL_DISTRO_NAME is set)")
		return res
	}
	// Cline (a Node VS Code extension) spawns the MCP command+args directly
	// via child_process — NOT through Git Bash — so no MSYS_NO_PATHCONV
	// prefix is needed (that guard is only for the shell-string hook case).
	desired := mcpServerEntry{
		Command: "wsl.exe",
		Args:    append([]string{"-d", distro, "--", r.opts.BinaryPath}, r.serveArgs()...),
	}
	return r.registerJSONMCPEntry("cline-windows", path, desired)
}

// detectWindowsClineSettings returns the Windows-side cline_mcp_settings.json
// path (resolved via crossmount), or "" if no Windows VS Code Cline
// globalStorage is reachable. Honors WindowsClineHome when set.
func (r *Registrar) detectWindowsClineSettings() string {
	if r.opts.WindowsClineHome != "" {
		return locate.ClineSettingsPath(r.opts.WindowsClineHome, crossmount.OSWindows)
	}
	for _, h := range crossmount.AllHomes() {
		if h.OS != crossmount.OSWindows {
			continue
		}
		p := locate.ClineSettingsPath(h.Path, crossmount.OSWindows)
		if r.dirExists(filepath.Dir(p)) {
			return p
		}
	}
	return ""
}

// detectWindowsClineSettingsDir reports the settings dir for Installed()
// detection — returns the parent dir when a Windows Cline globalStorage is
// present, "" otherwise.
func (r *Registrar) detectWindowsClineSettingsDir() string {
	p := r.detectWindowsClineSettings()
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// opencodeMCPEntry is OpenCode's MCP server shape under the top-level
// "mcp" key in opencode.json: a typed local-command server. Distinct from
// the {command,args} shape Claude Code/Cursor use — OpenCode takes the
// whole launch as one command array plus an explicit enabled flag.
type opencodeMCPEntry struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command"`
	Enabled     bool              `json:"enabled"`
	Environment map[string]string `json:"environment,omitempty"`
}

// registerOpenCodeJSON patches the "mcp" object in
// ~/.config/opencode/opencode.json. Other top-level keys ($schema, plugin,
// …) are preserved verbatim by writeJSON, and OTHER mcp servers are kept as
// raw JSON so we never drop fields we don't model (e.g. remote servers'
// "url"). Only the "observer" entry is authored.
func (r *Registrar) registerOpenCodeJSON(path string) RegistrationResult {
	tool := "opencode"
	res := RegistrationResult{Tool: tool, ConfigPath: path, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		res.Error = fmt.Errorf("mcp.register opencode: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			res.Error = fmt.Errorf("mcp.register opencode: parse %s: %w", path, err)
			return res
		}
	}
	// Keep sibling servers as raw JSON so unmodelled fields survive.
	servers := map[string]json.RawMessage{}
	if existing, ok := settings["mcp"]; ok {
		if err := json.Unmarshal(existing, &servers); err != nil {
			res.Error = fmt.Errorf("mcp.register opencode: parse mcp: %w", err)
			return res
		}
	}

	desired := opencodeMCPEntry{
		Type:    "local",
		Command: append([]string{r.opts.BinaryPath}, r.serveArgs()...),
		Enabled: true,
	}
	if cur, ok := servers[ServerName]; ok {
		var curEntry opencodeMCPEntry
		// A parse failure here means a malformed prior entry — fall through
		// to overwrite (curEntry stays zero, so neither guard below trips a
		// false "already set").
		_ = json.Unmarshal(cur, &curEntry)
		if curEntry.Type == desired.Type && curEntry.Enabled == desired.Enabled &&
			stringSlicesEqual(curEntry.Command, desired.Command) {
			res.AlreadySet = true
			return res
		}
		// Command drifted but the head binary is ours — refresh silently. A
		// different binary at the head keeps the conflict semantics.
		if (len(curEntry.Command) == 0 || curEntry.Command[0] != r.opts.BinaryPath) && !r.opts.Force {
			res.Error = fmt.Errorf("mcp.register opencode: %s already points at %v; pass --force to overwrite",
				ServerName, curEntry.Command)
			return res
		}
	}

	desiredRaw, err := json.Marshal(desired)
	if err != nil {
		res.Error = fmt.Errorf("mcp.register opencode: marshal entry: %w", err)
		return res
	}
	servers[ServerName] = desiredRaw

	patched, err := json.Marshal(servers)
	if err != nil {
		res.Error = fmt.Errorf("mcp.register opencode: marshal mcp: %w", err)
		return res
	}
	settings["mcp"] = patched

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

// registerCodexTOML patches ~/.codex/config.toml's [mcp_servers] table.
// Unknown TOML sections are preserved by round-tripping through a generic
// map; the order of unrelated keys may change but content is preserved.
// Comments will not survive — this is documented in the user-facing init
// output.
func (r *Registrar) registerCodexTOML(path string) RegistrationResult {
	tool := "codex"
	dir := filepath.Dir(path)
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

	desiredArgs := r.serveArgs()
	desired := map[string]any{
		"command": r.opts.BinaryPath,
		"args":    desiredArgs,
	}
	if existing, ok := servers[ServerName].(map[string]any); ok {
		cmd, _ := existing["command"].(string)
		if cmd == r.opts.BinaryPath && tomlArgsEqual(existing["args"], desiredArgs) {
			res.AlreadySet = true
			return res
		}
		// Args drifted but binary is ours — refresh silently. Different
		// binary keeps the old conflict semantics.
		if cmd != r.opts.BinaryPath && !r.opts.Force {
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
	if tool == "cline-windows" {
		path := r.detectWindowsClineSettings()
		if path == "" {
			return UnregistrationResult{Tool: tool, Skipped: true, DryRun: r.opts.DryRun}
		}
		return r.unregisterJSONMCP(tool, path)
	}
	loc, ok := locate.ForClient(tool, r.opts.HomeDir)
	if !ok {
		return UnregistrationResult{
			Tool:   tool,
			Error:  fmt.Errorf("mcp.Unregister: tool %q not supported", tool),
			DryRun: r.opts.DryRun,
		}
	}
	switch loc.Format {
	case locate.FormatCodexTOML:
		return r.unregisterCodexTOML(loc.Path)
	case locate.FormatOpenCodeJSON:
		return r.unregisterOpenCodeJSON(loc.Path)
	default:
		return r.unregisterJSONMCP(tool, loc.Path)
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

// unregisterOpenCodeJSON removes the "observer" entry from the "mcp" object
// in opencode.json, preserving sibling servers (as raw JSON) and all other
// top-level keys. The "mcp" key is dropped when it empties, and the file is
// removed when nothing else remains.
func (r *Registrar) unregisterOpenCodeJSON(path string) UnregistrationResult {
	tool := "opencode"
	res := UnregistrationResult{Tool: tool, ConfigPath: path, DryRun: r.opts.DryRun}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Skipped = true
			return res
		}
		res.Error = fmt.Errorf("mcp.unregister opencode: read: %w", err)
		return res
	}
	settings := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &settings); err != nil {
		res.Error = fmt.Errorf("mcp.unregister opencode: parse %s: %w", path, err)
		return res
	}
	servers := map[string]json.RawMessage{}
	if existing, ok := settings["mcp"]; ok {
		if err := json.Unmarshal(existing, &servers); err != nil {
			res.Error = fmt.Errorf("mcp.unregister opencode: parse mcp: %w", err)
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
		delete(settings, "mcp")
	} else {
		patched, err := json.Marshal(servers)
		if err != nil {
			res.Error = fmt.Errorf("mcp.unregister opencode: marshal mcp: %w", err)
			return res
		}
		settings["mcp"] = patched
	}

	if r.opts.DryRun {
		return res
	}
	if len(settings) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			res.Error = fmt.Errorf("mcp.unregister opencode: remove %s: %w", path, err)
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

func (r *Registrar) unregisterCodexTOML(path string) UnregistrationResult {
	tool := "codex"
	dir := filepath.Dir(path)
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

// serveArgs returns the argv (after the binary name) that the registered
// MCP launch command will use. When ConfigPath is set, the launch
// becomes `<binary> serve --config <path>` so the MCP server reads the
// same config as the proxy that's running it.
func (r *Registrar) serveArgs() []string {
	if r.opts.ConfigPath == "" {
		return []string{"serve"}
	}
	return []string{"serve", "--config", r.opts.ConfigPath}
}

// stringSlicesEqual reports whether two string slices are element-wise
// equal. Used in the idempotency check so a change in registered args
// (e.g. new --config flag) re-writes the entry instead of being
// treated as already-set.
func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tomlArgsEqual handles the codex TOML round-trip case: BurntSushi/toml
// unmarshals string arrays into []any rather than []string, so a strict
// type-asserted equality check would always miss. This walks both
// shapes and compares element-wise.
func tomlArgsEqual(existing any, desired []string) bool {
	existingSlice, ok := existing.([]any)
	if !ok {
		return false
	}
	if len(existingSlice) != len(desired) {
		return false
	}
	for i, v := range existingSlice {
		s, ok := v.(string)
		if !ok || s != desired[i] {
			return false
		}
	}
	return true
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
