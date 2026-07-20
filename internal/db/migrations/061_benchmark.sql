-- 061_benchmark.sql — the Benchmarks Harness node-local schema
-- (docs/plans/benchmarks-harness-plan-2026-07-11.md §3.3).
--
-- Four tables model one active benchmark run: a fixed task corpus driven
-- across a {harness × model} matrix through the proxy-routing launcher verbs,
-- each attempt correlated to its produced session(s) so cost/tokens/cache are
-- derived at report time from api_turns/token_usage — NOT denormalized here
-- (one owner of cost is the cost engine).
--
-- The attempt is separated from its 0..N session memberships so pre-session
-- failures (setup_error — no session) and one-to-many sub-agent sessions are
-- both representable.
--
-- NODE-LOCAL — these tables hold repo paths, prompts, and judge rationales.
-- They MUST NOT leave this machine: pinned in tests/invariant/privacy_test.go
-- (forbidden-table sentinel) and excluded from internal/store/orgpush.go by
-- construction (orgpush names an explicit table allow-list; these are never in
-- it). No paired orgserver migration exists, by design. Same posture as the
-- cachetrack / limit_snapshots tables.

CREATE TABLE IF NOT EXISTS benchmark_runs (
    run_id                TEXT PRIMARY KEY,
    spec_name             TEXT NOT NULL,
    spec_hash             TEXT NOT NULL,       -- content hash of the spec (intent pin)
    spec_json             TEXT NOT NULL,       -- full spec snapshot (canonical)
    manifest_json         TEXT,                -- reproducibility manifest (§3.8)
    pricing_snapshot_json TEXT,                -- pricing-table hash + rates at completion (§3.11)
    started_at            TEXT NOT NULL,
    finished_at           TEXT,
    status                TEXT NOT NULL,       -- running|completed|budget_stop|aborted|error
    planned_cells         INTEGER NOT NULL DEFAULT 0,
    completed_cells       INTEGER NOT NULL DEFAULT 0,
    spend_usd             REAL NOT NULL DEFAULT 0,
    judge_spend_usd       REAL NOT NULL DEFAULT 0,
    budget_json           TEXT,
    notes                 TEXT
);

CREATE INDEX IF NOT EXISTS idx_benchmark_runs_started ON benchmark_runs(started_at DESC);

-- one row per (task × config × repeat)
CREATE TABLE IF NOT EXISTS benchmark_attempts (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id               TEXT NOT NULL REFERENCES benchmark_runs(run_id),
    task_id              TEXT NOT NULL,
    config_id            TEXT NOT NULL,
    harness              TEXT NOT NULL,
    model_requested      TEXT NOT NULL,        -- what the spec asked for
    repeat_idx           INTEGER NOT NULL,
    workspace_path       TEXT,                 -- ephemeral; retention-swept (§3.12)
    wall_ms              INTEGER NOT NULL DEFAULT 0,
    exit_code            INTEGER,
    status               TEXT NOT NULL,        -- ok|model_fail|setup_error|harness_error|proxy_error|timeout|budget_stop|scorer_unavailable|orphaned
    final_answer_excerpt TEXT,                 -- size-capped, ANSI-stripped, scrubbed (§3.4)
    spend_usd            REAL NOT NULL DEFAULT 0,
    turns                INTEGER NOT NULL DEFAULT 0,
    error_class          TEXT,                 -- machine-readable failure reason
    started_at           TEXT NOT NULL,
    finished_at          TEXT,
    UNIQUE(run_id, task_id, config_id, repeat_idx)
);

CREATE INDEX IF NOT EXISTS idx_benchmark_attempts_run ON benchmark_attempts(run_id);

-- attempt → 0..N sessions (a harness may spawn sub-agent sessions)
CREATE TABLE IF NOT EXISTS benchmark_session_members (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id     INTEGER NOT NULL REFERENCES benchmark_attempts(id),
    run_id         TEXT NOT NULL REFERENCES benchmark_runs(run_id),
    session_id     TEXT NOT NULL,              -- soft FK to sessions(id)
    role           TEXT NOT NULL,              -- primary | subagent | judge
    model_returned TEXT,                       -- actual served model id, when captured
    UNIQUE(attempt_id, session_id)
);

CREATE INDEX IF NOT EXISTS idx_benchmark_members_run ON benchmark_session_members(run_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_members_session ON benchmark_session_members(session_id);

-- one row per (attempt × scorer)
CREATE TABLE IF NOT EXISTS benchmark_scores (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id  INTEGER NOT NULL REFERENCES benchmark_attempts(id),
    run_id      TEXT NOT NULL REFERENCES benchmark_runs(run_id),
    scorer      TEXT NOT NULL,
    score       REAL NOT NULL DEFAULT 0,
    passed      INTEGER NOT NULL DEFAULT 0,
    rationale   TEXT,
    judge_model TEXT,
    rubric_hash TEXT,
    degraded    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_benchmark_scores_attempt ON benchmark_scores(attempt_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_scores_run ON benchmark_scores(run_id);
