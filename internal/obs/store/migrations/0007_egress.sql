-- obs subsystem schema v7 — Plane-A policy egress-routing audit (G22, design
-- §7). Slot 0007 verified: obs migrations end at 0006_alerts.sql.
--
-- Separability/privacy (obs plan §2.2/§10, decision D3): like every obs_* table
-- this is NODE-LOCAL and applied only by the obs applier when [observability]
-- is enabled. It is pinned by tests/invariant/privacy_test.go's
-- forbiddenCacheTables sentinel so the name can never reach
-- internal/store/orgpush.go — no org wire, no server pair. v1 ships NO egress
-- org tier (design §8), so there is no push path at all.
--
-- Privacy: no raw request text is ever stored — only message_hash (the same
-- provenance obs_admission_events uses). model_from/model_to/upstream_id/
-- target_shape/policy_hash are operator-config values (safe to store). user is
-- end-user PII, identical posture to obs_admission_events.user (node-local).
--
-- Chain: SHA-256(prev_hash || 0x1e || canonical bytes), like obs_admission_events,
-- but SERIALIZED against concurrent writers (finding 15): the store insert holds
-- a process mutex across read-tail+insert AND the UNIQUE(prev_hash) constraint is
-- a DB-level backstop so a forked chain cannot even be written. request_id soft-
-- joins to obs_admission_events + api_turns (the design's P0 stable id makes the
-- join live).

CREATE TABLE IF NOT EXISTS obs_egress_decisions (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                      TEXT NOT NULL,
    prev_hash               TEXT NOT NULL DEFAULT '' UNIQUE, -- chain_prev (UNIQUE = no fork)
    row_hash                TEXT NOT NULL,                   -- SHA-256(prev || 0x1e || canonical bytes)
    mode                    TEXT NOT NULL,                   -- advise | enforce (off never records)
    rule_name               TEXT NOT NULL,                   -- the rule that fired
    policy_hash             TEXT NOT NULL,                   -- compiled egress policy hash
    action                  TEXT NOT NULL,                   -- none | route_upstream | route_model | set_effort | deny
    upstream_id             TEXT,                            -- route_upstream target id
    target_shape            TEXT,                            -- anthropic | openai
    model_from              TEXT,                            -- requested (pre-mutation) model
    model_to                TEXT,                            -- route_model target model
    effort                  TEXT,                            -- set_effort level
    reason_code             TEXT NOT NULL,                   -- closed Plane-A reason enum
    must_use_target         INTEGER NOT NULL DEFAULT 0,      -- locality pin intent
    applied                 INTEGER NOT NULL DEFAULT 0,      -- did the proxy realize the directive (enforce only)
    fail_closed             INTEGER NOT NULL DEFAULT 0,      -- did a MustUseTarget route fail closed
    switch_held             INTEGER NOT NULL DEFAULT 0,      -- was a model switch held by cooldown
    est_cache_forfeit_class TEXT,                            -- coarse cache-forfeit class of a switch
    degraded                TEXT,                            -- auditable degrade note ('' = none)
    verdict_decision        TEXT,                            -- admission decision that fed egress
    criterion_id            TEXT,                            -- fired criterion ('' if none)
    message_hash            TEXT,                            -- raw request NEVER stored, only its hash
    request_id              TEXT,                            -- soft join to obs_admission_events + api_turns
    session_id              TEXT,
    tenant                  TEXT,
    user                    TEXT                             -- end-user PII (node-local; never pushed in v1)
);
CREATE INDEX IF NOT EXISTS idx_obs_egress_decisions_ts       ON obs_egress_decisions(ts);
CREATE INDEX IF NOT EXISTS idx_obs_egress_decisions_request  ON obs_egress_decisions(request_id);
CREATE INDEX IF NOT EXISTS idx_obs_egress_decisions_action   ON obs_egress_decisions(action);
CREATE INDEX IF NOT EXISTS idx_obs_egress_decisions_rule     ON obs_egress_decisions(rule_name);
