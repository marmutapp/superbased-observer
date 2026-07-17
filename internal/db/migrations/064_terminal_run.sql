-- 064_terminal_run.sql — the terminal-run identity + correlation model
-- (docs/plans/terminal-product-exploitation-plan-2026-07-12.md §2.1a / §7,
-- Phase 0 item S0b).
--
-- An ephemeral PTY handle is NOT an Observer session id: a fresh (non-handoff)
-- launch has no session id until the target tool creates one AFTER startup,
-- and hooks/transcript/proxy turns then arrive asynchronously and may disagree.
-- These two NODE-LOCAL tables give every launch a durable identity and a set of
-- zero-or-more confidence-scored correlations to observed agent sessions, so
-- later features (cost, status, decorations, actions) join to a run, never to a
-- raw handle→session guess.
--
--   terminal_run          — one row per launch. The durable identity.
--   terminal_run_session  — zero-or-more scored correlations from a run to an
--                           observed agent session, established over time.
--
-- IDENTITY INVARIANT (plan §2.1a): the source handoff session
-- (`source_session_id`, present only for kind='handoff') and any target session
-- the run spawns (rows in terminal_run_session) are DISTINCT and must never be
-- conflated. The source is the session we continued FROM; the correlated
-- sessions are the ones the launch produced.
--
-- CONTENT DISCIPLINE (CLAUDE.md): metadata / coordinates / hashes only. The
-- project root is stored as a domain-separated hash (`project_root_hash`),
-- never a raw path; the OOB correlation token is stored hashed
-- (`correlation_token_hash`), never in the clear. No command text or terminal
-- output is ever stored here.
--
-- NODE-LOCAL — these tables MUST NOT leave the machine: pinned in
-- tests/invariant/privacy_test.go (forbidden-table sentinel + an end-to-end
-- SelectUnpushedSince assertion) and excluded from internal/store/orgpush.go by
-- construction (orgpush names an explicit table allow-list; neither is in it).
-- No paired orgserver migration exists, by design — same posture as the
-- cachetrack / limit_snapshots / remote_audit node-local tables.

CREATE TABLE IF NOT EXISTS terminal_run (
    run_id                 TEXT PRIMARY KEY,            -- opaque, minted at launch
    tool                   TEXT NOT NULL,               -- target tool (e.g. claude-code)
    kind                   TEXT NOT NULL,               -- handoff | fresh
    source_session_id      TEXT NOT NULL DEFAULT '',    -- handoff source (kind=handoff); NEVER a correlated target
    project_root_hash      TEXT NOT NULL DEFAULT '',    -- domain-separated hash; never a raw path
    correlation_token_hash TEXT NOT NULL DEFAULT '',    -- domain-separated hash of the OOB correlation token
    launched_at            TEXT NOT NULL,               -- RFC3339 UTC
    ended_at               TEXT,                        -- RFC3339 UTC; NULL while running
    exit_code              INTEGER                      -- NULL while running
);

CREATE INDEX IF NOT EXISTS idx_terminal_run_launched ON terminal_run(launched_at);
CREATE INDEX IF NOT EXISTS idx_terminal_run_token ON terminal_run(correlation_token_hash);

CREATE TABLE IF NOT EXISTS terminal_run_session (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      TEXT NOT NULL,                          -- FK → terminal_run.run_id
    session_id  TEXT NOT NULL,                          -- the correlated agent session
    confidence  REAL NOT NULL,                          -- 0..1, scored by internal/termrun
    source      TEXT NOT NULL,                          -- oob | marker | heuristic
    observed_at TEXT NOT NULL,                          -- RFC3339 UTC
    UNIQUE(run_id, session_id)
);

CREATE INDEX IF NOT EXISTS idx_terminal_run_session_run ON terminal_run_session(run_id);
CREATE INDEX IF NOT EXISTS idx_terminal_run_session_session ON terminal_run_session(session_id);
