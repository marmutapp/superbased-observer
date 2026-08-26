-- 086_launch_seeds.sql — pending {pid → future session} launch records.
--
-- THE GAP (docs/audits/process-attribution-coverage-audit-2026-07-15.md):
-- direct process attribution has historically been written by exactly one
-- producer per tool — the Claude Code SessionStart hook's ancestor walk, plus
-- later codex/cursor/hermes seeds. Every other `observer <tool>` launcher fell
-- through to the medium-confidence lazy CorrelateCrossOS pass, which is why
-- the session-detail process/network panels are blank for most tools
-- (cmd/observer/terminal_pidseed.go states this verbatim for terminals).
--
-- A pidbridge row cannot be written at spawn time: it requires a REAL session
-- id, and sessions are created by adapter ingestion of the tool's own storage
-- — unknowable before the child runs. So a launcher records the child pid
-- HERE at spawn (the only moment it is knowable), and the daemon's background
-- correlation sweep consumes the seed the moment a matching session is
-- ingested, writing the authoritative HIGH-confidence session_pid_bridge row
-- then. Ownership is one-directional and bounded:
--
--   - The LAUNCHER only ever INSERTS (at spawn). It does NOT retract on
--     child exit — live-verified 2026-08-21: a headless launch exits seconds
--     after spawn, long before the 90s sweep tick, so an exit-retract deleted
--     seeds before they could ever be consumed. A pending seed is not
--     identity (no session id; readers treat it as a hint), so leaving one
--     behind is harmless.
--   - CONSUMPTION is one atomic DELETE by the daemon sweep (claim-and-delete):
--     the winner writes the bridge row, whose later lifecycle is the standard
--     pidbridge prune — the same retention posture as hook-written rows.
--   - Unconsumed rows are expired by the sweep after 1h (a SIGKILLed launcher
--     or a launch that never produced a session leaves nothing durable).
--
-- The sweep NEVER overwrites an existing session_pid_bridge row for a pid:
-- hook-written rows (claude-code/codex/cursor/hermes) stay byte-authoritative.
--
-- NODE-LOCAL by construction: launch_seeds feeds only session_pid_bridge,
-- which is pinned out of the org-push wire in tests/invariant/privacy_test.go.
-- No wire surface here either.

CREATE TABLE IF NOT EXISTS launch_seeds (
    pid        INTEGER PRIMARY KEY,
    tool       TEXT NOT NULL,
    cwd        TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_launch_seeds_updated_at
    ON launch_seeds(updated_at);
