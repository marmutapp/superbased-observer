// Package hook implements the hook subcommand handlers (pre-tool, post-tool,
// pre-compact, post-compact), hook registration across AI tools (patching
// settings.json, hooks.json, etc.), and hook integrity verification via
// SHA256 checksums. See spec §14.
//
// Key invariant: hooks must exit 0 and stay under the 500ms latency budget on
// all paths — never block the host tool.
package hook
