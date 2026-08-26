-- Agent Arena: multi-harness prompt runs with judged merge-back.
-- Plan: docs/plans/agent-arena-terminal-multi-harness-2026-08-22.md
--
-- One arena_runs row per operator-initiated run (project + prompt +
-- candidate harness set + judge). One arena_candidates row per
-- (run, harness) slot. Patch text and transcripts stay on disk — the DB
-- carries paths, stats, scores and provenance only.
--
-- run status:       pending | running | judging | complete | failed
-- candidate status: pending | running | done | failed | timeout |
--                   judged | kept | discarded

CREATE TABLE IF NOT EXISTS arena_runs (
    id           TEXT PRIMARY KEY,
    project_root TEXT NOT NULL,
    base_branch  TEXT NOT NULL,
    base_sha     TEXT NOT NULL,
    prompt       TEXT NOT NULL,
    judge_tool   TEXT NOT NULL DEFAULT '',
    judge_model  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_arena_runs_created ON arena_runs(created_at);

CREATE TABLE IF NOT EXISTS arena_candidates (
    id                   TEXT PRIMARY KEY,
    run_id               TEXT NOT NULL REFERENCES arena_runs(id),
    tool                 TEXT NOT NULL,
    model                TEXT NOT NULL DEFAULT '',
    seq                  INTEGER NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending',
    branch_name          TEXT NOT NULL DEFAULT '',
    worktree_path        TEXT NOT NULL DEFAULT '',
    patch_path           TEXT NOT NULL DEFAULT '',
    exit_code            INTEGER,
    wall_ms              INTEGER,
    timed_out            INTEGER NOT NULL DEFAULT 0,
    final_answer_excerpt TEXT NOT NULL DEFAULT '',
    diff_files           INTEGER NOT NULL DEFAULT 0,
    diff_added           INTEGER NOT NULL DEFAULT 0,
    diff_removed         INTEGER NOT NULL DEFAULT 0,
    input_tokens         INTEGER NOT NULL DEFAULT 0,
    output_tokens        INTEGER NOT NULL DEFAULT 0,
    cost_usd             REAL NOT NULL DEFAULT 0,
    session_ids          TEXT NOT NULL DEFAULT '[]',
    scores               TEXT NOT NULL DEFAULT '',
    verdict              TEXT NOT NULL DEFAULT '',
    kept_commit_sha      TEXT NOT NULL DEFAULT '',
    error                TEXT NOT NULL DEFAULT '',
    updated_at           TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_arena_candidates_run ON arena_candidates(run_id, seq);
