CREATE TABLE cron_specs (
		spec_id TEXT PRIMARY KEY,
		external_id TEXT NOT NULL,
		source_path TEXT NOT NULL UNIQUE,
		trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('one_off', 'schedule', 'event')),
		source_mtime_ms INTEGER,
		source_hash TEXT,
		parse_status TEXT NOT NULL CHECK (parse_status IN ('valid', 'invalid')),
		parse_error TEXT,
		enabled INTEGER NOT NULL DEFAULT 1,
		removed INTEGER NOT NULL DEFAULT 0,
		title TEXT NOT NULL,
		prompt TEXT,
		workspace_root TEXT,
		schedule_expr TEXT,
		timezone TEXT,
		event_type TEXT,
		filters_json TEXT,
		debounce_seconds INTEGER,
		dedupe_window_seconds INTEGER,
		cooldown_seconds INTEGER,
		mode TEXT,
		system_prompt TEXT,
		provider_id TEXT,
		model_id TEXT,
		max_iterations INTEGER,
		timeout_seconds INTEGER,
		max_parallel INTEGER,
		tools_json TEXT,
		notes_directory TEXT,
		extensions_json TEXT,
		source TEXT,
		tags_json TEXT,
		metadata_json TEXT,
		revision INTEGER NOT NULL DEFAULT 1,
		last_materialized_run_id TEXT,
		last_run_at TEXT,
		next_run_at TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
CREATE TABLE cron_runs (
		run_id TEXT PRIMARY KEY,
		spec_id TEXT NOT NULL REFERENCES cron_specs(spec_id) ON DELETE CASCADE,
		spec_revision INTEGER NOT NULL,
		trigger_kind TEXT NOT NULL CHECK (trigger_kind IN ('one_off', 'schedule', 'event', 'manual', 'retry')),
		status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'done', 'failed', 'cancelled')),
		claim_token TEXT,
		claim_started_at TEXT,
		claim_until_at TEXT,
		scheduled_for TEXT,
		trigger_event_id TEXT,
		started_at TEXT,
		completed_at TEXT,
		session_id TEXT,
		report_path TEXT,
		error TEXT,
		attempt_count INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
CREATE TABLE cron_event_log (
		event_id TEXT PRIMARY KEY,
		event_type TEXT NOT NULL,
		source TEXT NOT NULL,
		subject TEXT,
		occurred_at TEXT NOT NULL,
		received_at TEXT NOT NULL,
		workspace_root TEXT,
		dedupe_key TEXT,
		payload_json TEXT,
		attributes_json TEXT,
		processing_status TEXT NOT NULL DEFAULT 'received'
			CHECK (processing_status IN ('received', 'unmatched', 'queued', 'suppressed', 'failed')),
		matched_spec_count INTEGER NOT NULL DEFAULT 0,
		queued_run_count INTEGER NOT NULL DEFAULT 0,
		suppressed_count INTEGER NOT NULL DEFAULT 0,
		error TEXT,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
CREATE INDEX cron_runs_claimable_idx
		ON cron_runs(status, scheduled_for, claim_until_at);
CREATE INDEX cron_runs_spec_idx
		ON cron_runs(spec_id, created_at DESC);
CREATE INDEX cron_runs_trigger_event_idx
		ON cron_runs(trigger_event_id);
CREATE INDEX cron_runs_event_spec_status_idx
		ON cron_runs(spec_id, trigger_kind, status, scheduled_for);
CREATE INDEX cron_event_log_type_idx
		ON cron_event_log(event_type, received_at DESC);
CREATE INDEX cron_event_log_received_idx
		ON cron_event_log(received_at DESC);
CREATE INDEX cron_event_log_dedupe_idx
		ON cron_event_log(event_type, source, dedupe_key, received_at DESC);
CREATE INDEX cron_specs_next_run_idx
		ON cron_specs(trigger_kind, enabled, next_run_at);
CREATE INDEX cron_specs_event_match_idx
		ON cron_specs(trigger_kind, event_type, enabled);
CREATE INDEX cron_specs_parse_status_idx
		ON cron_specs(parse_status, updated_at DESC);
CREATE INDEX cron_specs_source_path_idx
		ON cron_specs(source_path);
CREATE UNIQUE INDEX cron_runs_one_off_active_idx
		ON cron_runs(spec_id, spec_revision)
		WHERE trigger_kind = 'one_off';
