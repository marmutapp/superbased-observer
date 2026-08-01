package tooltax

import "strings"

// The MCP name-parse constants. They are EXPORTED because the parse is
// mirrored into TypeScript: web/taxgen emits them into
// web/src/lib/actiontax.gen.json and web/src/lib/actions.ts drives its
// parser off that data instead of re-declaring the literals (WP-T2).
// A generator that hard-codes "mcp__" is exactly the drift this package
// exists to remove.
const (
	// MCPPrefix is the Claude/Anthropic MCP tool-name namespace.
	MCPPrefix = "mcp__"
	// MCPSeparator splits <server> from <tool> inside the namespace.
	MCPSeparator = "__"
	// MCPSeparatorMinIndex is the smallest offset — measured in the
	// remainder AFTER MCPPrefix — at which MCPSeparator counts as a
	// split point. It is 1, not 0: on the degenerate `mcp____tool` the
	// separator sits at offset 0 and is NOT a split point, so the name
	// resolves to server="__tool", tool="". That is the historical Go
	// behaviour (the `i > 0` guard) and, per this constant, now also
	// the TypeScript behaviour — the one divergence WP-T1 recorded in
	// TestMCPIdentityMatchesTheDashboardParser.
	MCPSeparatorMinIndex = 1
	// MCPTargetSeparator is the second form accepted on display-
	// normalised TARGETS only: `<server>:<tool>`, emitted by the codex
	// adapter (151 corpus rows). See MCPIdentityFromTarget for why the
	// third historical form, `<server>/<tool>`, is dropped.
	MCPTargetSeparator = ":"
)

// MCPIdentity parses the canonical MCP tool-name form
// `mcp__<server>__<tool>` and is the SINGLE Go owner of that parse
// (plan §1). models.IsMCPToolName delegates to it, and so — via
// MCPIdentityFromTarget — do policy.MCPServerFromTarget and
// policy.MCPToolFromTarget.
//
// ok reports membership in the `mcp__` namespace, NOT that both halves
// were present: a degenerate `mcp__server` (no second separator) yields
// server="server", tool="", ok=true, and a bare `mcp__` yields
// ("", "", true). That is deliberate — it reproduces
// models.IsMCPToolName's prefix test byte-for-byte, and matches what
// policy.MCPServerFromTarget and web/src/lib/actions.ts::mcpIdentity
// already do with the same degenerate inputs.
//
// MCPIdentity does NOT trim whitespace: it is applied to raw tool names
// straight off the wire, where a leading space is a different name.
// MCPIdentityFromTarget trims, because targets are display-normalised.
func MCPIdentity(raw string) (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(raw, MCPPrefix)
	if !found {
		return "", "", false
	}
	if i := strings.Index(rest, MCPSeparator); i >= MCPSeparatorMinIndex {
		return rest[:i], rest[i+len(MCPSeparator):], true
	}
	return rest, "", true
}

// IsMCPToolName reports whether a raw tool name is in the `mcp__`
// namespace. Exactly equivalent to models.IsMCPToolName, which
// delegates here.
func IsMCPToolName(raw string) bool {
	_, _, ok := MCPIdentity(raw)
	return ok
}

// MCPIdentityFromTarget parses an MCP identity out of a normalised
// action TARGET (or any display-normalised name). It accepts the two
// forms the corpus actually contains:
//
//   - `mcp__<server>__<tool>` — the canonical namespace form: 2,761
//     raw_tool_name rows + 69 target rows in the 321,675-row corpus
//     measured 2026-07-31;
//   - `<server>:<tool>` — the colon form emitted by the codex adapter
//     into actions.target: 151 rows (node_repl:js ×148, codex:
//     list_mcp_resources, node_repl:js_add_node_module_dir,
//     observer:get_session_summary).
//
// The third historical form, `<server>/<tool>` (parsed only by the Go
// side, policy/rules_taint.go:213), is DROPPED: it has ZERO rows in the
// corpus — 0 in actions.target for action_type='mcp_call' and 0 in
// actions.raw_tool_name across every action type. Keeping it was a net
// negative: `/` is the path separator, so a path-shaped MCP target
// (cowork emits bare targets like "deep-research"; other adapters emit
// file paths) would mis-parse its first path segment as a server name
// and fabricate a taint origin that never existed.
//
// A bare name with no separator yields ok=false, matching the
// documented "unknown server → the rule cannot evaluate, never a hit"
// behaviour of the guard layer.
func MCPIdentityFromTarget(target string) (server, tool string, ok bool) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", false
	}
	if s, tl, found := MCPIdentity(t); found {
		return s, tl, true
	}
	if i := strings.Index(t, MCPTargetSeparator); i > 0 {
		if i+1 < len(t) {
			return t[:i], t[i+1:], true
		}
		return t[:i], "", true
	}
	return "", "", false
}
