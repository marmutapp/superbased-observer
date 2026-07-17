-- 0006_alerts.sql — node-side observability alerting fired-event log
-- (general-observability gap-audit item #9, §2.6).
--
-- The node analogue of the org-server obs_alert_events table (server migration
-- 016): a row per threshold crossing the node-local alert evaluator
-- (internal/obs/alert, wired in cmd/observer) fires against this node's OWN
-- obs_* data. Unlike the org side there is NO obs_alert_rules table — node
-- rules are authored in [observability.alerts] config, not a DB row — so the
-- per-rule last-fired needed for cooldown/dedup is derived from
-- MAX(fired_at) here (one owner, one table).
--
-- Privacy (plan §10): node-local, obs-owned. Pinned by
-- tests/invariant/privacy_test.go's AST sentinel so the obs_* name can never
-- appear in internal/store/orgpush.go — this table is NEVER on the org wire.
CREATE TABLE IF NOT EXISTS obs_alert_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    rule_name      TEXT NOT NULL,
    metric         TEXT NOT NULL,                 -- error_rate | cost_usd | latency_p95_ms
    comparator     TEXT NOT NULL DEFAULT 'gt',    -- gt | gte
    threshold      REAL NOT NULL,
    value          REAL NOT NULL,                 -- observed metric value at fire time
    window_minutes INTEGER NOT NULL DEFAULT 0,
    delivered      INTEGER NOT NULL DEFAULT 0,    -- webhook delivery success (0 when no webhook / failed)
    fired_at       TEXT NOT NULL                  -- RFC3339Nano UTC
);
CREATE INDEX IF NOT EXISTS idx_obs_alert_events_rule ON obs_alert_events(rule_name, fired_at);
CREATE INDEX IF NOT EXISTS idx_obs_alert_events_fired ON obs_alert_events(fired_at);
