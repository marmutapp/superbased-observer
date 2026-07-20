// Package locate is the single owner of "where does each AI client
// keep its user-level MCP server configuration" (guard spec §9.1).
//
// Two consumers share this table so the knowledge is never re-derived:
//
//   - internal/mcp's Registrar (observer init) writes the observer MCP
//     entry into these files;
//   - internal/guard/mcpsec (G10) reads the same files to inventory
//     and pin every configured MCP server.
//
// The package is deliberately a stdlib-only leaf: mcpsec sits under
// internal/guard, which internal/store imports, so it must not pull
// the full internal/mcp dependency tree (an import cycle through
// store would result). Path or format knowledge for a new client is
// added HERE, nowhere else.
//
// Scope: USER-LEVEL configs only. Project-scoped MCP registries
// (claude-code's <root>/.mcp.json, cursor's <root>/.cursor/mcp.json)
// are watched by the R-304 policy rule but are not inventoried —
// enumerating every project root is the watcher's domain, not a
// static locator's (documented approximation, revisit if §9 grows a
// per-project inventory).
package locate

import (
	"path/filepath"
	"runtime"
)

// Format identifies the on-disk MCP config encoding.
type Format string

// Format values.
const (
	// FormatMCPServersJSON is the shared {"mcpServers": {...}} JSON
	// shape used by Claude Code (~/.claude.json) and Cursor
	// (~/.cursor/mcp.json).
	FormatMCPServersJSON Format = "mcpservers_json"
	// FormatCodexTOML is Codex's [mcp_servers] table inside
	// ~/.codex/config.toml.
	FormatCodexTOML Format = "codex_toml"
	// FormatOpenCodeJSON is OpenCode's own MCP shape: a "mcp" object in
	// ~/.config/opencode/opencode.json whose entries are typed local-
	// command servers ({"type":"local","command":[…],"enabled":true}),
	// NOT the {"mcpServers":{…}} {command,args} shape Claude Code/Cursor
	// use. It needs its own writer (registerOpenCodeJSON).
	FormatOpenCodeJSON Format = "opencode_json"
)

// Location is one client's user-level MCP config file.
type Location struct {
	// Client is the canonical client name ("claude-code", "cursor",
	// "codex") — the same vocabulary hook registration and the
	// adapters use.
	Client string
	// Path is the absolute config file path under home.
	Path string
	// Format selects the parser for the file.
	Format Format
}

// Locations returns every supported client's MCP config location for
// the given home directory, in stable order. Existence is NOT
// checked — callers decide whether a missing file matters.
func Locations(home string) []Location {
	return []Location{
		{Client: "claude-code", Path: filepath.Join(home, ".claude.json"), Format: FormatMCPServersJSON},
		{Client: "cursor", Path: filepath.Join(home, ".cursor", "mcp.json"), Format: FormatMCPServersJSON},
		{Client: "codex", Path: filepath.Join(home, ".codex", "config.toml"), Format: FormatCodexTOML},
		// OpenCode reads a global config at ~/.config/opencode/opencode.json
		// (and per-project opencode.json files). We register globally so MCP
		// is available in every project, mirroring claude-code/cursor. The
		// path follows OpenCode's XDG default; $XDG_CONFIG_HOME overrides are
		// not yet honoured here (consistent with the home-only Locations
		// signature).
		{Client: "opencode", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), Format: FormatOpenCodeJSON},
		// Cline (VS Code extension `saoudrizwan.claude-dev`) keeps MCP config
		// in VS Code's globalStorage under the daemon OS's config root. This
		// is the NATIVE (same-OS) path; the cross-OS Windows path (a WSL
		// daemon writing a Windows VS Code Cline) is resolved dynamically via
		// crossmount in the registrar (the cline-windows virtual target).
		{Client: "cline", Path: ClineSettingsPath(home, runtime.GOOS), Format: FormatMCPServersJSON},
	}
}

// clineSettingsRelparts is the VS Code globalStorage tail for Cline's MCP
// settings file, shared by the native and cross-OS path resolvers so the
// shape has ONE owner.
var clineSettingsRelparts = []string{"Code", "User", "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json"}

// ClineSettingsPath builds the absolute cline_mcp_settings.json path for a
// home directory under the given OS's VS Code config-root convention:
// windows %APPDATA% (home/AppData/Roaming), darwin ~/Library/Application
// Support, linux (and anything else) XDG ~/.config. Used for BOTH the
// native daemon-OS path (Locations) and the cross-OS Windows path (the MCP
// registrar via crossmount), so the relative tail lives in exactly one
// place. goos uses runtime.GOOS values ("windows"/"darwin"/"linux").
func ClineSettingsPath(home, goos string) string {
	var configRoot string
	switch goos {
	case "windows":
		configRoot = filepath.Join(home, "AppData", "Roaming")
	case "darwin":
		configRoot = filepath.Join(home, "Library", "Application Support")
	default:
		configRoot = filepath.Join(home, ".config")
	}
	return filepath.Join(append([]string{configRoot}, clineSettingsRelparts...)...)
}

// ForClient returns the location for one client, false when the
// client has no known MCP config.
func ForClient(client, home string) (Location, bool) {
	for _, loc := range Locations(home) {
		if loc.Client == client {
			return loc, true
		}
	}
	return Location{}, false
}
