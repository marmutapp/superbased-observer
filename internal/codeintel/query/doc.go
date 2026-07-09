// Package query implements a read-only Cypher-subset engine
// (lexer -> parser -> planner -> executor) over the codeintel graph. It
// is PURE — no database/sql, net/http, or fsnotify; the planner emits a
// plan the store seam executes, or operates over an in-memory graph
// view. The supported subset (and the explicitly unsupported parts) is
// documented in docs/codeintel/query-cypher.md.
package query
