-- 005_guard_events.sql — server-side guard-event store (guard spec
-- §14.3 central reporting, G14).
--
-- Agents have pushed PushEnvelope.guard_events rows since G2; until
-- this migration the ingest layer ACKed and dropped them. One row per
-- pushed guard verdict, shaped to receive orgcontract.GuardEventRow.
--
-- Dedup key: (chain_hash, user_id). Agent guard rows carry no
-- (source_file, source_event_id) natural key — the tamper-evidence
-- chain hash (guard spec §10.4) is unique per event per node, so it
-- is the dedup anchor. A defensive synthesized hash is bound instead
-- when an envelope arrives with an empty chain_hash (see
-- ingest.guardDedupKey); the column never holds ''.
--
-- Privacy posture (guard spec §10.2 — mirrors actions.target):
-- rule_id / category / severity / decision / enforced / source /
-- tool / event_kind / timestamps / target_hash / chain links are
-- content-free and always present. reason, target_excerpt and
-- taint_origin are content-bearing: they arrive ONLY when the node
-- operator opted into [org_client.share].full_content and are stored
-- NULL otherwise. The org admin cannot force them on.
--
-- chain_prev/chain_hash let the rollup layer detect discontinuous
-- audit chains per user (§14.3 "agents with broken audit chains")
-- via set-membership: an event whose chain_prev is absent from the
-- user's pushed chain_hash set starts a new chain segment.
CREATE TABLE IF NOT EXISTS guard_events (
    chain_hash        TEXT NOT NULL,  -- sha256 hex chain link (or synthesized dedup key)
    user_id           TEXT NOT NULL,
    org_id            TEXT,
    user_email        TEXT,
    session_id        TEXT,
    timestamp         TEXT,           -- RFC3339, event time on the agent
    tool              TEXT,
    event_kind        TEXT,
    rule_id           TEXT,
    category          TEXT,
    severity          TEXT,
    decision          TEXT,
    degraded_from     TEXT,
    enforced          INTEGER,
    source            TEXT,
    target_hash       TEXT,
    reason            TEXT,           -- full_content opt-in only; NULL otherwise
    target_excerpt    TEXT,           -- full_content opt-in only; NULL otherwise
    taint_origin      TEXT,           -- full_content opt-in only; NULL otherwise
    chain_prev        TEXT,
    pushed_at         TEXT NOT NULL,
    pushed_by_user_id TEXT NOT NULL,
    PRIMARY KEY (chain_hash, user_id)
);

CREATE INDEX IF NOT EXISTS idx_guard_events_user_ts    ON guard_events(user_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_guard_events_rule       ON guard_events(rule_id);
CREATE INDEX IF NOT EXISTS idx_guard_events_ts         ON guard_events(timestamp);
-- Serves the chain-continuity probe (chain_prev IN the user's
-- chain_hash set); the PK index leads on chain_hash so it cannot.
CREATE INDEX IF NOT EXISTS idx_guard_events_user_chain ON guard_events(user_id, chain_hash);
