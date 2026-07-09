-- 042_model_calibration.sql — per-project model calibration aggregates
-- (P0 schema for §R7.2 / §R20).
--
-- One row per (model, turn_kind, project, window): the observed outcome
-- aggregates the Model Value Report computes and the §R7.2 calibration
-- job (P2) refreshes on a ticker. P0 writers are the report surfaces
-- (`observer model-value --save-calibration`); the periodic job lands in
-- P2 on the same store seam.
--
-- Privacy posture: aggregates only — model ids, enum turn-kinds, counts,
-- milliseconds, dollars. No content, no paths (projects are referenced
-- by rowid). NODE-LOCAL: excluded from
-- internal/store/orgpush.go::SelectUnpushedSince and pinned in
-- tests/invariant/privacy_test.go alongside router_decisions.
--
-- Column notes:
-- - project_id = 0 means "all projects" (the global cell). NOT NULL so
--   the UNIQUE key behaves (SQLite treats NULLs as distinct in UNIQUE).
-- - n is the turn volume behind the aggregates; *_graded columns carry
--   the denominators actually graded for each rate (latency and error
--   data exist only on proxy-observed rows, so n_graded ≤ n — §R7.2
--   demands sample sizes ship next to every delta).
-- - Attribution honesty (§R7.2): these are correlational observations.
--   Consumers MUST surface n + confidence intervals, never bare rates.

CREATE TABLE IF NOT EXISTS model_calibration (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    model              TEXT    NOT NULL,
    turn_kind          TEXT    NOT NULL,
    project_id         INTEGER NOT NULL DEFAULT 0,
    window_days        INTEGER NOT NULL,
    computed_at        TEXT    NOT NULL,
    n                  INTEGER NOT NULL DEFAULT 0,
    cost_usd_total     REAL    NOT NULL DEFAULT 0,
    latency_p50_ms     INTEGER NOT NULL DEFAULT 0,
    latency_p95_ms     INTEGER NOT NULL DEFAULT 0,
    latency_graded     INTEGER NOT NULL DEFAULT 0,
    error_count        INTEGER NOT NULL DEFAULT 0,
    error_graded       INTEGER NOT NULL DEFAULT 0,
    tool_failure_count INTEGER NOT NULL DEFAULT 0,
    tool_action_count  INTEGER NOT NULL DEFAULT 0,
    UNIQUE(model, turn_kind, project_id, window_days)
);
CREATE INDEX IF NOT EXISTS idx_model_calibration_model ON model_calibration(model, turn_kind);
