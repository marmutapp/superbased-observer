// Package claudeplugin owns the identity of the SuperBased Claude Code
// plugin and detects whether it is installed on a host.
//
// Two surfaces can wire observer into Claude Code, and they overlap:
//
//   - `observer init --claude-code` writes hook commands into
//     ~/.claude/settings.json and an MCP entry into ~/.claude.json;
//   - the Claude Code plugin (generated into plugins/claude-code/ by
//     plugins/plugingen) declares the SAME hooks and MCP server in the
//     tool's own packaging format.
//
// Claude Code merges hook configuration from every source, so a user with
// both gets each hook event fired twice and the observer MCP tool schema
// loaded twice. This package is how the init registrars and `observer
// doctor` notice that overlap: it is the one owner of the plugin's name,
// its marketplace name, and the on-disk artifacts that prove it installed.
//
// Detection is filesystem-only (no DB, no network) and every read is
// best-effort: an unreadable or malformed file yields "not detected"
// rather than an error, because a false positive would silently withhold
// hook registration from a user who has no plugin at all.
package claudeplugin
