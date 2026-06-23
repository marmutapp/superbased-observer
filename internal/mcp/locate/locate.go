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

import "path/filepath"

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
	}
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
