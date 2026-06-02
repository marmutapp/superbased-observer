-- 001_initial.sql — initial schema for SuperBased Observer (spec §6.2).

CREATE TABLE IF NOT EXISTS schema_meta (
    key     TEXT PRIMARY KEY,
    value   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    root_path       TEXT NOT NULL,
    git_remote      TEXT,
    name            TEXT,
    created_at      TEXT NOT NULL,
    last_session_at TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_root ON projects(root_path);

CREATE TABLE IF NOT EXISTS sessions (
    id                  TEXT PRIMARY KEY,
    project_id          INTEGER NOT NULL REFERENCES projects(id),
    tool                TEXT NOT NULL,
    model               TEXT,
    git_branch          TEXT,
    started_at          TEXT NOT NULL,
    ended_at            TEXT,
    total_actions       INTEGER DEFAULT 0,
    metadata            TEXT,
    quality_score       REAL,
    redundancy_ratio    REAL,
    error_rate          REAL,
    onboarding_cost     INTEGER,
    turns_to_first_edit INTEGER,
    retry_cost_tokens   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_sessions_project ON sessions(project_id);
CREATE INDEX IF NOT EXISTS idx_sessions_tool ON sessions(tool);
CREATE INDEX IF NOT EXISTS idx_sessions_started ON sessions(started_at);

CREATE TABLE IF NOT EXISTS actions (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id          TEXT NOT NULL REFERENCES sessions(id),
    project_id          INTEGER NOT NULL REFERENCES projects(id),
    timestamp           TEXT NOT NULL,
    turn_index          INTEGER,

    action_type         TEXT NOT NULL,
    is_native_tool      INTEGER DEFAULT 0,

    target              TEXT,
    target_hash         TEXT,

    success             INTEGER DEFAULT 1,
    error_message       TEXT,

    duration_ms         INTEGER,

    content_hash        TEXT,
    file_mtime          TEXT,
    file_size_bytes     INTEGER,
    freshness           TEXT,
    prior_action_id     INTEGER,
    change_detected     INTEGER DEFAULT 0,

    preceding_reasoning TEXT,

    raw_tool_name       TEXT,
    raw_tool_input      TEXT,

    tool                TEXT NOT NULL,

    source_file         TEXT,
    source_event_id     TEXT,
    UNIQUE(source_file, source_event_id)
);
CREATE INDEX IF NOT EXISTS idx_actions_session ON actions(session_id);
CREATE INDEX IF NOT EXISTS idx_actions_project ON actions(project_id);
CREATE INDEX IF NOT EXISTS idx_actions_type ON actions(action_type);
CREATE INDEX IF NOT EXISTS idx_actions_target_hash ON actions(target_hash);
CREATE INDEX IF NOT EXISTS idx_actions_timestamp ON actions(timestamp);
CREATE INDEX IF NOT EXISTS idx_actions_freshness ON actions(freshness);

CREATE TABLE IF NOT EXISTS file_state (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id       INTEGER NOT NULL REFERENCES projects(id),
    file_path        TEXT NOT NULL,
    content_hash     TEXT NOT NULL,
    file_mtime       TEXT NOT NULL,
    file_size_bytes  INTEGER NOT NULL,
    last_action_id   INTEGER REFERENCES actions(id),
    last_action_type TEXT NOT NULL,
    last_seen_at     TEXT NOT NULL,
    last_modified_by TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_file_state_project_path ON file_state(project_id, file_path);

CREATE TABLE IF NOT EXISTS token_usage (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id              TEXT NOT NULL REFERENCES sessions(id),
    timestamp               TEXT NOT NULL,
    tool                    TEXT NOT NULL,
    model                   TEXT,
    input_tokens            INTEGER,
    output_tokens           INTEGER,
    cache_read_tokens       INTEGER,
    cache_creation_tokens   INTEGER,
    reasoning_tokens        INTEGER,
    estimated_cost_usd      REAL,
    source                  TEXT NOT NULL,
    reliability             TEXT DEFAULT 'unknown',
    source_file             TEXT,
    source_event_id         TEXT,
    UNIQUE(source_file, source_event_id)
);
CREATE INDEX IF NOT EXISTS idx_token_usage_session ON token_usage(session_id);

CREATE TABLE IF NOT EXISTS api_turns (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id              TEXT,
    project_id              INTEGER REFERENCES projects(id),
    timestamp               TEXT NOT NULL,
    provider                TEXT NOT NULL,
    model                   TEXT NOT NULL,
    request_id              TEXT,
    input_tokens            INTEGER NOT NULL,
    output_tokens           INTEGER NOT NULL,
    cache_read_tokens       INTEGER,
    cache_creation_tokens   INTEGER,
    cost_usd                REAL,
    message_count           INTEGER,
    tool_use_count          INTEGER,
    system_prompt_hash      TEXT,
    time_to_first_token_ms  INTEGER,
    total_response_ms       INTEGER,
    stop_reason             TEXT
);
CREATE INDEX IF NOT EXISTS idx_api_turns_session ON api_turns(session_id);
CREATE INDEX IF NOT EXISTS idx_api_turns_timestamp ON api_turns(timestamp);

CREATE TABLE IF NOT EXISTS failure_context (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    action_id            INTEGER NOT NULL REFERENCES actions(id),
    session_id           TEXT NOT NULL REFERENCES sessions(id),
    project_id           INTEGER NOT NULL REFERENCES projects(id),
    timestamp            TEXT NOT NULL,
    command_hash         TEXT NOT NULL,
    command_summary      TEXT NOT NULL,
    exit_code            INTEGER,
    error_category       TEXT,
    error_message        TEXT,
    retry_count          INTEGER DEFAULT 0,
    eventually_succeeded INTEGER DEFAULT 0,
    tee_file_path        TEXT
);
CREATE INDEX IF NOT EXISTS idx_failure_context_command ON failure_context(command_hash);
CREATE INDEX IF NOT EXISTS idx_failure_context_session ON failure_context(session_id);

-- Standalone FTS5 table (not external-content) — we store the excerpts here.
-- action_id is tokenized as a regular column so callers can match against it
-- if they want; primary use is via the rowid alongside the actions table.
CREATE VIRTUAL TABLE IF NOT EXISTS action_excerpts USING fts5(
    action_id UNINDEXED,
    tool_name,
    target,
    excerpt,
    error_message
);

CREATE TABLE IF NOT EXISTS compaction_events (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id          TEXT NOT NULL REFERENCES sessions(id),
    project_id          INTEGER NOT NULL REFERENCES projects(id),
    timestamp           TEXT NOT NULL,
    tool                TEXT NOT NULL,
    pre_action_count    INTEGER,
    file_state_snapshot TEXT,
    ghost_files_after   TEXT
);

CREATE TABLE IF NOT EXISTS project_patterns (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id           INTEGER NOT NULL REFERENCES projects(id),
    pattern_type         TEXT NOT NULL,
    pattern_data         TEXT NOT NULL,
    confidence           REAL,
    last_reinforced_at   TEXT,
    decay_half_life_days INTEGER DEFAULT 30,
    observation_count    INTEGER,
    source_tools         TEXT,
    source_session_count INTEGER
);

CREATE TABLE IF NOT EXISTS observer_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   TEXT NOT NULL,
    level       TEXT NOT NULL,
    component   TEXT NOT NULL,
    message     TEXT NOT NULL,
    details     TEXT
);
CREATE INDEX IF NOT EXISTS idx_observer_log_level ON observer_log(level);

CREATE TABLE IF NOT EXISTS parse_cursors (
    source_file TEXT PRIMARY KEY,
    byte_offset INTEGER NOT NULL DEFAULT 0,
    last_parsed TEXT NOT NULL
);
