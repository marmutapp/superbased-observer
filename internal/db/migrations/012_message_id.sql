-- 012_message_id.sql
--
-- Add the upstream Anthropic message.id (msg_xxx) as a first-class
-- column on both `actions` and `token_usage`. Pre-fix this id was
-- only available indirectly:
--
--   * api_turns.request_id stores it (proxy path).
--   * token_usage.source_event_id stored EITHER msg.id OR per-line
--     uuid, depending on which version of the claudecode adapter
--     ingested the row — historical rows used line.uuid, newer rows
--     use msg.id. That ambiguity blocked clean per-message rollups.
--   * actions had no exposure of it at all.
--
-- Adding an explicit column on both tables lets the dashboard group
-- token usage and tool calls by upstream message — the natural unit
-- of an Anthropic API turn — and surface the id in the per-message
-- timeline + actions table.
--
-- Pre-existing rows have NULL until `observer backfill --message-id`
-- walks the JSONL and populates them. The dashboard handles NULL
-- gracefully by falling back to source_event_id for grouping.

ALTER TABLE actions     ADD COLUMN message_id TEXT;
ALTER TABLE token_usage ADD COLUMN message_id TEXT;

CREATE INDEX IF NOT EXISTS idx_actions_session_message     ON actions(session_id, message_id);
CREATE INDEX IF NOT EXISTS idx_token_usage_session_message ON token_usage(session_id, message_id);
