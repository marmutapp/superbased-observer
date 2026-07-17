-- 037 — cache_events.message_id for cross-tier dedup
--
-- Tier-1 (proxy) cache_events rows joined to api_turns.request_id
-- for dedup (CacheEventExistsForMessage). Tier-2 (transcript)
-- rows have NULL api_turn_id and need a direct message_id column
-- to support the spec §12 idempotent backfill — without it,
-- `observer backfill --cache-rescan` re-emits every event on
-- every run (the dedup gate sees tier='proxy' only).
--
-- Backfilled rows: pre-migration cache_events rows leave
-- message_id NULL. They're all tier='proxy' (Tier-2 path didn't
-- write anything before this migration); the existing
-- api_turns.request_id join still covers them, so dedup stays
-- correct end-to-end without a backfill pass.
ALTER TABLE cache_events ADD COLUMN message_id TEXT;

-- Partial index on the (session_id, message_id) lookup pair —
-- NULL message_id (pre-migration rows) is excluded so the index
-- stays small. CacheEventExistsForMessage's Tier-2 path hits
-- this directly: WHERE session_id = ? AND message_id = ?.
CREATE INDEX idx_cache_events_msgid
  ON cache_events(session_id, message_id)
  WHERE message_id IS NOT NULL;
