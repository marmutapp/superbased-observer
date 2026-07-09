-- Model-routing org surfaces (model-routing spec §R19), paired with
-- agent-side migration 043_org_routing_policies.
--
-- routing_summaries receives the §R19.4 AGGREGATE rollup the agent
-- pushes under its node-side [org_client.share] routing_summary
-- opt-in: counts + dollars by (day, tier, reason, mode) — never
-- decision rows, never model ids. Upsert by natural key so a re-pushed
-- window is idempotent.
CREATE TABLE IF NOT EXISTS routing_summaries (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    org_id             TEXT NOT NULL,
    user_email         TEXT NOT NULL DEFAULT '',
    day                TEXT NOT NULL,
    tier               TEXT NOT NULL,
    reason             TEXT NOT NULL DEFAULT '',
    mode               TEXT NOT NULL DEFAULT '',
    decisions          INTEGER NOT NULL DEFAULT 0,
    applied            INTEGER NOT NULL DEFAULT 0,
    est_savings_usd    REAL NOT NULL DEFAULT 0,
    cache_forfeit_usd  REAL NOT NULL DEFAULT 0,
    pushed_at          TEXT NOT NULL,
    pushed_by_user_id  TEXT NOT NULL,
    UNIQUE (org_id, user_email, day, tier, reason, mode)
);
CREATE INDEX IF NOT EXISTS idx_routing_summaries_org_day ON routing_summaries(org_id, day);

-- org_routing_policies is the versioned, signed policy registry
-- (§R19.1/§R19.2): the admin publishes; agents fetch + cache. The
-- body is a TOML fragment in the [routing] vocabulary; enabled/mode
-- keys in it are STRUCTURALLY ignored by the agent's composer —
-- enforcement is node-side opt-in by design (§R23: no remote enforce
-- toggle exists).
CREATE TABLE IF NOT EXISTS org_routing_policies (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    version     INTEGER NOT NULL UNIQUE,
    body        TEXT NOT NULL,
    body_hash   TEXT NOT NULL,
    signature   TEXT NOT NULL,
    created_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL
);

-- routing_policy_audit is the §R19.2 change log: who published which
-- version when.
CREATE TABLE IF NOT EXISTS routing_policy_audit (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    version    INTEGER NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT NOT NULL,
    at         TEXT NOT NULL
);

-- routing_policy_keys holds the server's Ed25519 policy-signing key
-- (single row, generated on first publish). Agents TOFU-pin the public
-- half on first receipt (the fetch channel itself rides the enrolment
-- bearer + TLS trust).
CREATE TABLE IF NOT EXISTS routing_policy_keys (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    public_key  TEXT NOT NULL,
    private_key TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
