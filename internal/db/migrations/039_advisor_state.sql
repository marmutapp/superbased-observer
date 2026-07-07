-- 039: advisor (suggestions engine, spec §15.7) user state + digest snapshot.
-- BOTH tables are NODE-LOCAL: never pushed by orgpush.SelectUnpushedSince
-- (pinned by tests/invariant/privacy_test.go — suggestion state and the
-- digest snapshot embed file paths / command summaries).

-- advisor_state holds per-suggestion user decisions keyed by the stable
-- dedup_key. status: 'dismissed' | 'snoozed' | 'acted'. snoozed_until is
-- set only for status='snoozed'; a dismissed key stays quiet for the
-- engine's cooldown window, then may resurface if still detected.
CREATE TABLE IF NOT EXISTS advisor_state (
    dedup_key     TEXT PRIMARY KEY,
    status        TEXT NOT NULL,
    snoozed_until TEXT,
    updated_at    TEXT NOT NULL
);

-- advisor_digest is a one-row JSON snapshot of the current top suggestions,
-- refreshed by the daemon on a slow ticker. It exists so latency-bound
-- consumers (the session-start hook, the MCP get_suggestions tool) do a
-- single point-read instead of running the engine (plan §Phase-3:
-- never compute in-hook).
CREATE TABLE IF NOT EXISTS advisor_digest (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    generated_at TEXT NOT NULL,
    payload      TEXT NOT NULL
);
