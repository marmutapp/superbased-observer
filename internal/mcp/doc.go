// Package mcp implements the Model Context Protocol server that exposes
// project-knowledge tools to AI coding assistants over stdio transport.
//
// The transport is newline-delimited JSON-RPC 2.0 per the MCP spec (stdio
// stream). Each message fits on one line; Content-Length framing is only
// for HTTP-SSE transports.
//
// See spec §11 for the tool list and response shape. Phase 3 starter tools:
//   - check_file_freshness
//   - get_file_history
//   - get_session_summary
//   - search_past_outputs
//
// The server owns stdin/stdout of the hosting process — `observer serve`
// is the expected launch mode, spawned per-AI-tool by the registration
// written by `observer init`.
package mcp
