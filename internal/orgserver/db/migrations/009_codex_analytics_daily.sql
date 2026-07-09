-- 009_codex_analytics_daily.sql — server-side store for OpenAI Codex's org
-- analytics APIs (native-console instance #2 / Rail C). Sibling to
-- 008_cc_analytics_daily but with TWO additions the cross-vendor research forced:
--
--   * surface — Codex exposes the analytics over TWO distinct APIs:
--       'chatgpt_enterprise' — api.chatgpt.com workspace analytics, cost in CREDITS;
--       'openai_org'         — api.openai.com org usage/cost, cost in DOLLARS.
--     A tenant may poll both; the surface dimension keeps them from colliding and
--     records which API a row came from.
--   * unit — THE cross-vendor unit trap. Three analytics surfaces meter cost in
--     three different units (CC cents, Codex-Enterprise credits, OpenAI-org
--     dollars). The spendCTE merge must NEVER sum mixed units, so every row
--     records its metric's unit ('credits' | 'usd' | 'tokens' | 'count'); the
--     cost-merge (Phase 3, gated) normalizes to USD per surface at read time.
--
-- Admin-keyed, server-side only — the agent never touches this table (it is
-- absent from internal/store/orgpush.go by construction). Filled by
-- internal/orgserver/codexanalytics.Poller.
--
-- user_key is the analytics actor identity AS RETURNED:
--   * chatgpt_enterprise: the user's email "where workspace settings permit",
--     else an OpenAI workspace user id (join to org_members.email when it is an
--     email; bucket as unenrolled otherwise).
--   * openai_org: an OpenAI user_id (NOT email) — needs a two-step resolve via
--     the Admin Users API before the org_members email join (see resolver.go).
-- actor_type carries 'user' | 'automation' | 'workspace'.
-- Metric vocabulary (codexanalytics const block): cost (unit credits|usd),
-- tokens_input/output/cached (unit tokens), threads/turns/model_requests/sessions
-- (unit count). Per-model breakdowns are summed into user-day totals (the grain
-- the spend merge needs); acceptance/lines are NOT exposed by Codex's analytics.
CREATE TABLE IF NOT EXISTS codex_analytics_daily (
    day          TEXT NOT NULL,   -- YYYY-MM-DD (UTC)
    user_key     TEXT NOT NULL,   -- email | workspace user id | OpenAI user_id
    actor_type   TEXT,            -- user | automation | workspace
    surface      TEXT NOT NULL,   -- chatgpt_enterprise | openai_org
    unit         TEXT,            -- credits | usd | tokens | count
    metric       TEXT NOT NULL,
    value        REAL NOT NULL,
    org_id       TEXT,
    workspace_id TEXT,            -- chatgpt_enterprise workspace scope (NULL for openai_org)
    pulled_at    TEXT NOT NULL,   -- RFC3339 when the poller wrote this row
    UNIQUE(day, user_key, surface, metric)
);

CREATE INDEX IF NOT EXISTS idx_codex_analytics_day ON codex_analytics_daily(day);
