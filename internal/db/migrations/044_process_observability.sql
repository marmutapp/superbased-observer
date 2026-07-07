-- 044_process_observability.sql — OS-level process capture tables
-- (docs/process-observability.md §10). Optional, opt-in feature; these
-- tables stay empty unless [observer.process] enabled = true and a
-- process backend is running.
--
-- Two tables: one row per observed process run, one row per high-signal
-- process event (network/file/privilege/etc — populated from P5 onward).
--
-- Privacy posture: NODE-LOCAL. process_runs / process_events MUST NOT
-- appear in internal/store/orgpush.go::SelectUnpushedSince — they are
-- pinned in tests/invariant/privacy_test.go alongside the cachetrack and
-- router-decision tables. They carry scrubbed/capped argv previews, path
-- and argv hashes, env posture (allowlist only), and compact
-- cgroup/container/namespace identifiers — never file contents, command
-- output, network payloads, or full environment. No paired
-- internal/orgserver/db/migrations/ migration: node-local tables never
-- reach the server.
--
-- Identity: rows are keyed by process_key = sha256(boot_id:pid:start_time)
-- (docs/process-observability.md §9.3), NOT pid alone — Linux PIDs are
-- reused. pid/ppid are kept as raw numerics for the tree view but never
-- joined on in isolation.
--
-- Attribution (§9): session_id/project_id/tool come from the pidbridge
-- seed + descendant inheritance. action_id/turn_index are the §9.2.4
-- refinement — filled by the deferred run_command -> process_exec
-- correlation pass (the `actions` row may be ingested AFTER the process
-- event arrives, so the link is resolved later, not at exec time). Both
-- nullable; a process attributed only to a session is valid.

CREATE TABLE IF NOT EXISTS process_runs (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    process_key              TEXT NOT NULL UNIQUE,
    boot_id                  TEXT,
    pid                      INTEGER NOT NULL,
    ppid                     INTEGER,
    start_time_ticks         INTEGER,
    parent_process_key       TEXT,
    session_id               TEXT REFERENCES sessions(id),
    project_id               INTEGER REFERENCES projects(id),
    tool                     TEXT,
    action_id                INTEGER REFERENCES actions(id),
    turn_index               INTEGER,
    attribution_source       TEXT NOT NULL,
    attribution_confidence   TEXT NOT NULL,
    exe_path                 TEXT,
    exe_basename             TEXT,
    exe_device               TEXT,
    exe_inode                TEXT,
    exe_hash                 TEXT,
    cwd                      TEXT,
    argv_preview             TEXT,
    argv_hash                TEXT,
    argv_argc                INTEGER,
    uid                      INTEGER,
    gid                      INTEGER,
    euid                     INTEGER,
    egid                     INTEGER,
    username                 TEXT,
    cgroup_hash              TEXT,
    container_id             TEXT,
    pid_namespace            TEXT,
    mount_namespace          TEXT,
    net_namespace            TEXT,
    seccomp_mode             TEXT,
    apparmor_label           TEXT,
    selinux_label            TEXT,
    capabilities_eff         TEXT,
    env_posture_json         TEXT,
    started_at               TEXT NOT NULL,
    last_seen_at             TEXT NOT NULL,
    exited_at                TEXT,
    exit_code                INTEGER,
    exit_signal              INTEGER,
    duration_ms              INTEGER,
    cpu_user_ms              INTEGER,
    cpu_system_ms            INTEGER,
    max_rss_bytes            INTEGER,
    read_bytes               INTEGER,
    write_bytes              INTEGER,
    metadata_json            TEXT
);

CREATE INDEX IF NOT EXISTS idx_process_runs_session
    ON process_runs(session_id, started_at);

CREATE INDEX IF NOT EXISTS idx_process_runs_project
    ON process_runs(project_id, started_at);

CREATE INDEX IF NOT EXISTS idx_process_runs_pid
    ON process_runs(pid, started_at);

CREATE INDEX IF NOT EXISTS idx_process_runs_action
    ON process_runs(action_id);

CREATE TABLE IF NOT EXISTS process_events (
    id                       INTEGER PRIMARY KEY AUTOINCREMENT,
    process_run_id           INTEGER REFERENCES process_runs(id),
    process_key              TEXT NOT NULL,
    timestamp                TEXT NOT NULL,
    event_type               TEXT NOT NULL,
    session_id               TEXT REFERENCES sessions(id),
    project_id               INTEGER REFERENCES projects(id),
    tool                     TEXT,
    action_id                INTEGER REFERENCES actions(id),
    turn_index               INTEGER,
    target_kind              TEXT,
    target                   TEXT,
    target_hash              TEXT,
    severity                 TEXT,
    finding_rule_id          TEXT,
    details_json             TEXT
);

CREATE INDEX IF NOT EXISTS idx_process_events_session
    ON process_events(session_id, timestamp);

CREATE INDEX IF NOT EXISTS idx_process_events_type
    ON process_events(event_type, timestamp);
