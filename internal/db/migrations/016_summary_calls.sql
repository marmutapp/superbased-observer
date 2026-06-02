-- 016_summary_calls.sql
--
-- D20 (Tier 2 / v1.4.43+) cost-net surface for rolling summarisation.
--
-- Pre-fix: messagesummary.AnthropicSummarizer calls api.anthropic.com
-- DIRECTLY (not through our proxy) so api_turns never sees the Haiku
-- spend. The user could see "rolling_summary mechanism saved X bytes"
-- but had no way to see "...and cost Y in Haiku tokens to produce
-- those summaries".
--
-- Post-fix: every successful AnthropicSummarizer.Summarize call
-- records one row here. The Compression-tab cost-net card joins
-- this against `compression_events.mechanism = 'rolling_summary'` to
-- compute (cache_creation_savings - haiku_spend) — the load-bearing
-- "is rolling-summ paying its way?" metric.
--
-- session_id may be empty for batched/cross-session callers; model
-- and cost_usd are populated at insert time using the cost.Engine's
-- pricing table. Index on (timestamp, session_id) so the dashboard's
-- per-window aggregate stays fast even with many sessions.
CREATE TABLE IF NOT EXISTS summary_calls (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id              TEXT,
    timestamp               TEXT NOT NULL,
    model                   TEXT NOT NULL,
    input_tokens            INTEGER NOT NULL DEFAULT 0,
    output_tokens           INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd                REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_summary_calls_timestamp ON summary_calls(timestamp);
CREATE INDEX IF NOT EXISTS idx_summary_calls_session   ON summary_calls(session_id) WHERE session_id IS NOT NULL;
