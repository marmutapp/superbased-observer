// Package audit records one row per V7-12 MCP tool invocation
// (success or denial) into the mcp_audit table. Provides operators a
// forensic trail for "why was X denied?" and "what files is the agent
// reading?" without per-call log spam.
//
// Two writer implementations:
//
//   - [NewSQLWriter] flushes Rows into mcp_audit asynchronously through
//     a buffered channel + background goroutine. The MCP hot path never
//     blocks on the DB; oldest-row-dropped overflow logs at most once
//     per minute.
//   - [NewNoopWriter] is a no-op stub for [intelligence.mcp.audit]
//     enabled = false and for tests that don't care about audit.
//
// All Writer methods are safe for concurrent use.
//
// See docs/plans/v1.7.8-mcp-get-file-plan-2026-05-30.md §5 for the schema
// design and rationale, and docs/v4-codex-compression-recipe-and-
// issues.md V7-14 for the original design proposal.
package audit
