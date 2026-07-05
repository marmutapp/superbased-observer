// Package resolve turns ParseResult(s) into edges: name-matched CALLS
// and IMPORTS. It is PURE — no database/sql, net/http, or fsnotify.
// Resolution rules are table-driven and capability-gated (no branching
// on language identity). Type-resolved (Hybrid-LSP) edges are a future
// additive backend, not built here (Tier D). See
// docs/codeintel/resolution.md.
package resolve
