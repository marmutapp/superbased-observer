-- 008_cc_analytics_daily.sql — server-side store for a coding assistant's own
-- org-analytics API (native-console Workstream C / Phase 5).
--
-- Filled by internal/orgserver/ccanalytics.Poller from the Claude Code
-- Analytics API: per (day, user, metric) values — lines accepted, accept rate,
-- DAU, and per-user token/cost where the org configured OTel. Admin-keyed,
-- server-side only; the agent never touches this table.
--
-- This is org-aggregate data the agents do NOT push. Two consumers (a separate
-- integration step, gated on a live key):
--   1. CC-EXCLUSIVE metrics (accept rate, lines accepted, DAU) — surfaced as a
--      new rollup; NO duplication risk (agents never capture these).
--   2. cost/tokens — merged into the ONE proxy-deduped spend definition
--      (rollup/cost.go::spendCTE) with a `spend_source` dimension where the
--      rule is "agent_push wins per (user, day)", filling only non-enrolled
--      users. That merge + the CC-user -> org user_id identity mapping need the
--      live API's schema/identity model to land correctly.
--
-- user_key is the analytics actor identity AS RETURNED: actor.email_address for
-- a user_actor (the case-insensitive join to org_members.email — see
-- ccanalytics.ResolveOrgUserID) or actor.api_key_name for an api_actor
-- (service/CI; no org email — bucketed as automation/unenrolled). actor_type
-- carries which it is. Metric vocabulary (ccanalytics const block): sessions,
-- lines_added, lines_removed, commits, pull_requests, cost_usd (DOLLARS, summed
-- across model_breakdown; the API reports cents), tokens_input/output/
-- cache_read/cache_creation (summed), and tool_<tool>_accepted/_rejected
-- (acceptance rate is derived, not stored — the API has no rate field).
CREATE TABLE IF NOT EXISTS cc_analytics_daily (
    day        TEXT NOT NULL,   -- YYYY-MM-DD (UTC)
    user_key   TEXT NOT NULL,   -- email (user_actor) | api_key_name (api_actor)
    actor_type TEXT,            -- user_actor | api_actor
    metric     TEXT NOT NULL,
    value      REAL NOT NULL,
    org_id     TEXT,
    pulled_at  TEXT NOT NULL,   -- RFC3339 when the poller wrote this row
    UNIQUE(day, user_key, metric)
);

CREATE INDEX IF NOT EXISTS idx_cc_analytics_day ON cc_analytics_daily(day);
