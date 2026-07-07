-- 032_token_usage_turn_id.sql — codex per-event token rows.
--
-- Background: pre-v1.7.24 the codex adapter set
-- token_usage.message_id = ctxState.TurnID, so all per-inference
-- token_count events of one user-turn collapsed onto the same
-- message_id. The dashboard's transcript view groups by
-- COALESCE(message_id, source_event_id), producing one displayed
-- "message" per user-turn — useful for the turn-rollup view, but
-- losing the per-inference detail.
--
-- v1.7.24+ flips this for codex: token_usage.message_id is set to
-- the per-event identifier (the same string as source_event_id),
-- giving one row per codex token_count event. The turn association
-- is preserved in the new turn_id column added by this migration,
-- so the dashboard can group by `COALESCE(turn_id, message_id,
-- source_event_id)` to recover the rollup view AND offer a per-
-- inference detail view that groups by message_id.
--
-- Backwards-compat: existing codex rows have turn_id NULL.
-- COALESCE(turn_id, message_id, source_event_id) on those rows
-- falls back to message_id (= the old turnID), so old rows still
-- group correctly without a data backfill. Claudecode and other
-- adapters do not populate turn_id; their per-API-call message_id
-- semantic is already correct.

ALTER TABLE token_usage ADD COLUMN turn_id TEXT;

CREATE INDEX IF NOT EXISTS idx_token_usage_session_turn
    ON token_usage(session_id, turn_id);
