-- Org-tier observability alerting (obs-org-tier plan §6 / OP6b).
--
-- obs_alert_rules are admin-authored threshold rules over the content-free
-- obs_summaries aggregate (error rate / cost / p95 latency) that fire a webhook
-- when crossed. DISTINCT from the api_turns budget caps (which alert on proxy
-- spend) — these alert on custom-app / agent trajectory health. The Evaluator
-- (internal/orgserver/obsalert) polls them, compares the metric to the
-- threshold, honours a cooldown via last_fired_at, and logs every fire to
-- obs_alert_events. Server-only; no agent side.
CREATE TABLE IF NOT EXISTS obs_alert_rules (
    id               TEXT PRIMARY KEY,
    org_id           TEXT NOT NULL,
    name             TEXT NOT NULL DEFAULT '',
    metric           TEXT NOT NULL,                 -- error_rate | cost_usd | latency_p95_ms
    comparator       TEXT NOT NULL DEFAULT 'gt',    -- gt | gte
    threshold        REAL NOT NULL,
    window_days      INTEGER NOT NULL DEFAULT 7,
    webhook_url      TEXT NOT NULL DEFAULT '',
    cooldown_minutes INTEGER NOT NULL DEFAULT 360,  -- min gap between fires (default 6h)
    enabled          INTEGER NOT NULL DEFAULT 1,
    last_fired_at    TEXT NOT NULL DEFAULT '',
    last_value       REAL NOT NULL DEFAULT 0,
    created_by       TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obs_alert_rules_org ON obs_alert_rules(org_id);

CREATE TABLE IF NOT EXISTS obs_alert_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_id     TEXT NOT NULL,
    org_id      TEXT NOT NULL,
    metric      TEXT NOT NULL,
    threshold   REAL NOT NULL,
    value       REAL NOT NULL,
    delivered   INTEGER NOT NULL DEFAULT 0,         -- webhook delivery success
    fired_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_obs_alert_events_rule ON obs_alert_events(rule_id, fired_at);
