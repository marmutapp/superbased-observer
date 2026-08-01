// Package tooltax is the canonical, cross-adapter tool/MCP taxonomy —
// the single owner of "what does this native tool name mean" and "what
// is this MCP call's identity".
//
// Motivation (docs/plans/tool-taxonomy-standardization-plan-2026-07-31.md
// §0): every adapter preserves the native tool name in
// actions.raw_tool_name, but the NORMALIZATION of those names is
// fragmented across ~15 package-private maps and switches with no
// cross-adapter view, no shared type, and three mutually-inconsistent
// MCP-detection conventions. tooltax replaces that with one ordered
// data table plus one MCP identity parser.
//
// # Purity
//
// tooltax is the strictest pure-logic package in the repo: it imports
// NOTHING project-internal (not even internal/models) and no SQL/HTTP/
// fsnotify. That is enforced by imports_test.go. The consequence is
// deliberate — internal/models may import tooltax (so models.IsMCPToolName
// can delegate here) without an import cycle, and so may internal/policy,
// internal/adapter/*, internal/store and the dashboard.
//
// Because tooltax cannot import internal/models, the canonical action
// types are re-declared here as untyped string constants whose VALUES
// are identical to the models.Action* constants. A conformance test in
// the external test package (tooltax_test) pins that equality, so a
// divergence is loud.
//
// # The table
//
// table is ORDERED and walked top-down (CLAUDE.md rule 5): specific
// (tool, native) rows first, then tool-less fallback rows, then glob
// rows (mcp__*) last. Resolve does an exact pass followed by a
// normalized pass (lower-case, `_`/`-`/`.`/space stripped) so the
// adapters that normalize before switching (antigravity, gemini, grok,
// copilot, command-code, qwen-code, kimi-code) resolve the same names
// their private switches do.
//
// # Work-package status
//
// This is WP-T1 of the plan: the package, the table, the imports pin,
// the per-row tests and the MCP identity owner. Out of scope here and
// tracked separately:
//
//   - WP-T2 (DONE) emits web/src/lib/actiontax.gen.json from the
//     action-type registry, the category list and the MCP constants —
//     see web/taxgen, gated by `make verify-taxonomy-build`. The (tool,
//     native) table itself is deliberately not emitted: the browser
//     never resolves a raw native name.
//   - WP-T3 converts the adapters' private maps into rows here; until
//     then conformance tests pin each private map as a SUBSET of this
//     table so drift is loud without a big-bang rewrite.
//   - WP-T4 adds the new action types below as models.Action* constants
//     and backfills the historical `unknown` rows they explain.
//   - WP-T5 surfaces Category/Surface on /api/tools/breakdown.
package tooltax
