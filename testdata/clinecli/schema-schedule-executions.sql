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
