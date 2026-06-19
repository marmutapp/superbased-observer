package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
)

// Foreign-database import/merge (usability arc P6.9). Merges another
// observer.db — a stranded second install, a pre-migration machine,
// a split-brain era Windows-side DB — into this one, idempotently.
//
// Mechanics: the foreign file is ATTACHed read-only on one pinned
// connection and every table moves with set-based INSERT … SELECT.
// Idempotency leans on the schema's own keys wherever they exist —
// actions and token_usage carry UNIQUE(source_file, source_event_id),
// sessions and claudecode_effort have natural primary keys, projects
// are unique on root_path — with NOT EXISTS identity heuristics for
// the keyless cases (api_turns; legacy rows with NULL
// source_event_id). Running the same import twice inserts nothing.
//
// Scope: projects, sessions, actions, token_usage, api_turns,
// claudecode_effort, failure_context (+ FTS5 excerpts re-indexed for
// imported actions). NODE-LOCAL tables stay put by design: cache_*
// (privacy-pinned), session_pid_bridge (foreign PIDs are meaningless
// here), parse_cursors / file_state (per-node caches), advisor state.

// ImportOptions configures ImportFrom.
type ImportOptions struct {
	// DryRun runs the whole merge inside a transaction and rolls it
	// back — the returned counts are exact, nothing lands.
	DryRun bool
	// Indexer, when non-nil, re-indexes imported actions' tool output
	// into the FTS5 search table after commit (skipped on dry-run).
	Indexer *indexing.Indexer
}

// ImportTableResult is one table's merge outcome: rows inserted this
// run vs the foreign table's total (the difference already existed
// locally or failed an identity guard).
type ImportTableResult struct {
	Inserted int64 `json:"inserted"`
	Foreign  int64 `json:"foreign_rows"`
}

// ImportResult reports per-table outcomes plus the FTS excerpt count.
type ImportResult struct {
	Projects        ImportTableResult `json:"projects"`
	Sessions        ImportTableResult `json:"sessions"`
	Actions         ImportTableResult `json:"actions"`
	TokenUsage      ImportTableResult `json:"token_usage"`
	APITurns        ImportTableResult `json:"api_turns"`
	Effort          ImportTableResult `json:"claudecode_effort"`
	FailureContext  ImportTableResult `json:"failure_context"`
	ExcerptsIndexed int64             `json:"excerpts_indexed"`
}

// ImportFrom merges the observer database at foreignPath into this
// store. The foreign file must already be at the current schema —
// the CLI opens it through db.Open (which migrates) before calling
// this. All inserts run in one transaction: any failure imports
// nothing.
func (s *Store) ImportFrom(ctx context.Context, foreignPath string, opts ImportOptions) (ImportResult, error) {
	var res ImportResult

	// Pin one connection: ATTACH is per-connection state, and the
	// pool would otherwise route statements elsewhere.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return res, fmt.Errorf("store.ImportFrom: conn: %w", err)
	}
	defer conn.Close()

	// Read-only attach — the import must never write the source.
	srcURI := "file:" + filepath.ToSlash(foreignPath) + "?mode=ro"
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS src", srcURI); err != nil {
		return res, fmt.Errorf("store.ImportFrom: attach %s: %w", foreignPath, err)
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "DETACH DATABASE src") //nolint:errcheck // best-effort detach on a closing conn

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("store.ImportFrom: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	steps := []struct {
		dst   *ImportTableResult
		table string
		stmts []string
	}{
		{&res.Projects, "projects", []string{insertProjectsSQL}},
		{&res.Sessions, "sessions", []string{insertSessionsSQL}},
		{&res.Actions, "actions", []string{insertActionsKeyedSQL, insertActionsLegacySQL}},
		{&res.TokenUsage, "token_usage", []string{insertTokenKeyedSQL, insertTokenLegacySQL}},
		{&res.APITurns, "api_turns", []string{insertAPITurnsSQL}},
		{&res.Effort, "claudecode_effort", []string{insertEffortSQL}},
		{&res.FailureContext, "failure_context", []string{insertFailuresSQL}},
	}
	for _, st := range steps {
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM src."+st.table).Scan(&st.dst.Foreign); err != nil {
			return res, fmt.Errorf("store.ImportFrom: count src.%s: %w", st.table, err)
		}
		for _, stmt := range st.stmts {
			r, err := tx.ExecContext(ctx, stmt)
			if err != nil {
				return res, fmt.Errorf("store.ImportFrom: merge %s: %w", st.table, err)
			}
			n, _ := r.RowsAffected()
			st.dst.Inserted += n
		}
	}

	if opts.DryRun {
		// The deferred rollback undoes everything; counts stay exact.
		return res, nil
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("store.ImportFrom: commit: %w", err)
	}
	committed = true

	if opts.Indexer != nil {
		n, err := s.indexImportedExcerpts(ctx, conn, opts.Indexer)
		if err != nil {
			// The merge itself landed; report the indexing failure
			// without unwinding it.
			return res, fmt.Errorf("store.ImportFrom: rows merged, but excerpt indexing failed: %w", err)
		}
		res.ExcerptsIndexed = n
	}
	return res, nil
}

