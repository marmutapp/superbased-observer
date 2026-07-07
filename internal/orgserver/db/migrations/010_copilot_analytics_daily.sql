-- 010_copilot_analytics_daily.sql — server-side store for GitHub Copilot's org
-- analytics surfaces (native-console instance #3 / Rail C). Sibling to
-- 008_cc_analytics_daily + 009_codex_analytics_daily, with the same surface+unit
-- discriminator the cross-vendor research forced — Copilot adds TWO new units:
--
--   * surface — Copilot's Rail C splits into THREE unrelated APIs:
--       'engagement' — the usage-metrics report API (NDJSON behind signed links);
--                      engagement COUNTS only (no tokens, no cost).
--       'seats'      — copilot/billing[/seats]; seat COUNTS (unit 'seats'), the
--                      subscription baseline (× per-seat price at read time).
--       'billing'    — enhanced-billing premium-request usage; $ line items
--                      (unit 'usd'), the AI-Credits/premium metered spend.
--   * unit — THE cross-vendor unit trap, now FIVE shapes across three vendors
--     (CC cents, Codex credits, OpenAI dollars, Copilot 'seats' + 'usd'). A row
--     records its metric's unit ('count' | 'seats' | 'usd' | 'tokens'); the
--     sibling CostSummary read normalizes seats×price + usd to USD. Units are
--     NEVER summed across vendors.
--
-- THE MERGE INVERSION (instance §5.2): unlike CC/Codex, Copilot has no per-token
-- agent spend to dedup against (flat seats + account-level metered billing), so
-- these cost rows do NOT enter rollup/cost.go::spendCTE — copilotanalytics.CostSummary
-- is a SIBLING read. Engagement rows are Copilot-exclusive metrics, never summed
-- into a cost figure.
--
-- Admin-keyed, server-side only — the agent never touches this table (it is
-- absent from internal/store/orgpush.go by construction). Filled by
-- internal/orgserver/copilotanalytics.Poller.
--
-- user_key is the analytics actor identity AS RETURNED:
--   * engagement/seats: a GitHub LOGIN (NOT an email — GitHub emails are often
--     private; the login→org_members join uses an admin-supplied map, see
--     resolver.go). The sentinel '__org__' marks org/enterprise-aggregate rows.
--   * billing: always '__org__' (line items are org/account-level).
-- actor_type carries 'user' | 'automation' (bot/CI seats) | 'org' (aggregate).
CREATE TABLE IF NOT EXISTS copilot_analytics_daily (
    day        TEXT NOT NULL,   -- YYYY-MM-DD (UTC)
    user_key   TEXT NOT NULL,   -- GitHub login | '__org__'
    actor_type TEXT,            -- user | automation | org
    surface    TEXT NOT NULL,   -- engagement | seats | billing
    unit       TEXT,            -- count | seats | usd | tokens
    metric     TEXT NOT NULL,
    value      REAL NOT NULL,
    org_id     TEXT,            -- observer org id
    owner      TEXT,            -- GitHub org login or enterprise slug
    pulled_at  TEXT NOT NULL,   -- RFC3339 when the poller wrote this row
    UNIQUE(day, user_key, surface, metric)
);

CREATE INDEX IF NOT EXISTS idx_copilot_analytics_day ON copilot_analytics_daily(day);
