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
CREATE INDEX idx_schedules_next_run
	ON schedules(enabled, next_run_at);