// indexImportedExcerpts pushes tool output of newly-imported actions
// into the FTS5 search table. Runs on the pinned (still-attached)
// connection to find the imported set; the writes go through the
// Indexer's own handle (local tables only).
func (s *Store) indexImportedExcerpts(ctx context.Context, conn *sql.Conn, ix *indexing.Indexer) (int64, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT a.id, COALESCE(a.raw_tool_name, ''), COALESCE(a.target, ''),
		       a.raw_tool_output, COALESCE(a.error_message, '')
		  FROM actions a
		  JOIN src.actions sa
		    ON sa.source_file = a.source_file AND sa.source_event_id = a.source_event_id
		 WHERE COALESCE(a.raw_tool_output, '') != ''
		   AND NOT EXISTS (SELECT 1 FROM action_excerpts e WHERE e.action_id = a.id)`)
	if err != nil {
		return 0, fmt.Errorf("select imported outputs: %w", err)
	}
	type row struct {
		id                     int64
		tool, target, raw, msg string
	}
	var pending []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.tool, &r.target, &r.raw, &r.msg); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan imported output: %w", err)
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	var n int64
	for _, r := range pending {
		if err := ix.Index(ctx, r.id, r.tool, r.target, r.raw, r.msg); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// remapProject resolves a foreign project id to the local id through
// the stable root_path key. Used inline by the insert statements.
const remapProject = `(SELECT p.id FROM projects p JOIN src.projects sp ON sp.root_path = p.root_path WHERE sp.id = %s)`

var (
	insertProjectsSQL = `
		INSERT OR IGNORE INTO projects
			(root_path, git_remote, name, created_at, last_session_at, root_path_hash, git_remote_hash)
		SELECT root_path, git_remote, name, created_at, last_session_at, root_path_hash, git_remote_hash
		  FROM src.projects`

	insertSessionsSQL = fmt.Sprintf(`
		INSERT OR IGNORE INTO sessions
			(id, project_id, tool, model, git_branch, started_at, ended_at, total_actions,
			 metadata, quality_score, redundancy_ratio, error_rate, onboarding_cost,
			 turns_to_first_edit, retry_cost_tokens, summary_md, org_id, user_email,
			 stale_reads_wasteful, stale_reads_necessary, redundancy_ratio_wasteful)
		SELECT ss.id, `+remapProject+`, ss.tool, ss.model, ss.git_branch, ss.started_at,
		       ss.ended_at, ss.total_actions, ss.metadata, ss.quality_score,
		       ss.redundancy_ratio, ss.error_rate, ss.onboarding_cost,
		       ss.turns_to_first_edit, ss.retry_cost_tokens, ss.summary_md, ss.org_id,
		       ss.user_email, ss.stale_reads_wasteful, ss.stale_reads_necessary,
		       ss.redundancy_ratio_wasteful
		  FROM src.sessions ss
		 WHERE `+remapProject+` IS NOT NULL`, "ss.project_id", "ss.project_id")

	// actionCols/actionSels: prior_action_id is forced NULL — it
	// points at a row id in the FOREIGN database, which means nothing
	// here.
	actionCols = `(session_id, project_id, timestamp, turn_index, action_type, is_native_tool,
			 target, target_hash, success, error_message, duration_ms, content_hash,
			 file_mtime, file_size_bytes, freshness, prior_action_id, change_detected,
			 preceding_reasoning, raw_tool_name, raw_tool_input, tool, source_file,
			 source_event_id, is_sidechain, message_id, metadata, raw_tool_output,
			 org_id, user_email, source_file_hash)`
	actionSels = fmt.Sprintf(`sa.session_id, `+remapProject+`, sa.timestamp, sa.turn_index,
			 sa.action_type, sa.is_native_tool, sa.target, sa.target_hash, sa.success,
			 sa.error_message, sa.duration_ms, sa.content_hash, sa.file_mtime,
			 sa.file_size_bytes, sa.freshness, NULL, sa.change_detected,
			 sa.preceding_reasoning, sa.raw_tool_name, sa.raw_tool_input, sa.tool,
			 sa.source_file, sa.source_event_id, sa.is_sidechain, sa.message_id,
			 sa.metadata, sa.raw_tool_output, sa.org_id, sa.user_email, sa.source_file_hash`,
		"sa.project_id")

	// The EXISTS(sessions) guard keeps FK violations out: OR IGNORE
	// suppresses UNIQUE conflicts but NOT foreign-key failures, and a
	// corrupt foreign row referencing a missing session would abort
	// the whole transaction.
	insertActionsKeyedSQL = fmt.Sprintf(`
		INSERT OR IGNORE INTO actions %s
		SELECT %s
		  FROM src.actions sa
		 WHERE sa.source_event_id IS NOT NULL
		   AND `+remapProject+` IS NOT NULL
		   AND EXISTS (SELECT 1 FROM sessions ls WHERE ls.id = sa.session_id)`,
		actionCols, actionSels, "sa.project_id")

	// Legacy rows without the idempotency key: identity falls back to
	// (session, timestamp, action_type, target).
	insertActionsLegacySQL = fmt.Sprintf(`
		INSERT INTO actions %s
		SELECT %s
		  FROM src.actions sa
		 WHERE sa.source_event_id IS NULL
		   AND `+remapProject+` IS NOT NULL
		   AND EXISTS (SELECT 1 FROM sessions ls WHERE ls.id = sa.session_id)
		   AND NOT EXISTS (SELECT 1 FROM actions a
		                    WHERE a.session_id = sa.session_id
		                      AND a.timestamp = sa.timestamp
		                      AND a.action_type = sa.action_type
		                      AND COALESCE(a.target, '') = COALESCE(sa.target, ''))`,
		actionCols, actionSels, "sa.project_id")

	//nolint:gosec // G101: SQL column list (token_usage columns), not credentials.
	tokenCols = `(session_id, timestamp, tool, model, input_tokens, output_tokens,
			 cache_read_tokens, cache_creation_tokens, reasoning_tokens,
			 estimated_cost_usd, source, reliability, source_file, source_event_id,
			 cache_creation_1h_tokens, message_id, web_search_requests, org_id,
			 user_email, turn_id, source_file_hash, fast)`
	//nolint:gosec // G101: SQL select list (token_usage columns), not credentials.
	tokenSels = `st.session_id, st.timestamp, st.tool, st.model, st.input_tokens,
			 st.output_tokens, st.cache_read_tokens, st.cache_creation_tokens,
			 st.reasoning_tokens, st.estimated_cost_usd, st.source, st.reliability,
			 st.source_file, st.source_event_id, st.cache_creation_1h_tokens,
			 st.message_id, st.web_search_requests, st.org_id, st.user_email,
			 st.turn_id, st.source_file_hash, st.fast`

	insertTokenKeyedSQL = fmt.Sprintf(`
		INSERT OR IGNORE INTO token_usage %s
		SELECT %s
		  FROM src.token_usage st
		 WHERE st.source_event_id IS NOT NULL
		   AND EXISTS (SELECT 1 FROM sessions ls WHERE ls.id = st.session_id)`, tokenCols, tokenSels)

	insertTokenLegacySQL = fmt.Sprintf(`
		INSERT INTO token_usage %s
		SELECT %s
		  FROM src.token_usage st
		 WHERE st.source_event_id IS NULL
		   AND EXISTS (SELECT 1 FROM sessions ls WHERE ls.id = st.session_id)
		   AND NOT EXISTS (SELECT 1 FROM token_usage u
		                    WHERE u.session_id = st.session_id
		                      AND u.timestamp = st.timestamp
		                      AND COALESCE(u.model, '') = COALESCE(st.model, '')
		                      AND COALESCE(u.input_tokens, -1) = COALESCE(st.input_tokens, -1)
		                      AND COALESCE(u.output_tokens, -1) = COALESCE(st.output_tokens, -1)
		                      AND u.source = st.source)`, tokenCols, tokenSels)

	// api_turns has no unique key in the schema: proxy rows carry a
	// request_id (match on it); rows without one fall back to the
	// (session, timestamp, provider, model, tokens) identity.
	insertAPITurnsSQL = fmt.Sprintf(`
		INSERT INTO api_turns
			(session_id, project_id, timestamp, provider, model, request_id,
			 input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			 cost_usd, message_count, tool_use_count, system_prompt_hash,
			 time_to_first_token_ms, total_response_ms, stop_reason,
			 message_prefix_hash, compression_original_bytes,
			 compression_compressed_bytes, compression_count,
			 compression_dropped_count, compression_marker_count,
			 cache_creation_1h_tokens, http_status, error_class, error_message,
			 web_search_requests, org_id, user_email, fast)
		SELECT st.session_id, `+remapProject+`, st.timestamp, st.provider, st.model,
		       st.request_id, st.input_tokens, st.output_tokens, st.cache_read_tokens,
		       st.cache_creation_tokens, st.cost_usd, st.message_count,
		       st.tool_use_count, st.system_prompt_hash, st.time_to_first_token_ms,
		       st.total_response_ms, st.stop_reason, st.message_prefix_hash,
		       st.compression_original_bytes, st.compression_compressed_bytes,
		       st.compression_count, st.compression_dropped_count,
		       st.compression_marker_count, st.cache_creation_1h_tokens,
		       st.http_status, st.error_class, st.error_message,
		       st.web_search_requests, st.org_id, st.user_email, st.fast
		  FROM src.api_turns st
		 WHERE CASE WHEN COALESCE(st.request_id, '') != ''
		       THEN NOT EXISTS (SELECT 1 FROM api_turns t
		                         WHERE t.request_id = st.request_id AND t.provider = st.provider)
		       ELSE NOT EXISTS (SELECT 1 FROM api_turns t
		                         WHERE COALESCE(t.session_id, '') = COALESCE(st.session_id, '')
		                           AND t.timestamp = st.timestamp
		                           AND t.provider = st.provider
		                           AND t.model = st.model
		                           AND t.input_tokens = st.input_tokens
		                           AND t.output_tokens = st.output_tokens)
		       END`, "st.project_id")

	insertEffortSQL = `
		INSERT OR IGNORE INTO claudecode_effort
			(session_id, tool_use_id, effort_level, event_name, received_at)
		SELECT session_id, tool_use_id, effort_level, event_name, received_at
		  FROM src.claudecode_effort`

	// failure_context rows remap action_id through the foreign
	// action's idempotency key. Legacy (NULL-key) foreign actions
	// keep their failure rows out — the heuristic identity isn't
	// strong enough to attach derived rows safely.
	insertFailuresSQL = fmt.Sprintf(`
		INSERT INTO failure_context
			(action_id, session_id, project_id, timestamp, command_hash,
			 command_summary, exit_code, error_category, error_message,
			 retry_count, eventually_succeeded, tee_file_path)
		SELECT la.id, sf.session_id, `+remapProject+`, sf.timestamp, sf.command_hash,
		       sf.command_summary, sf.exit_code, sf.error_category, sf.error_message,
		       sf.retry_count, sf.eventually_succeeded, sf.tee_file_path
		  FROM src.failure_context sf
		  JOIN src.actions sa ON sa.id = sf.action_id
		  JOIN actions la
		    ON la.source_file = sa.source_file AND la.source_event_id = sa.source_event_id
		 WHERE sa.source_event_id IS NOT NULL
		   AND `+remapProject+` IS NOT NULL
		   AND EXISTS (SELECT 1 FROM sessions ls WHERE ls.id = sf.session_id)
		   AND NOT EXISTS (SELECT 1 FROM failure_context f
		                    WHERE f.action_id = la.id AND f.timestamp = sf.timestamp)`,
		"sf.project_id", "sf.project_id")
)
