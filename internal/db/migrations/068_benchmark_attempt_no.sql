-- 068_benchmark_attempt_no.sql — retain benchmark infra retries as distinct
-- physical attempts (audit P0.7 / ux-integrity-fix-wave-2026-07-16 #3).
--
-- Migration 061 keyed benchmark_attempts UNIQUE(run_id, task_id, config_id,
-- repeat_idx). The runner retries infra failures (setup/harness/proxy) by
-- re-running the SAME logical cell — so the retry's INSERT collided with that
-- UNIQUE, the row was lost, and the error was only printed. This migration
-- adds a physical `attempt_no` and folds it into the uniqueness key so every
-- retry persists as its own row; report/stats then select the terminal
-- (max attempt_no) attempt per logical cell.
--
-- The UNIQUE constraint is inline (an auto-index that cannot be DROPped), so
-- the table must be rebuilt. benchmark_session_members + benchmark_scores carry
-- FKs to benchmark_attempts(id); with foreign_keys ON inside the migration
-- transaction we cannot DROP the referenced parent while children hold rows.
-- The rebuild therefore recreates the two child tables first (pointing at the
-- new parent), drops the old children, then drops + swaps the parent. All ids
-- are preserved so the soft/hard FKs keep resolving.
--
-- NODE-LOCAL — same posture as migration 061 (privacy-pinned; no orgserver
-- pair; benchmark data never leaves the machine).

-- 1. New parent with attempt_no + the widened uniqueness key.
CREATE TABLE benchmark_attempts_new (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id               TEXT NOT NULL REFERENCES benchmark_runs(run_id),
    task_id              TEXT NOT NULL,
    config_id            TEXT NOT NULL,
    harness              TEXT NOT NULL,
    model_requested      TEXT NOT NULL,
    repeat_idx           INTEGER NOT NULL,
    attempt_no           INTEGER NOT NULL DEFAULT 0,  -- physical retry index within the logical cell
    workspace_path       TEXT,
    wall_ms              INTEGER NOT NULL DEFAULT 0,
    exit_code            INTEGER,
    status               TEXT NOT NULL,
    final_answer_excerpt TEXT,
    spend_usd            REAL NOT NULL DEFAULT 0,
    turns                INTEGER NOT NULL DEFAULT 0,
    error_class          TEXT,
    started_at           TEXT NOT NULL,
    finished_at          TEXT,
    UNIQUE(run_id, task_id, config_id, repeat_idx, attempt_no)
);

INSERT INTO benchmark_attempts_new
  (id, run_id, task_id, config_id, harness, model_requested, repeat_idx, attempt_no,
   workspace_path, wall_ms, exit_code, status, final_answer_excerpt, spend_usd, turns,
   error_class, started_at, finished_at)
SELECT
  id, run_id, task_id, config_id, harness, model_requested, repeat_idx, 0,
  workspace_path, wall_ms, exit_code, status, final_answer_excerpt, spend_usd, turns,
  error_class, started_at, finished_at
FROM benchmark_attempts;

-- 2. Rebuild the children pointing at the new parent (schemas otherwise
--    unchanged), preserving all rows/ids.
CREATE TABLE benchmark_session_members_new (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id     INTEGER NOT NULL REFERENCES benchmark_attempts_new(id),
    run_id         TEXT NOT NULL REFERENCES benchmark_runs(run_id),
    session_id     TEXT NOT NULL,
    role           TEXT NOT NULL,
    model_returned TEXT,
    UNIQUE(attempt_id, session_id)
);

INSERT INTO benchmark_session_members_new
  (id, attempt_id, run_id, session_id, role, model_returned)
SELECT id, attempt_id, run_id, session_id, role, model_returned
FROM benchmark_session_members;

CREATE TABLE benchmark_scores_new (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id  INTEGER NOT NULL REFERENCES benchmark_attempts_new(id),
    run_id      TEXT NOT NULL REFERENCES benchmark_runs(run_id),
    scorer      TEXT NOT NULL,
    score       REAL NOT NULL DEFAULT 0,
    passed      INTEGER NOT NULL DEFAULT 0,
    rationale   TEXT,
    judge_model TEXT,
    rubric_hash TEXT,
    degraded    INTEGER NOT NULL DEFAULT 0
);

INSERT INTO benchmark_scores_new
  (id, attempt_id, run_id, scorer, score, passed, rationale, judge_model, rubric_hash, degraded)
SELECT id, attempt_id, run_id, scorer, score, passed, rationale, judge_model, rubric_hash, degraded
FROM benchmark_scores;

-- 3. Drop the old children (no rows reference them) then the old parent (now
--    unreferenced), and swap the *_new tables into the canonical names. The
--    RENAME of benchmark_attempts_new fixes up the *_new children's FK refs.
DROP TABLE benchmark_scores;
DROP TABLE benchmark_session_members;
DROP TABLE benchmark_attempts;

ALTER TABLE benchmark_attempts_new RENAME TO benchmark_attempts;
ALTER TABLE benchmark_session_members_new RENAME TO benchmark_session_members;
ALTER TABLE benchmark_scores_new RENAME TO benchmark_scores;

-- 4. Recreate the indexes migration 061 defined.
CREATE INDEX IF NOT EXISTS idx_benchmark_attempts_run ON benchmark_attempts(run_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_members_run ON benchmark_session_members(run_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_members_session ON benchmark_session_members(session_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_scores_attempt ON benchmark_scores(attempt_id);
CREATE INDEX IF NOT EXISTS idx_benchmark_scores_run ON benchmark_scores(run_id);
