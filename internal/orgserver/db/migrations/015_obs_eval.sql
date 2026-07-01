-- Org-tier observability T4 — eval-run health (obs-org-tier plan §5.1).
--
-- obs_eval_summaries receives the content-free eval-run aggregates the agent
-- pushes under [org_client.share] obs_eval_summary: per (day, dataset, run,
-- scorer, source) total/passed counts + mean/min score. No per-item bodies,
-- no reference/output text — the team-scale eval-health surface (run history,
-- regression tracking). Upsert by natural key; server-only.
CREATE TABLE IF NOT EXISTS obs_eval_summaries (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',
    day               TEXT NOT NULL,
    dataset_name      TEXT NOT NULL DEFAULT '',
    run_name          TEXT NOT NULL DEFAULT '',
    scorer_name       TEXT NOT NULL DEFAULT '',
    source            TEXT NOT NULL DEFAULT 'run',
    total             INTEGER NOT NULL DEFAULT 0,
    passed            INTEGER NOT NULL DEFAULT 0,
    mean_score        REAL NOT NULL DEFAULT 0,
    min_score         REAL NOT NULL DEFAULT 0,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, user_email, day, dataset_name, run_name, scorer_name, source)
);
CREATE INDEX IF NOT EXISTS idx_obs_eval_summaries_org_day ON obs_eval_summaries(org_id, day);
