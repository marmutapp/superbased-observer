-- Org-tier observability T5 — per-END-USER spend (org-budget guardrails plan
-- §2.1). obs_enduser_spend receives the per-(day, end_user) cost + token +
-- trace-count aggregate a node pushes under its [org_client.share] obs_summary
-- opt-in AND its full-content / admin-managed posture (the end-user id is PII).
--
-- DISTINCT from obs_summaries: this attributes spend to the hosted-app
-- END-USER (obs_traces.user — OTel enduser.id / the admission `user` / the
-- proxy X-Superbased-User header), NOT the developer/operator. It is the ONLY
-- per-end-user data the server holds; the node-local budget surfaces cover the
-- single-instance case, this table exists solely to enable CROSS-INSTANCE
-- rollup (SUM across every node that shares).
--
-- Server-only (no agent migration: obs_* tables are obs-owned + node-local;
-- only this aggregate crosses the wire, via the obs provider seam). Upsert by
-- natural key so a re-pushed window is idempotent.
--
-- The UNIQUE key INCLUDES user_email (the pushing developer/operator, re-pinned
-- server-side to the authenticated pusher) so each node's per-(day, end_user)
-- row is a DISTINCT row — the rollup SUMs across user_email to get the
-- cross-instance total for an end_user. end_user is app-shared and is NOT
-- re-pinned (it is the same identity across nodes, by design).
CREATE TABLE IF NOT EXISTS obs_enduser_spend (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id            TEXT NOT NULL,
    user_email        TEXT NOT NULL DEFAULT '',   -- the pushing developer/operator (re-pinned)
    day               TEXT NOT NULL,              -- UTC YYYY-MM-DD
    end_user          TEXT NOT NULL DEFAULT '',   -- the hosted-app end-user id (PII; app-shared)
    cost_usd          REAL NOT NULL DEFAULT 0,
    traces            INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    UNIQUE (org_id, user_email, day, end_user)
);
CREATE INDEX IF NOT EXISTS idx_obs_enduser_spend_org_day ON obs_enduser_spend(org_id, day);
