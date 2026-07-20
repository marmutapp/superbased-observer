// Package parse turns source bytes into a ParseResult: symbols (with
// spans), imports, and call sites. It is PURE — no database/sql,
// net/http, or fsnotify; backends are registered into a Registry keyed
// by [Language]. The wasm tree-sitter host, the go/ast backend, and the
// heuristic fallback all satisfy the same Parser interface, so adding a
// language is data (a grammar + queries + one registry row), not core
// code. See docs/codeintel/parsing.md.
package parse
