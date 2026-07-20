-- obs subsystem schema v2 — minimal eval plane (plan §8).
--
-- Separability/privacy (plan §2.2/§10, decision D3): like every obs_* table
-- these are NODE-LOCAL and applied only by the obs applier when [observability]
-- is enabled. They are pinned by tests/invariant/privacy_test.go's
-- forbiddenCacheTables sentinel so the names can never reach
-- internal/store/orgpush.go. Datasets snapshot a trace's input/output at
-- creation time (reproducible runs); raw bodies are written only under the
-- ContentGate, content_hash always.

CREATE TABLE IF NOT EXISTS obs_datasets (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS obs_dataset_items (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    dataset_id   INTEGER NOT NULL,          -- FK WITHIN obs_* only (soft)
    trace_id     TEXT,
    span_id      TEXT,
    input        TEXT,                       -- snapshot; raw only under ContentGate
    output       TEXT,                       -- snapshot; raw only under ContentGate
    reference    TEXT,                       -- expected/gold, operator-supplied
    content_hash TEXT NOT NULL,              -- content-free signal, always present
    created_at   TEXT NOT NULL,
    UNIQUE(dataset_id, span_id)
);
CREATE INDEX IF NOT EXISTS idx_obs_dataset_items_ds ON obs_dataset_items(dataset_id);

CREATE TABLE IF NOT EXISTS obs_eval_runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    dataset_id INTEGER NOT NULL,            -- FK WITHIN obs_* only (soft)
    name       TEXT,
    scorers    TEXT NOT NULL DEFAULT '[]',  -- JSON array of scorer specs
    started_at TEXT NOT NULL,
    ended_at   TEXT,
    total      INTEGER NOT NULL DEFAULT 0,
    passed     INTEGER NOT NULL DEFAULT 0,
    mean_score REAL NOT NULL DEFAULT 0,
    status     TEXT NOT NULL DEFAULT 'running'
);
CREATE INDEX IF NOT EXISTS idx_obs_eval_runs_ds ON obs_eval_runs(dataset_id);

CREATE TABLE IF NOT EXISTS obs_eval_scores (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER,                     -- NULL for online-sampled scores
    item_id    INTEGER,                     -- NULL for online-sampled scores
    span_id    TEXT,
    scorer     TEXT NOT NULL,
    score      REAL NOT NULL,
    passed     INTEGER NOT NULL,
    rationale  TEXT,
    source     TEXT NOT NULL DEFAULT 'run', -- 'run' | 'online'
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obs_eval_scores_run ON obs_eval_scores(run_id);
CREATE INDEX IF NOT EXISTS idx_obs_eval_scores_span ON obs_eval_scores(span_id);
