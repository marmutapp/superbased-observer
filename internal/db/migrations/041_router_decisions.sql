-- 041_router_decisions.sql — model-routing decision log (P0, §R9.1).
--
-- Backs the model-routing arc (docs/model-routing-spec.md). One row per
-- router evaluation — advise AND enforce modes both append here so the
-- dashboard, `observer routing savings`, and the §R18.2 advise-mode
-- shadow A/B all read one substrate. P0 writes nothing on live traffic
-- (advisory phase); the table ships first so the schema, the store seam,
-- and the privacy pinning land before any writer exists.
--
-- Privacy posture (CLAUDE.md "no contents in DB" Don't + §R9.1/§R23):
-- - Models, enum reason codes, policy hashes, dollars, counts, and row
--   refs ONLY. No prompt content, no paths, no command text.
-- - NODE-LOCAL: explicitly EXCLUDED from
--   internal/store/orgpush.go::SelectUnpushedSince and pinned in
--   tests/invariant/privacy_test.go (same posture as the cachetrack +
--   advisor tables). The §R19.4 org rollup, when it ships, pushes a
--   separate AGGREGATE wire shape — never these rows.
--
-- Column notes:
-- - api_turn_id is NULL for decisions with no proxied turn to hang on
--   (Channel-A config recommendations, §R26 item 1).
-- - mode: 'off' | 'advise' | 'enforce'; channel: 'A' | 'B'.
-- - reason_codes is a JSON array of closed-enum codes
--   (internal/routing.ReasonCode) so the dashboard can aggregate.
-- - applied=1 only when an enforce-mode rewrite actually went upstream.
-- - outcome_backref optionally points at a later row that graded this
--   decision (§R18.3 outcome loop; the escalation row for a downshift).
-- - policy_hash is the content hash of the exact policy that decided
--   (§R6.6) so rollbacks stay attributable.
-- - estimate_version versions the savings/cache-forfeit math (§R13) so
--   calibration can grade the estimator itself.

CREATE TABLE IF NOT EXISTS router_decisions (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    api_turn_id      INTEGER REFERENCES api_turns(id),
    session_id       TEXT,
    ts               TEXT    NOT NULL,
    mode             TEXT    NOT NULL,
    channel          TEXT    NOT NULL,
    original_model   TEXT    NOT NULL,
    selected_model   TEXT    NOT NULL,
    turn_kind        TEXT    NOT NULL,
    policy_name      TEXT    NOT NULL DEFAULT '',
    policy_hash      TEXT    NOT NULL,
    reason_codes     TEXT    NOT NULL DEFAULT '[]',
    est_savings_usd  REAL    NOT NULL DEFAULT 0,
    cache_forfeit_usd REAL   NOT NULL DEFAULT 0,
    estimate_version TEXT    NOT NULL DEFAULT '',
    applied          INTEGER NOT NULL DEFAULT 0,
    outcome_backref  INTEGER REFERENCES router_decisions(id)
);
CREATE INDEX IF NOT EXISTS idx_router_decisions_session ON router_decisions(session_id);
CREATE INDEX IF NOT EXISTS idx_router_decisions_ts      ON router_decisions(ts);
CREATE INDEX IF NOT EXISTS idx_router_decisions_kind    ON router_decisions(turn_kind);
