-- 062_aggregate_submissions.sql — the opt-in aggregate rail's node-local
-- state (docs/plans/g25-optin-aggregate-rail-design-2026-07-11.md §6.5, §9.1).
--
-- Two tables, both NODE-LOCAL and both content-free by construction:
--
--   aggregate_submissions — a per-month submission ledger (replaces the single
--   schema_meta cursor that could not express gaps/retries/corrections,
--   finding #13/#14). One row per finalized UTC month, carrying only the
--   payload hash, the reused-on-retry random submission_id, the wire schema
--   version, an attempt counter, a state enum, and a bounded snapshot of the
--   exact allow-listed JSON that was built for that month (payload_json — the
--   aggregate is content-free by the wire allow-list, so this stores no
--   project paths / prompts / model ids; it exists so `observer aggregate
--   status --raw` can show what actually left the machine).
--
--   aggregate_consent — a single-row consent receipt (§9.1): what schema /
--   endpoint / pricing / cost-method / tool-registry version the operator
--   consented to, the actor/mode, a hash of the disclosure text shown, and the
--   covered DB scope. The daemon submits ONLY while a receipt exists AND its
--   pinned versions still match the live ones; a schema-version bump, an
--   endpoint change, or a tool-registry-version bump invalidates it and
--   suspends submission until re-consent (material-change re-consent, #16).
--
-- These tables MUST NOT leave this machine: pinned in
-- tests/invariant/privacy_test.go (forbidden-table sentinel) and excluded from
-- internal/store/orgpush.go by construction (orgpush names an explicit table
-- allow-list; these are never in it). No paired orgserver migration exists, by
-- design — same posture as the cachetrack / limit_snapshots / benchmark_*
-- tables. The rail is org-independent (works for solo nodes) and never
-- round-trips through the Teams wire.

CREATE TABLE IF NOT EXISTS aggregate_submissions (
    month          TEXT PRIMARY KEY,          -- "2026-06", a fully-elapsed UTC month
    payload_hash   TEXT NOT NULL DEFAULT '',  -- sha256 of the canonical submission JSON
    submission_id  TEXT NOT NULL DEFAULT '',  -- RANDOM per submission; persisted-before-send, reused-on-retry
    schema_version INTEGER NOT NULL DEFAULT 0,
    attempts       INTEGER NOT NULL DEFAULT 0,
    state          TEXT NOT NULL DEFAULT 'pending', -- pending | submitted | failed
    payload_json   TEXT NOT NULL DEFAULT '',  -- bounded snapshot of the allow-listed payload (content-free)
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS aggregate_consent (
    id                    INTEGER PRIMARY KEY CHECK (id = 1), -- single-row table
    schema_version        INTEGER NOT NULL,
    endpoint              TEXT NOT NULL,       -- normalized (aggregate.NormalizeEndpoint)
    pricing_version       TEXT NOT NULL,
    cost_method_version   TEXT NOT NULL,
    tool_registry_version INTEGER NOT NULL,
    actor                 TEXT NOT NULL,       -- interactive | flag | managed
    disclosure_hash       TEXT NOT NULL,       -- sha256 of the disclosure text the operator saw
    scope_db_path         TEXT NOT NULL,       -- the covered database (disclosure scope)
    consented_at          TEXT NOT NULL,
    revoked_at            TEXT NOT NULL DEFAULT '' -- non-empty once disabled/revoked
);
