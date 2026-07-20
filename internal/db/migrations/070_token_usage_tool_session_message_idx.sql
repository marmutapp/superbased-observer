-- 070_token_usage_tool_session_message_idx.sql — covering index for the
-- post-insert dedup sweeps in store.InsertTokenEvents.
--
-- The tuple-dedup and snapshot-drift sweeps DELETE from token_usage with an
-- outer filter leading on `tool` (claude-code/codex) plus an EXISTS subquery
-- correlated on (tool, session_id, message_id). No pre-existing index leads
-- with `tool`, so the outer DELETE full-SCANs token_usage every time a
-- claude-code/codex batch lands. This composite index turns that SCAN into a
-- SEARCH ... (tool=?) and also covers the EXISTS correlation columns.
--
-- Node-local only: no wire-shape change, no server-side migration pair.
CREATE INDEX IF NOT EXISTS idx_token_usage_tool_session_message
    ON token_usage(tool, session_id, message_id);
