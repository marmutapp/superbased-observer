-- 049_limit_snapshots.sql — rate-limit / subscription-window snapshots for
-- the Next-Message Cost & Limit Predictor (the limit half).
--
-- The provider's 5-hour / weekly subscription-window utilization + reset
-- timestamps live ONLY in upstream HTTP response headers, which the proxy
-- is the only component positioned to read. One row per (scope, provider)
-- observation, written by store.InsertLimitSnapshot from the proxy graft.
--
-- NODE-LOCAL — account-personal telemetry that MUST NOT leave this
-- machine. Pinned in tests/invariant/privacy_test.go (forbidden-table
-- sentinel) and excluded from internal/store/orgpush.go by construction
-- (orgpush selects an explicit table allow-list; this table is never in
-- it). Same posture as the cachetrack tables.

CREATE TABLE IF NOT EXISTS limit_snapshots (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    scope_hash      TEXT NOT NULL,   -- auth-identity hash (R4); 'default' fallback
    provider        TEXT NOT NULL,   -- anthropic | openai
    session_id      TEXT,            -- best-effort link to the observing session
    observed_at     INTEGER NOT NULL, -- unix seconds
    window_5h_util  REAL,            -- 0..1; NULL when the header was absent
    window_5h_reset INTEGER,         -- unix seconds; NULL when absent
    window_7d_util  REAL,
    window_7d_reset INTEGER,
    req_limit       INTEGER,
    req_remaining   INTEGER,
    req_reset       INTEGER,
    tok_limit       INTEGER,
    tok_remaining   INTEGER,
    tok_reset       INTEGER,
    status          TEXT,            -- unified-status passthrough
    raw             TEXT             -- allow-listed, scrubbed header subset; nullable
);

CREATE INDEX IF NOT EXISTS idx_limit_snapshots_scope
    ON limit_snapshots(scope_hash, provider, observed_at DESC);
