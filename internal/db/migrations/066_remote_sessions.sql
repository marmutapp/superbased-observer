-- 066_remote_sessions.sql — persist remote device sessions across daemon restart
-- (docs/plans/persist-remote-sessions-plan-2026-07-14.md).
--
-- WHY: internal/remoteauth.SessionStore was in-memory only, so the dashboard
-- self-restart (self re-exec, 8c3fab9e) started a fresh process with an empty
-- session map — every paired device's cookie was orphaned → 401. These two
-- NODE-LOCAL tables let a paired phone survive a restart while preserving the
-- security invariant: once revoke/rotate/disable/TTL/idle expiry is durably
-- observed, no later restart may re-validate that cookie.
--
-- remote_sessions       — one row per live device session. id_hash is the
--                         sha256hex of the bearer cookie; the RAW token is NEVER
--                         stored, so a leaked observer.db yields no usable
--                         cookies. gen fences each row to a generation.
-- remote_session_state  — single row holding the current DURABLE generation.
--                         Rotate (dashboard terminate-all) and the CLI
--                         disable/rotate durable fence advance it + clear rows
--                         in one transaction; on the next daemon start LoadAll
--                         reads this gen and drops any row of a superseded gen.
--
-- HONESTY: session ids are hashed, but the fence is not tamper-evident against a
-- local owner who can mutate the SQLite file directly — same residual as
-- remote_audit; documented, not over-claimed.
--
-- NODE-LOCAL: both tables MUST NOT leave the machine. Pinned in
-- tests/invariant/privacy_test.go (forbidden-table sentinel) and excluded from
-- internal/store/orgpush.go by construction (orgpush names an explicit table
-- allow-list; these are never in it). No paired orgserver migration exists, by
-- design — same posture as remote_audit / limit_snapshots / terminal_run.

CREATE TABLE IF NOT EXISTS remote_sessions (
    id_hash    TEXT PRIMARY KEY,   -- sha256hex of the bearer cookie (NOT the raw token)
    gen        INTEGER NOT NULL,   -- generation this session belongs to
    created_at INTEGER NOT NULL,   -- unix nanoseconds UTC
    last_seen  INTEGER NOT NULL    -- unix nanoseconds UTC (idle clock)
);

CREATE TABLE IF NOT EXISTS remote_session_state (
    id  INTEGER PRIMARY KEY CHECK (id = 1),  -- single-row table
    gen INTEGER NOT NULL                     -- current durable generation
);

-- Seed the durable generation at 1 so LoadAll always finds a row.
INSERT OR IGNORE INTO remote_session_state (id, gen) VALUES (1, 1);
