-- 004_session_pid_bridge.sql — {pid → session_id} bridge for the proxy.
--
-- Claude Code does not add an X-Session-Id header to API requests, so until
-- now most api_turns.session_id values landed NULL. The SessionStart hook
-- writes one row here per Claude Code session (pid = the Claude Code
-- process that spawned the hook, obtained via os.Getppid()); the proxy
-- resolves an incoming TCP connection's client pid → ancestor pid → this
-- table to attribute the turn. See internal/pidbridge and spec §9 / §14.
--
-- pid is the PRIMARY KEY because Linux PIDs are process-unique at any
-- moment. The pruner (internal/pidbridge.Store.Prune) deletes rows older
-- than a TTL to cope with PID reuse after long-running installs.

CREATE TABLE IF NOT EXISTS session_pid_bridge (
    pid         INTEGER PRIMARY KEY,
    session_id  TEXT NOT NULL,
    tool        TEXT NOT NULL,
    cwd         TEXT,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_session_pid_bridge_updated_at
    ON session_pid_bridge(updated_at);

CREATE INDEX IF NOT EXISTS idx_session_pid_bridge_session_id
    ON session_pid_bridge(session_id);
