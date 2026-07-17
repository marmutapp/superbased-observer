-- 063_remote_audit.sql — the remote-dashboard-access audit log
-- (docs/plans/remote-dashboard-access-plan-2026-07-12.md §4.8).
--
-- NODE-LOCAL, metadata-only. One row per remote-access event, typed by kind:
--   http_request         — a request arrived on the remotely-exposed listener
--   session_paired       — a device session was minted via /api/remote/pair
--   session_revoked      — logout / rotation dropped a session
--   auth_failed          — a pairing/credential check was rejected (rate-limited)
--   ws_attach            — a websocket attached (view stream)
--   execute_action       — a single-use execute capability was consumed
--
-- Records SESSION IDS, never secrets (plan §4.8): the pairing secret, session
-- cookies, CSRF tokens, and execute-capability tokens NEVER land here. `detail`
-- is a short, bounded, non-sensitive descriptor (route path, host, decision).
--
-- HONESTY (plan §4.8): this is NOT compliance-grade immutable — a local owner
-- can mutate the SQLite file. A hash chain / external sink would be needed for
-- tamper-evidence; that is deliberately out of scope for v1 and documented as a
-- residual, not over-claimed.
--
-- This table MUST NOT leave the machine: pinned in
-- tests/invariant/privacy_test.go (forbidden-table sentinel) and excluded from
-- internal/store/orgpush.go by construction (orgpush names an explicit table
-- allow-list; this is never in it). No paired orgserver migration exists, by
-- design — same posture as the cachetrack / limit_snapshots / benchmark_*
-- node-local tables.

CREATE TABLE IF NOT EXISTS remote_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT NOT NULL,               -- RFC3339 UTC
    kind        TEXT NOT NULL,               -- event kind (see header)
    session_id  TEXT NOT NULL DEFAULT '',    -- device-session id (NOT a secret) or ''
    principal   TEXT NOT NULL DEFAULT '',    -- resolved capability: public|view|execute|anonymous
    remote_addr TEXT NOT NULL DEFAULT '',    -- best-effort peer address (host only)
    route       TEXT NOT NULL DEFAULT '',    -- matched route pattern / path
    decision    TEXT NOT NULL DEFAULT '',    -- allow|deny|ok|fail
    detail      TEXT NOT NULL DEFAULT ''     -- short bounded non-sensitive descriptor
);

CREATE INDEX IF NOT EXISTS idx_remote_audit_ts ON remote_audit(ts);
CREATE INDEX IF NOT EXISTS idx_remote_audit_kind ON remote_audit(kind);
