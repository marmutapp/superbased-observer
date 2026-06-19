-- 014_retrieval_signals.sql
--
-- K43 self-learning feedback loop (Tier 3): track which compressed
-- content the model later searches for or retrieves. Every successful
-- retrieve_stashed / non-empty search_past_outputs hit is one signal
-- that the underlying content was load-bearing — the more signals a
-- pattern accumulates, the more aggressive the compression layer can
-- be in stashing it (vs. inlining it).
--
-- Two signal_type values today:
--   * "retrieve_stashed" — the model called retrieve_stashed({sha}).
--     payload carries the sha so the learn extension can correlate
--     signals back to the producing tool / format. action_id is NULL
--     because stashed bytes don't carry an action_id reference.
--   * "search_hit"        — search_past_outputs returned this row.
--     action_id is the FTS5 hit's row id; payload carries the search
--     query so high-retrieval-rate query patterns can be surfaced.
--
-- Indexing strategy: action_id (so per-action retrieval rate joins are
-- O(N) on the signal count, not O(N×actions)), signal_type (for
-- aggregate queries like "what fraction of stashes were retrieved"),
-- signal_at (for time-windowed dogfood reports). No ON DELETE CASCADE
-- — orphan rows after action deletion are harmless and we'd rather
-- preserve the long tail for the K43 pattern miner.
CREATE TABLE IF NOT EXISTS retrieval_signals (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id   INTEGER REFERENCES actions(id),
    signal_type TEXT NOT NULL,
    signal_at   TEXT NOT NULL,
    session_id  TEXT,
    payload     TEXT
);
CREATE INDEX IF NOT EXISTS idx_retrieval_signals_action ON retrieval_signals(action_id);
CREATE INDEX IF NOT EXISTS idx_retrieval_signals_type   ON retrieval_signals(signal_type);
CREATE INDEX IF NOT EXISTS idx_retrieval_signals_at     ON retrieval_signals(signal_at);
