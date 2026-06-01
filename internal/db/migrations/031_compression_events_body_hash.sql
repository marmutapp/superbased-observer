-- 031_compression_events_body_hash.sql — V7-9 dedup accounting.
--
-- Pre-v1.7.12, compression_events rows captured one decision per
-- compress-or-drop call, which means a tool_result body that survives
-- across N turns generates N rows — each row identical except for
-- api_turn_id and timestamp. Aggregate gross counters
-- (SUM(original_bytes), COUNT(*) by mechanism) stay correct, but
-- "how many unique tool_result bodies actually hit the compressor"
-- requires deduplication on body content. Without it, the
-- "comp_events / N turns" ratio overstates per-turn work by the
-- average turn-span of a tool_result (often 30-50× on long sessions).
--
-- body_hash is the sha256-hex of the pre-compression body bytes.
-- Populated at event creation time by the conversation pipeline
-- (v1.7.12+). Pre-migration rows have NULL — the dedup query below
-- excludes them so existing aggregations stay byte-identical.
--
-- Reader: store.CountUniqueCompressions(ctx, sessionID) returns
-- COUNT(DISTINCT body_hash) within a session, scoped through
-- api_turn_id → api_turns.session_id.

ALTER TABLE compression_events ADD COLUMN body_hash TEXT;

-- Partial index on the populated rows. (session_id, body_hash) is
-- the hot dedup-rollup shape — joining through api_turns means we
-- index api_turn_id + body_hash and let the query planner walk
-- api_turns.id for session_id. WHERE body_hash IS NOT NULL keeps
-- the index small until backfill is complete (and forever, since
-- pre-v1.7.12 rows never get populated).
CREATE INDEX IF NOT EXISTS idx_compression_events_turn_body_hash
    ON compression_events(api_turn_id, body_hash)
    WHERE body_hash IS NOT NULL;
