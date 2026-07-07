-- 029_actions_org_attribution.sql
--
-- Teams & Org Visibility M0 (foundations): nullable attribution columns
-- on the four row types the org server consumes. NULL on solo-local
-- installs and on every row ingested before the agent enrolled.
--
-- The existing dashboard query paths do not project these columns; only
-- the org-mode push path (M2) reads them. They are written by the
-- identity Stamper at insert time when the agent is enrolled, and stay
-- NULL otherwise — so a never-enrolled agent's dashboard responses are
-- byte-identical to pre-029 (proven by tests/invariant).
--
-- Indexes are PARTIAL (WHERE org_id IS NOT NULL) so solo-local installs,
-- where every row has org_id NULL, pay zero index-maintenance cost: the
-- partial index stays empty.

ALTER TABLE actions     ADD COLUMN org_id     TEXT;
ALTER TABLE actions     ADD COLUMN user_email TEXT;
ALTER TABLE sessions    ADD COLUMN org_id     TEXT;
ALTER TABLE sessions    ADD COLUMN user_email TEXT;
ALTER TABLE api_turns   ADD COLUMN org_id     TEXT;
ALTER TABLE api_turns   ADD COLUMN user_email TEXT;
ALTER TABLE token_usage ADD COLUMN org_id     TEXT;
ALTER TABLE token_usage ADD COLUMN user_email TEXT;

CREATE INDEX IF NOT EXISTS idx_actions_org_user      ON actions     (org_id, user_email) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sessions_org_user     ON sessions    (org_id, user_email) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_api_turns_org_user    ON api_turns   (org_id, user_email) WHERE org_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_token_usage_org_user  ON token_usage (org_id, user_email) WHERE org_id IS NOT NULL;
