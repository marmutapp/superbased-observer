-- Org-tier observability T7 — per-item eval scores (Plane-A eval-run detail
-- org tier, gap-audit 2026-07-10 §1 / §2.2 / §6). Server-only pair for the node
-- obs eval tables (obs_eval_scores / obs_eval_runs / obs_datasets /
-- obs_dataset_items); the rows arrive under [org_client.share.obs].eval_items
-- and are composed via the obs provider seam (orgpush.go never names these
-- tables — the privacy sentinel stays green).
--
-- Distinct from obs_eval_summaries (migration 015 / T4), which holds run/scorer
-- AGGREGATES only. This table holds the per-item scores that let the org Evals
-- page drill into ONE run and diff two runs cell-for-cell.
--
-- Privacy posture (mirrors obs_admission_events): the score METADATA is
-- content-free and always ships (run/dataset identity, span/trace soft-join
-- keys, scorer, score/pass verdict, duration, ts, content_hash). The four
-- content-bearing columns (input_excerpt, expected_excerpt, output_excerpt,
-- rationale) arrive ONLY when the node shares full content and are stored NULL
-- otherwise, so the server cannot tell "stripped" from "never had one" (no
-- posture leak — same as obs_content / obs_admission_events). The org admin
-- cannot force any of this on remotely.
--
-- Idempotent re-push: the natural key is (org_id, user_email, run_id, item_id,
-- scorer). user_email = the re-pinned pusher (keeps each node's runs distinct);
-- a re-push UPSERTs so a later full-content window can backfill the excerpts on
-- a row that first arrived metadata-only.
CREATE TABLE IF NOT EXISTS obs_eval_items (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',   -- the pushing developer/operator (re-pinned)
    run_id            INTEGER NOT NULL,           -- node-local obs_eval_runs.id
    run_name          TEXT NOT NULL DEFAULT '',
    dataset_id        INTEGER NOT NULL DEFAULT 0, -- node-local obs_eval_runs.dataset_id
    dataset_name      TEXT NOT NULL DEFAULT '',
    item_id           INTEGER NOT NULL DEFAULT 0, -- node-local obs_dataset_items.id (0 when none)
    span_id           TEXT NOT NULL DEFAULT '',   -- content-free soft join → obs_spans
    trace_id          TEXT NOT NULL DEFAULT '',   -- content-free soft join → obs_traces (trajectory link)
    scorer            TEXT NOT NULL DEFAULT '',
    score             REAL NOT NULL DEFAULT 0,
    passed            INTEGER NOT NULL DEFAULT 0,
    source            TEXT NOT NULL DEFAULT 'run', -- run | online (only 'run' ships)
    duration_ms       INTEGER NOT NULL DEFAULT 0,  -- scored span duration
    ts                TEXT NOT NULL DEFAULT '',     -- score instant (RFC3339)
    content_hash      TEXT NOT NULL DEFAULT '',     -- content-free signal, always present
    input_excerpt     TEXT,                         -- NULL unless full-content sharing (gated)
    expected_excerpt  TEXT,                         -- NULL unless full-content sharing (gated)
    output_excerpt    TEXT,                         -- NULL unless full-content sharing (gated)
    rationale         TEXT,                         -- NULL unless full-content sharing (gated; verdict prose)
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, user_email, run_id, item_id, scorer)
);
CREATE INDEX IF NOT EXISTS idx_obs_eval_items_org_run ON obs_eval_items(org_id, pushed_by_user_id, run_id);
CREATE INDEX IF NOT EXISTS idx_obs_eval_items_org_ts ON obs_eval_items(org_id, ts);
CREATE INDEX IF NOT EXISTS idx_obs_eval_items_org_dataset ON obs_eval_items(org_id, dataset_name);
