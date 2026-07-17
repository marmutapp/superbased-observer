-- Org-tier observability T1 — aggregate rollup (obs-org-tier plan §5.1).
--
-- obs_summaries receives the content-free per-(day, model, provider,
-- project_hash, source) counts + token/cost/latency sums the agent pushes
-- under its node-side [org_client.share] obs_summary opt-in. No trace ids, no
-- span topology, no bodies — the analytics floor. Server-only (no agent
-- migration: the obs_* tables are obs-owned + node-local, and only this
-- aggregate crosses the wire, via the obs provider seam). Upsert by natural
-- key so a re-pushed window is idempotent.
CREATE TABLE IF NOT EXISTS obs_summaries (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id             TEXT NOT NULL,
    user_email         TEXT NOT NULL DEFAULT '',
    day                TEXT NOT NULL,
    model              TEXT NOT NULL DEFAULT '',
    provider           TEXT NOT NULL DEFAULT '',
    project_hash       TEXT NOT NULL DEFAULT '',
    source             TEXT NOT NULL DEFAULT '',
    traces             INTEGER NOT NULL DEFAULT 0,
    spans              INTEGER NOT NULL DEFAULT 0,
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens       INTEGER NOT NULL DEFAULT 0,
    cost_usd           REAL NOT NULL DEFAULT 0,
    error_traces       INTEGER NOT NULL DEFAULT 0,
    duration_ms_sum    INTEGER NOT NULL DEFAULT 0,
    duration_ms_count  INTEGER NOT NULL DEFAULT 0,
    pushed_at          TEXT NOT NULL,
    pushed_by_user_id  TEXT NOT NULL,
    UNIQUE (org_id, user_email, day, model, provider, project_hash, source)
);
CREATE INDEX IF NOT EXISTS idx_obs_summaries_org_day ON obs_summaries(org_id, day);
