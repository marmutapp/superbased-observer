-- 002_budgets_audit.sql
--
-- Teams & Org Visibility M3 (org dashboard): two server-side tables that the
-- dashboard owns and the agent never sees.
--
--   1. budgets   — per-team / per-project monthly spend caps with webhook
--                  alerts. The budget evaluator goroutine in cmd/observer-org
--                  polls rolling 30-day spend per scope and fires the webhook
--                  once per newly-crossed threshold (last_fired_threshold is
--                  the high-water mark, so a poll cycle never re-fires).
--
--   2. audit_log — append-only record of admin/lead actions, most importantly
--                  the per-developer drill-down (written BEFORE the
--                  per-developer data is served, so the audit can never lag the
--                  disclosure). org admins read all rows; team leads read only
--                  rows targeting a team they lead.
--
-- Both tables carry NO content (no prompt/output text); they are governance
-- metadata, consistent with the structural-privacy posture of migration 001.

-- ---------------------------------------------------------------------------
-- 1. Budgets
-- ---------------------------------------------------------------------------

-- A budget binds a monthly USD cap to a single scope target (a team_id or a
-- project_root). alert_thresholds is a JSON array of fractions of the cap at
-- which to alert (default 75% / 90% / 100%). last_fired_threshold is the
-- highest threshold already alerted for the current rolling window; the
-- evaluator only fires for thresholds strictly above it, and lowers it again
-- when spend ages out of the 30-day window below a fired threshold, so a
-- later re-crossing re-alerts. There is at most one budget per scope target.
CREATE TABLE IF NOT EXISTS budgets (
    id                   TEXT PRIMARY KEY,
    scope                TEXT NOT NULL CHECK (scope IN ('team', 'project')),
    scope_id             TEXT NOT NULL,
    monthly_usd_cap      REAL NOT NULL CHECK (monthly_usd_cap > 0),
    alert_webhook_url    TEXT,
    alert_thresholds     TEXT NOT NULL DEFAULT '[0.75,0.9,1.0]',
    last_fired_threshold REAL NOT NULL DEFAULT 0,
    created_at           TEXT NOT NULL,
    updated_at           TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_budgets_scope ON budgets(scope, scope_id);

-- ---------------------------------------------------------------------------
-- 2. Audit log
-- ---------------------------------------------------------------------------

-- audit_log is append-only. action is a stable machine code
-- ('view_team_developers', 'drill_down_developers', 'revoke_bearer',
-- 'promote_team_lead', ...). target_team_id scopes a row to a team so a team
-- lead can be shown their own audit trail; it is NULL for org-wide actions.
-- target_detail carries a short non-content identifier (e.g. the affected
-- user_id or jti). source_ip is X-Forwarded-For (first hop) when present, else
-- the remote address.
CREATE TABLE IF NOT EXISTS audit_log (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    actor_user_id  TEXT NOT NULL,
    action         TEXT NOT NULL,
    target_team_id TEXT,
    target_detail  TEXT,
    source_ip      TEXT,
    timestamp      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_log_ts ON audit_log(timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_log_team ON audit_log(target_team_id, timestamp);

-- ---------------------------------------------------------------------------
-- 3. Issued bearers (for admin Revoke)
-- ---------------------------------------------------------------------------

-- Bearers are stateless signed envelopes and are NOT stored for validation
-- (migration 001) — but an admin Revoke needs a concrete jti to add to
-- revoked_bearers, and the dashboard needs to list a developer's live bearers
-- to offer the action. issued_bearers is that index: one row per Mint, written
-- at enrol time. It carries no secret (the signed bearer is never stored), only
-- the public jti and lifetime. A row is informational; revocation still flows
-- through revoked_bearers, which VerifyBearer consults.
CREATE TABLE IF NOT EXISTS issued_bearers (
    jti        TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES org_members(user_id) ON DELETE CASCADE,
    issued_at  TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_issued_bearers_user ON issued_bearers(user_id);
