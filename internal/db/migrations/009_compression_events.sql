-- 009_compression_events.sql — per-event detail for the conversation
-- compression pipeline.
--
-- Until now the api_turns row carried only aggregate counters
-- (compression_count, compression_dropped_count, compression_marker_count)
-- plus original/compressed bytes. That hides which mechanism actually
-- saved the bytes — was it a JSON tool_result rewrite, a code-block
-- compactor, a low-importance message drop? The events table records one
-- row per compress-or-drop decision so the dashboard can break down
-- savings by mechanism over time.
--
-- mechanism is one of:
--   'json' | 'code' | 'logs' | 'text' | 'diff' | 'html'   — per-type
--                                                           compressor ran
--   'drop'                                                 — message dropped
--                                                           and replaced by
--                                                           a marker
--
-- importance_score is set only for 'drop' rows — the score the budget
-- enforcer used to pick the message for elimination. NULL for compress
-- mechanisms.

CREATE TABLE IF NOT EXISTS compression_events (
    id                INTEGER PRIMARY KEY,
    api_turn_id       INTEGER NOT NULL REFERENCES api_turns(id) ON DELETE CASCADE,
    timestamp         TEXT    NOT NULL,
    mechanism         TEXT    NOT NULL,
    original_bytes    INTEGER NOT NULL,
    compressed_bytes  INTEGER NOT NULL,
    msg_index         INTEGER,
    importance_score  REAL
);

CREATE INDEX IF NOT EXISTS idx_compression_events_ts        ON compression_events(timestamp);
CREATE INDEX IF NOT EXISTS idx_compression_events_turn      ON compression_events(api_turn_id);
CREATE INDEX IF NOT EXISTS idx_compression_events_mechanism ON compression_events(mechanism);
