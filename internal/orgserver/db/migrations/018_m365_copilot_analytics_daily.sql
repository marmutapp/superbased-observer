-- 018_m365_copilot_analytics_daily.sql — server-side store for Microsoft 365
-- Copilot's org-tier native-console connector (World 1 / native-console
-- instance #4). Sibling to 009_codex_analytics_daily / 010_copilot_analytics_daily,
-- but for MICROSOFT's M365 Copilot — NOT GitHub Copilot (that is instance #3,
-- copilot_analytics_daily). The two share only the word "Copilot"; this is a
-- categorically different vendor surface (graph.microsoft.com, Entra app-only).
--
-- TWO tables, because Rail A carries actual prompt/response CONTENT:
--
--   * m365_copilot_analytics_daily — the aggregated daily metric rollup, shaped
--     like codex_analytics_daily. surface distinguishes the two rails
--     ('graph' = Rail A Graph aiInteractionHistory content-derived counts;
--     'purview' = Rail B Office 365 Management Activity metadata). app_class is
--     the M365 surface discriminator (BizChat / Teams / WebChat / Word / Outlook
--     / …) — the cross-vendor unit trap analogue: a BizChat prompt and a Word
--     prompt are distinct rows, never summed blind. unit records the metric's
--     unit ('count' | 'interactions'); no cost unit — aiInteractionHistory is
--     NOT metered (re-verify per release; never say "free").
--
--   * m365_copilot_content — the Rail A content bodies (interactionType =
--     userPrompt | aiResponse), one row per aiInteraction. content_hash
--     (sha256-hex of the scrubbed body) is content-free and ALWAYS present;
--     content is content-bearing and is disclosed only behind the ADMIN-TIER
--     view gate (a distinct 'view_m365_copilot_content' audit row before any
--     read), mirroring the otel_content message-content viewer (server mig 007).
--     Unlike otel_content (fed by node opt-in), this content is polled
--     SERVER-SIDE by the admin's own Entra app, so storing it is the admin's
--     deliberate act; metadata-only mode (store_content=false) stores NULL
--     content + the hash so the rollup counts still work.
--
-- Admin-keyed, server-side only — the agent NEVER touches these tables (they are
-- absent from internal/store/orgpush.go by construction, exactly like
-- codex_analytics_daily / copilot_analytics_daily). Filled by
-- internal/orgserver/m365copilotanalytics.Poller. GLOBAL COMMERCIAL CLOUD ONLY
-- (GCC-High / DoD / 21Vianet are NOT supported by getAllEnterpriseInteractions);
-- Copilot Studio agents are excluded; delta queries are unsupported.
--
-- user_key is the Graph user identity as returned (the Entra object id, or the
-- userPrincipalName / email when the resolver maps it); actor_type carries
-- 'user' | 'automation' | 'tenant'.
CREATE TABLE IF NOT EXISTS m365_copilot_analytics_daily (
    day        TEXT NOT NULL,   -- YYYY-MM-DD (UTC)
    user_key   TEXT NOT NULL,   -- Entra object id | userPrincipalName | email
    actor_type TEXT,            -- user | automation | tenant
    surface    TEXT NOT NULL,   -- graph | purview
    app_class  TEXT NOT NULL DEFAULT '', -- M365 surface: bizchat | teams | webchat | word | …
    unit       TEXT,            -- count | interactions
    metric     TEXT NOT NULL,
    value      REAL NOT NULL,
    org_id     TEXT,
    tenant_id  TEXT,            -- Entra tenant the Graph app polled
    pulled_at  TEXT NOT NULL,   -- RFC3339 when the poller wrote this row
    UNIQUE(day, user_key, surface, app_class, metric)
);

CREATE INDEX IF NOT EXISTS idx_m365_copilot_analytics_day ON m365_copilot_analytics_daily(day);

-- Rail A content bodies. Dedup key: interaction_id (Graph's stable aiInteraction
-- id). content is NULL in metadata-only mode; content_hash is always present.
CREATE TABLE IF NOT EXISTS m365_copilot_content (
    interaction_id   TEXT NOT NULL,   -- Graph aiInteraction.id (stable)
    session_id       TEXT,
    request_id       TEXT,
    app_class        TEXT NOT NULL DEFAULT '',
    interaction_type TEXT,            -- userPrompt | aiResponse
    user_key         TEXT NOT NULL,
    org_id           TEXT,
    tenant_id        TEXT,
    content          TEXT,            -- NULL unless store_content (admin-managed poll)
    content_hash     TEXT NOT NULL,   -- sha256-hex of the scrubbed body; content-free
    created_at       TEXT,            -- RFC3339 aiInteraction.createdDateTime
    pulled_at        TEXT NOT NULL,   -- RFC3339 when the poller wrote this row
    UNIQUE(interaction_id)
);

CREATE INDEX IF NOT EXISTS idx_m365_copilot_content_user ON m365_copilot_content(user_key);
CREATE INDEX IF NOT EXISTS idx_m365_copilot_content_session ON m365_copilot_content(session_id);
