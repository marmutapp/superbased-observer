// Package codegraph downloads and reads the codebase-memory-mcp SQLite
// graph database.
//
// codebase-memory-mcp (github.com/DeusData/codebase-memory-mcp) is an
// MIT-licensed tool that ships a tree-sitter parser supporting 66
// languages and emits a per-project graph DB with nodes (symbol
// definitions: functions, classes, modules) and edges (CALLS, IMPORTS,
// CONTAINS, SIMILAR_TO, SEMANTICALLY_RELATED). We read it in read-only
// mode; we do not fork or rewrite it.
//
// Install downloads the latest release from GitHub, verifies the SHA-256
// checksum against the release's checksums.txt, and extracts the
// platform binary to ~/.local/bin/. FindProjectDB locates the per-project
// graph.db under ~/.local/share/codebase-memory-mcp/. Available() returns
// false until a graph DB exists for the project; query methods return
// empty results in that state.
//
// Schema (confirmed against upstream v0.6.0):
//
//	nodes: id, kind, name, file_path, start_line, end_line, fqn, language
//	edges: id, source_id, target_id, kind
//
// See spec §12.
package codegraph
