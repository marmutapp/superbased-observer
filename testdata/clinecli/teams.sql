CREATE TABLE team_store_schema_version (
				lock INTEGER PRIMARY KEY CHECK (lock = 1),
				version INTEGER NOT NULL
			);
CREATE TABLE team_events (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				team_name TEXT NOT NULL,
				ts TEXT NOT NULL,
				event_type TEXT NOT NULL,
				payload_json TEXT NOT NULL,
				causation_id TEXT,
				correlation_id TEXT
			);
CREATE TABLE sqlite_sequence(name,seq);
CREATE INDEX idx_team_events_name_ts
				ON team_events(team_name, ts DESC);
CREATE TABLE team_runtime_snapshot (
				team_name TEXT PRIMARY KEY,
				state_json TEXT NOT NULL,
				teammates_json TEXT NOT NULL,
				updated_at TEXT NOT NULL
			);
CREATE TABLE team_tasks (
				team_name TEXT NOT NULL,
				task_id TEXT NOT NULL,
				title TEXT NOT NULL,
				description TEXT NOT NULL,
				status TEXT NOT NULL,
				assignee TEXT,
				depends_on_json TEXT NOT NULL,
				summary TEXT,
				version INTEGER NOT NULL DEFAULT 1,
				updated_at TEXT NOT NULL,
				PRIMARY KEY(team_name, task_id)
			);
CREATE TABLE team_runs (
				team_name TEXT NOT NULL,
				run_id TEXT NOT NULL,
				agent_id TEXT NOT NULL,
				task_id TEXT,
				status TEXT NOT NULL,
				message TEXT NOT NULL,
				started_at TEXT,
				ended_at TEXT,
				error TEXT,
				lease_owner TEXT,
				heartbeat_at TEXT,
				version INTEGER NOT NULL DEFAULT 1,
				PRIMARY KEY(team_name, run_id)
			);
CREATE INDEX idx_team_runs_status
				ON team_runs(team_name, status);
CREATE TABLE team_outcomes (
				team_name TEXT NOT NULL,
				outcome_id TEXT NOT NULL,
				title TEXT NOT NULL,
				status TEXT NOT NULL,
				schema_json TEXT NOT NULL,
				finalized_at TEXT,
				version INTEGER NOT NULL DEFAULT 1,
				PRIMARY KEY(team_name, outcome_id)
			);
CREATE TABLE team_outcome_fragments (
				team_name TEXT NOT NULL,
				outcome_id TEXT NOT NULL,
				fragment_id TEXT NOT NULL,
				section TEXT NOT NULL,
				source_agent_id TEXT NOT NULL,
				source_run_id TEXT,
				content TEXT NOT NULL,
				status TEXT NOT NULL,
				reviewed_by TEXT,
				reviewed_at TEXT,
				version INTEGER NOT NULL DEFAULT 1,
				PRIMARY KEY(team_name, fragment_id)
			);
