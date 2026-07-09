// Package heuristic is the regex + brace/indent fallback parse backend
// for languages that have no precise (go/ast, tree-sitter) backend yet.
// It line-scans source against a per-language rule table and emits
// APPROXIMATE symbol spans, imports, and call sites.
//
// Its spans are advisory only: it ALWAYS reports ExactSpans=false, so the
// compressor's collapse gate (ADR-0005) treats every heuristic node as
// non-collapsible. End lines are best-effort (balanced-brace scan for
// brace languages, dedent scan for indent languages) and may be zero when
// the engine cannot determine them confidently.
//
// The package is PURE (CLAUDE.md anti-spaghetti rule 1): it imports only
// the standard library plus the codeintel root and parse packages. All
// regexes compile once at package init; Parse is deterministic and never
// panics on any input.
package heuristic
