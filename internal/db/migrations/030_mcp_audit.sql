-- 030_mcp_audit.sql
--
-- V7-14: per-call audit log for the V7-12 retrieval-surface MCP tools.
-- v1.7.8 wires the first tool (get_file); follow-up PRs will append
-- get_symbols, get_relations, and the retrieve_stashed batch+slice
-- extension into the same table.
--
-- One row per MCP call — success or denial — so operators can answer
-- "why was X denied?" or "what files is the agent reading?" via plain
-- SQL until the operator CLI lands (deferred follow-up).
--
-- Co-located with the main observer DB rather than the separate
-- ~/.observer/mcp_audit.db originally proposed in
-- docs/v4-codex-compression-recipe-and-issues.md V7-14. Volume budget
-- (one row per MCP call) sits comfortably inside the main DB and one
-- less file / GC goroutine / open handle is worth the co-location.
-- If volume becomes a real concern the table can be moved later
-- without changing the writer interface.
--
-- Retention: unbounded at the table level for v1.7.8. At realistic
-- MCP call rates (~1k calls/day per heavy user) this is ~365k rows/yr
-- which SQLite shrugs at. Manual purge via
-- `DELETE FROM mcp_audit WHERE ts < '...'`; a CLI lands in v1.8.x.
--
-- Indexing strategy mirrors the V7-14 design proposal:
--   * ts                                 → time-window queries
--   * (tool_name, ts)                    → per-tool roll-ups
--   * (session_id, ts)                   → per-session forensics
--   * (path_requested, ts)               → top-paths capacity planning
--   * (response_ok, reason, ts) PARTIAL  → "what's being denied?" fast
--
-- response_ok=1 rows dominate volume, so the denied index is partial
-- (WHERE response_ok = 0) to stay small and fast.

CREATE TABLE IF NOT EXISTS mcp_audit (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                  TEXT    NOT NULL,            -- RFC3339Nano UTC
    session_id          TEXT,                        -- nullable
    tool_name           TEXT    NOT NULL,            -- 'get_file' today
    request_hash        TEXT    NOT NULL,            -- sha256 of normalized request
    path_requested      TEXT,                        -- nullable for non-path tools
    response_size_bytes INTEGER NOT NULL DEFAULT 0,
    response_truncated  INTEGER NOT NULL DEFAULT 0,  -- 0|1
    response_ok         INTEGER NOT NULL,            -- 0|1
    reason              TEXT,                        -- denial reason; null on success
    duration_us         INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_mcp_audit_ts
    ON mcp_audit(ts);
CREATE INDEX IF NOT EXISTS idx_mcp_audit_tool
    ON mcp_audit(tool_name, ts);
CREATE INDEX IF NOT EXISTS idx_mcp_audit_session
    ON mcp_audit(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_mcp_audit_path
    ON mcp_audit(path_requested, ts);
CREATE INDEX IF NOT EXISTS idx_mcp_audit_denied
    ON mcp_audit(response_ok, reason, ts) WHERE response_ok = 0;
