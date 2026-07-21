CREATE TABLE sessions (
		session_id TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		pid INTEGER NOT NULL,
		started_at TEXT NOT NULL,
		ended_at TEXT,
		exit_code INTEGER,
		status TEXT NOT NULL,
		status_lock INTEGER NOT NULL DEFAULT 0,
		interactive INTEGER NOT NULL,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		cwd TEXT NOT NULL,
		workspace_root TEXT NOT NULL,
		team_name TEXT,
		enable_tools INTEGER NOT NULL,
		enable_spawn INTEGER NOT NULL,
		enable_teams INTEGER NOT NULL,
		parent_session_id TEXT,
		parent_agent_id TEXT,
		agent_id TEXT,
		conversation_id TEXT,
		is_subagent INTEGER NOT NULL DEFAULT 0,
		prompt TEXT,
		metadata_json TEXT,
		transcript_path TEXT NOT NULL DEFAULT '',
		hook_path TEXT NOT NULL,
		messages_path TEXT,
		updated_at TEXT NOT NULL
	);
CREATE TABLE subagent_spawn_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_session_id TEXT NOT NULL,
		parent_agent_id TEXT NOT NULL,
		task TEXT,
		system_prompt TEXT,
		created_at TEXT NOT NULL,
		consumed_at TEXT
	);
CREATE TABLE sqlite_sequence(name,seq);
CREATE TABLE schedules (
		schedule_id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		cron_pattern TEXT NOT NULL,
		prompt TEXT NOT NULL,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		mode TEXT NOT NULL DEFAULT 'act',
		workspace_root TEXT,
		cwd TEXT,
		system_prompt TEXT,
		max_iterations INTEGER,
		timeout_seconds INTEGER,
		max_parallel INTEGER NOT NULL DEFAULT 1,
		enabled INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		last_run_at TEXT,
		next_run_at TEXT,
		claim_token TEXT,
		claim_started_at TEXT,
		claim_until_at TEXT,
		created_by TEXT,
		tags TEXT,
		metadata_json TEXT
	);
CREATE TABLE schedule_executions (
		execution_id TEXT PRIMARY KEY,
		schedule_id TEXT NOT NULL,
		session_id TEXT,
		triggered_at TEXT NOT NULL,
		started_at TEXT,
		ended_at TEXT,
		status TEXT NOT NULL,
		exit_code INTEGER,
		error_message TEXT,
		iterations INTEGER,
		tokens_used INTEGER,
		cost_usd REAL,
		FOREIGN KEY (schedule_id) REFERENCES schedules(schedule_id) ON DELETE CASCADE,
		FOREIGN KEY (session_id) REFERENCES sessions(session_id) ON DELETE SET NULL
	);
CREATE INDEX idx_schedule_executions_schedule
	ON schedule_executions(schedule_id, triggered_at DESC);
CREATE INDEX idx_schedules_next_run
	ON schedules(enabled, next_run_at);
