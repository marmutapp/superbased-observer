// Package livesym is a tiny regex-based symbol parser used as a
// graceful-degradation fallback for the V7-12 retrieval surface
// when codebase-memory-mcp's tree-sitter index is unavailable or
// stale.
//
// V7-17 items 2 + 4 (notes-file polish, v1.7.16+): when
// codegraph.Client.Available() is false OR Stale() is true, the
// get_symbols / get_relations tools call Parse(path) to recover
// approximate symbol positions from the live file. The matches
// carry honest "I don't know" markers (EndLine = -1, no body)
// alongside the existing degraded flag, so the agent can
// distinguish "codegraph said exactly" from "regex's best guess."
//
// Supported languages: Go, TypeScript/TSX, JavaScript/JSX, Python.
// Other extensions return langUnsupported=true; the MCP layer
// surfaces this as the regex_fallback_language_unsupported warning.
//
// End-line inference is intentionally NOT implemented — regex-based
// end inference (brace counting / indentation tracking) is fragile
// across multi-line strings, nested struct literals, and decorators.
// EndLine = -1 is the honest answer until a future PR adds
// tree-sitter parsing for accurate spans.
package livesym
