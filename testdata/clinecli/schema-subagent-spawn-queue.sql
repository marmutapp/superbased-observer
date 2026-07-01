CREATE TABLE subagent_spawn_queue (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		root_session_id TEXT NOT NULL,
		parent_agent_id TEXT NOT NULL,
		task TEXT,
		system_prompt TEXT,
		created_at TEXT NOT NULL,
		consumed_at TEXT
	);
