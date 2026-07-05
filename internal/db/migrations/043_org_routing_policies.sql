-- Agent-side cache of the org-distributed routing policy (model-routing
-- spec §R19.1; paired with org-server migration 006_routing.sql).
-- Single-row cache: the newest verified policy document plus the
-- TOFU-pinned server signing key. NODE-LOCAL: this table never enters
-- the org push (pinned in tests/invariant/privacy_test.go) — it is
-- RECEIVED state, and its body may describe org-internal projects.
--
-- Enforcement posture (§R23): caching a policy never enables anything.
-- The composer (routingconfig.ComposeOrgPolicy) structurally ignores
-- enabled/mode keys in the body — the node operator's [routing] config
-- is the only enforce switch.
CREATE TABLE IF NOT EXISTS org_routing_policies (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    version       INTEGER NOT NULL,
    body          TEXT NOT NULL,
    body_hash     TEXT NOT NULL,
    signature     TEXT NOT NULL,
    server_pubkey TEXT NOT NULL,
    received_at   TEXT NOT NULL
);
