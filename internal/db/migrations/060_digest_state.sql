-- 060_digest_state.sql — scheduled report digest (gap-register G13) send-once
-- de-dup marker. NODE-LOCAL bookkeeping only: it records the Period.Key of the
-- most-recently-sent personal cost digest so a daemon restart (or an extra tick
-- within the same period) never re-sends. It carries no captured content (only
-- a period label + a timestamp), so it is never pushed by
-- orgpush.SelectUnpushedSince. Keyed by `kind` for future digest variants;
-- today there is one row, kind='node'.
CREATE TABLE IF NOT EXISTS digest_state (
    kind        TEXT PRIMARY KEY,
    last_period TEXT NOT NULL,
    sent_at     TEXT NOT NULL
);
