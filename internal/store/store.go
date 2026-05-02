package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/failure"
	"github.com/marmutapp/superbased-observer/internal/freshness"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// Store is the storage layer over an initialized SQLite database. All methods
// are safe for concurrent use.
type Store struct {
	db *sql.DB
}

// New wraps an already-opened *sql.DB (use internal/db.Open).
func New(db *sql.DB) *Store { return &Store{db: db} }

// normalizeProjectRoot folds paths that point inside a `.git` directory
// back to the working tree root. Pre-fix the live install accumulated a
// project row at `<repo>/.git/worktrees` because some
// session's cwd resolved into the worktree manager directory; that's an
// administrative path, not a project. Returns the input unchanged for
// any other shape.
func normalizeProjectRoot(rootPath string) string {
	const sep = "/.git/"
	if i := strings.Index(rootPath, sep); i > 0 {
		return rootPath[:i]
	}
	if strings.HasSuffix(rootPath, "/.git") {
		return strings.TrimSuffix(rootPath, "/.git")
	}
	return rootPath
}

// UpsertProject inserts or returns the id of the projects row for rootPath.
// remote may be empty.
func (s *Store) UpsertProject(ctx context.Context, rootPath, remote string) (int64, error) {
	if rootPath == "" {
		return 0, errors.New("store.UpsertProject: rootPath is required")
	}
	rootPath = normalizeProjectRoot(rootPath)
	now := timestamp(time.Now().UTC())
	// Try insert; on conflict, keep the existing row but update remote if
	// the caller supplied a non-empty value.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (root_path, git_remote, created_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(root_path) DO UPDATE SET
		   git_remote = COALESCE(NULLIF(excluded.git_remote, ''), projects.git_remote)`,
		rootPath, remote, now,
	)
	if err != nil {
		return 0, fmt.Errorf("store.UpsertProject: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE root_path = ?`, rootPath,
	).Scan(&id); err != nil {
		return 0, fmt.Errorf("store.UpsertProject: select id: %w", err)
	}
	return id, nil
}

// UpsertSession inserts a new session row or updates its mutable fields
// (ended_at, total_actions, model). id and started_at are immutable after
// first insert.
func (s *Store) UpsertSession(ctx context.Context, sess models.Session) error {
	if sess.ID == "" || sess.ProjectID == 0 || sess.Tool == "" {
		return errors.New("store.UpsertSession: ID, ProjectID, Tool are required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, model, git_branch, started_at, ended_at, total_actions, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   model = COALESCE(NULLIF(excluded.model, ''), sessions.model),
		   ended_at = COALESCE(excluded.ended_at, sessions.ended_at),
		   git_branch = COALESCE(NULLIF(excluded.git_branch, ''), sessions.git_branch),
		   total_actions = MAX(sessions.total_actions, excluded.total_actions)`,
		sess.ID,
		sess.ProjectID,
		sess.Tool,
		sess.Model,
		sess.GitBranch,
		timestamp(sess.StartedAt),
		nullableTimestamp(sess.EndedAt),
		sess.TotalActions,
		sess.Metadata,
	)
	if err != nil {
		return fmt.Errorf("store.UpsertSession: %w", err)
	}
	return nil
}

// GetCursor returns the persisted byte offset for sourceFile, or 0 on first
// access. A missing row is not an error.
func (s *Store) GetCursor(ctx context.Context, sourceFile string) (int64, error) {
	var off int64
	err := s.db.QueryRowContext(ctx,
		`SELECT byte_offset FROM parse_cursors WHERE source_file = ?`, sourceFile,
	).Scan(&off)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store.GetCursor: %w", err)
	}
	return off, nil
}

// CursorEntry is one parse_cursors row exposed to callers that need to
// enumerate every known session file (e.g. the watcher's poll fallback).
type CursorEntry struct {
	SourceFile string
	ByteOffset int64
}

// ListCursors returns every parse_cursors row. Order is unspecified.
//
// Used by the watcher's poll fallback to re-stat known session files
// and recover from fsnotify Write events dropped on busy filesystems
// (notably WSL2/NTFS, where fsnotify is documented to be lossy).
func (s *Store) ListCursors(ctx context.Context) ([]CursorEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source_file, byte_offset FROM parse_cursors`)
	if err != nil {
		return nil, fmt.Errorf("store.ListCursors: %w", err)
	}
	defer rows.Close()
	var out []CursorEntry
	for rows.Next() {
		var c CursorEntry
		if err := rows.Scan(&c.SourceFile, &c.ByteOffset); err != nil {
			return nil, fmt.Errorf("store.ListCursors: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ListCursors: %w", err)
	}
	return out, nil
}

// SetCursor persists the byte offset for sourceFile. Monotonic — a lower
// offset than the existing one is rejected to protect against accidental
// rewinds.
func (s *Store) SetCursor(ctx context.Context, sourceFile string, offset int64) error {
	if sourceFile == "" {
		return errors.New("store.SetCursor: sourceFile is required")
	}
	now := timestamp(time.Now().UTC())
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO parse_cursors (source_file, byte_offset, last_parsed)
		 VALUES (?, ?, ?)
		 ON CONFLICT(source_file) DO UPDATE SET
		   byte_offset = MAX(parse_cursors.byte_offset, excluded.byte_offset),
		   last_parsed = excluded.last_parsed`,
		sourceFile, offset, now,
	)
	if err != nil {
		return fmt.Errorf("store.SetCursor: %w", err)
	}
	return nil
}

// insertActionSQL upserts an action row keyed on
// (source_file, source_event_id). On conflict, only `duration_ms` is
// allowed to update — and only when the new value is non-zero AND the
// existing value is zero. This propagates adapter improvements for
// fields the source format under-reports (claude-code's tool_use →
// tool_result gap, codex's response_item function_call → output gap)
// to historical rows on a re-scan, without ever clobbering a value
// that's already populated. All other columns stay frozen on
// re-insert. Pattern mirrors the v1.4.27 token_usage.model fix.
const insertActionSQL = `INSERT INTO actions (
	session_id, project_id, timestamp, turn_index,
	action_type, is_native_tool,
	target, target_hash,
	success, error_message,
	duration_ms,
	content_hash, file_mtime, file_size_bytes, freshness, prior_action_id, change_detected,
	preceding_reasoning,
	raw_tool_name, raw_tool_input,
	tool,
	source_file, source_event_id,
	is_sidechain,
	message_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_file, source_event_id) DO UPDATE SET
	duration_ms = CASE
		WHEN excluded.duration_ms > 0 AND (actions.duration_ms IS NULL OR actions.duration_ms = 0)
		THEN excluded.duration_ms
		ELSE actions.duration_ms
	END`

// InsertActions writes a batch of actions using INSERT OR IGNORE — duplicate
// (source_file, source_event_id) rows are silently skipped. Returns the
// count of newly inserted rows. Runs in a single transaction.
//
// For each successfully inserted row, the corresponding actions[i].ID is
// populated with the new rowid so callers can chain additional work
// (e.g. freshness.UpsertFileState). Rows skipped via INSERT OR IGNORE retain
// ID = 0.
func (s *Store) InsertActions(ctx context.Context, actions []models.Action) (int, error) {
	if len(actions) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.InsertActions: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertActionSQL)
	if err != nil {
		return 0, fmt.Errorf("store.InsertActions: prepare: %w", err)
	}
	defer stmt.Close()

	var inserted int
	for i := range actions {
		a := &actions[i]
		res, err := stmt.ExecContext(ctx,
			a.SessionID,
			a.ProjectID,
			timestamp(a.Timestamp),
			a.TurnIndex,
			a.ActionType,
			boolToInt(a.IsNativeTool),
			a.Target,
			a.TargetHash,
			boolToInt(a.Success),
			a.ErrorMessage,
			a.DurationMs,
			nullableString(a.ContentHash),
			nullableTimestamp(a.FileMtime),
			nullableInt64(a.FileSizeBytes),
			nullableString(a.Freshness),
			nullableInt64(a.PriorActionID),
			boolToInt(a.ChangeDetected),
			nullableString(a.PrecedingReasoning),
			nullableString(a.RawToolName),
			nullableString(a.RawToolInput),
			a.Tool,
			a.SourceFile,
			a.SourceEventID,
			boolToInt(a.IsSidechain),
			nullableString(a.MessageID),
		)
		if err != nil {
			return inserted, fmt.Errorf("store.InsertActions: exec: %w", err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			if id, err := res.LastInsertId(); err == nil {
				a.ID = id
			}
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("store.InsertActions: commit: %w", err)
	}
	return inserted, nil
}

// InsertTokenEvents batches token_usage rows. Idempotent via
// UNIQUE(source_file, source_event_id).
func (s *Store) InsertTokenEvents(ctx context.Context, events []models.TokenEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.InsertTokenEvents: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// On conflict (same source_file + source_event_id), refresh model
	// only — counts, cost, source, and reliability are quality-sensitive
	// and must NOT be clobbered by a re-parse. Model is purely a label;
	// upgrading from a placeholder ("copilot/auto") to a resolved value
	// ("claude-haiku-4-5-20251001") is always a win, and an empty new
	// value is preserved as the existing one via COALESCE+NULLIF.
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO token_usage (
		session_id, timestamp, tool, model,
		input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
		cache_creation_1h_tokens, reasoning_tokens,
		estimated_cost_usd, source, reliability,
		source_file, source_event_id, message_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(source_file, source_event_id) DO UPDATE SET
		model = COALESCE(NULLIF(excluded.model, ''), token_usage.model)`)
	if err != nil {
		return 0, fmt.Errorf("store.InsertTokenEvents: prepare: %w", err)
	}
	defer stmt.Close()

	var inserted int
	for _, e := range events {
		res, err := stmt.ExecContext(ctx,
			e.SessionID,
			timestamp(e.Timestamp),
			e.Tool,
			e.Model,
			e.InputTokens,
			e.OutputTokens,
			e.CacheReadTokens,
			e.CacheCreationTokens,
			nullableInt64(e.CacheCreation1hTokens),
			e.ReasoningTokens,
			e.EstimatedCostUSD,
			e.Source,
			e.Reliability,
			nullableString(e.SourceFile),
			nullableString(e.SourceEventID),
			nullableString(e.MessageID),
		)
		if err != nil {
			return inserted, fmt.Errorf("store.InsertTokenEvents: exec: %w", err)
		}
		n, _ := res.RowsAffected()
		inserted += int(n)
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("store.InsertTokenEvents: commit: %w", err)
	}
	return inserted, nil
}

// IngestOptions parameterizes Ingest.
type IngestOptions struct {
	// IsNativeTool decides whether a ToolEvent's raw tool name maps to a
	// native tool (drives actions.is_native_tool). Defaults to always false.
	IsNativeTool func(rawToolName string) bool
	// Classifier, when non-nil, computes freshness for file-typed actions
	// (read_file, write_file, edit_file) and maintains the file_state table.
	// Requires an initialized DB with the file_state schema.
	Classifier *freshness.Classifier
	// RecordFailures, when true, populates failure_context for every
	// failed run_command action and updates retry_count / eventually_succeeded
	// on matching prior failures.
	RecordFailures bool
	// Indexer, when non-nil, stores the event's ToolOutput excerpt in the
	// FTS5 action_excerpts table so the MCP search_past_outputs tool can
	// retrieve it.
	Indexer *indexing.Indexer
}

// fileActionTypes is the set of normalized actions whose target is a file
// path eligible for freshness classification.
var fileActionTypes = map[string]struct{}{
	models.ActionReadFile:  {},
	models.ActionWriteFile: {},
	models.ActionEditFile:  {},
}

// IngestResult is the summary returned by Ingest.
type IngestResult struct {
	ActionsInserted int
	TokensInserted  int
	ProjectsTouched int
	SessionsTouched int
}

// Ingest is the high-level batch API used by the watcher and scan commands.
// It resolves projects from ToolEvent.ProjectRoot, upserts sessions, and
// inserts actions + token events in one go.
//
// Events with an empty SessionID or empty ProjectRoot are skipped and
// counted in warnings (callers should prefer to filter upstream).
func (s *Store) Ingest(
	ctx context.Context,
	events []models.ToolEvent,
	tokens []models.TokenEvent,
	opts IngestOptions,
) (IngestResult, error) {
	if opts.IsNativeTool == nil {
		opts.IsNativeTool = func(string) bool { return false }
	}

	projectIDs := map[string]int64{}
	sessionsSeen := map[string]struct{}{}
	var result IngestResult

	actions := make([]models.Action, 0, len(events))

	for _, e := range events {
		if e.SessionID == "" || e.ProjectRoot == "" {
			continue
		}
		pid, ok := projectIDs[e.ProjectRoot]
		if !ok {
			var err error
			pid, err = s.UpsertProject(ctx, e.ProjectRoot, "")
			if err != nil {
				return result, err
			}
			projectIDs[e.ProjectRoot] = pid
			result.ProjectsTouched++
		}
		if _, ok := sessionsSeen[e.SessionID]; !ok {
			err := s.UpsertSession(ctx, models.Session{
				ID:        e.SessionID,
				ProjectID: pid,
				Tool:      e.Tool,
				Model:     e.Model,
				GitBranch: e.GitBranch,
				StartedAt: e.Timestamp,
			})
			if err != nil {
				return result, err
			}
			sessionsSeen[e.SessionID] = struct{}{}
			result.SessionsTouched++
		}
		act := models.Action{
			SessionID:          e.SessionID,
			ProjectID:          pid,
			Timestamp:          e.Timestamp,
			TurnIndex:          e.TurnIndex,
			ActionType:         e.ActionType,
			IsNativeTool:       opts.IsNativeTool(e.RawToolName),
			Target:             e.Target,
			TargetHash:         sha256Hex(e.Target),
			Success:            e.Success,
			ErrorMessage:       e.ErrorMessage,
			DurationMs:         e.DurationMs,
			PrecedingReasoning: e.PrecedingReasoning,
			RawToolName:        e.RawToolName,
			RawToolInput:       e.RawToolInput,
			Tool:               e.Tool,
			SourceFile:         e.SourceFile,
			SourceEventID:      e.SourceEventID,
			IsSidechain:        e.IsSidechain,
			MessageID:          e.MessageID,
		}

		// File-typed actions with a classifier go through a per-event
		// classify → insert → file_state upsert cycle, so that a second
		// file event in this same batch sees the first one's hash.
		// Non-file actions stay in the batched actions slice.
		if opts.Classifier != nil && isFileAction(e.ActionType) {
			abs := resolveAbs(e.ProjectRoot, e.Target)
			if abs != "" {
				obs, err := opts.Classifier.Classify(ctx, pid, e.SessionID, e.ActionType, abs)
				if err == nil {
					act.ContentHash = obs.ContentHash
					act.FileMtime = obs.FileMtime
					act.FileSizeBytes = obs.FileSizeBytes
					act.Freshness = obs.Freshness
					act.PriorActionID = obs.PriorActionID
					act.ChangeDetected = obs.ChangeDetected
				}
				inserted, err := s.insertSingleAction(ctx, &act)
				if err != nil {
					return result, err
				}
				if inserted && act.ContentHash != "" {
					if err := opts.Classifier.UpsertFileState(
						ctx, pid, abs,
						freshness.FileObservation{
							ContentHash:    act.ContentHash,
							FileMtime:      act.FileMtime,
							FileSizeBytes:  act.FileSizeBytes,
							Freshness:      act.Freshness,
							PriorActionID:  act.PriorActionID,
							ChangeDetected: act.ChangeDetected,
						},
						act.ID, act.ActionType, e.SessionID,
					); err != nil {
						return result, err
					}
				}
				if inserted {
					result.ActionsInserted++
				}
				continue
			}
		}

		actions = append(actions, act)
	}

	// Upsert sessions referenced only by TokenEvents (e.g. subagent
	// compaction turns that have usage but no tool_use blocks).
	validTokens := make([]models.TokenEvent, 0, len(tokens))
	for _, tk := range tokens {
		if tk.SessionID == "" {
			continue
		}
		if _, ok := sessionsSeen[tk.SessionID]; ok {
			validTokens = append(validTokens, tk)
			continue
		}
		if tk.ProjectRoot == "" {
			// No owning project — skip to avoid an FK violation.
			continue
		}
		pid, ok := projectIDs[tk.ProjectRoot]
		if !ok {
			var err error
			pid, err = s.UpsertProject(ctx, tk.ProjectRoot, "")
			if err != nil {
				return result, err
			}
			projectIDs[tk.ProjectRoot] = pid
			result.ProjectsTouched++
		}
		err := s.UpsertSession(ctx, models.Session{
			ID:        tk.SessionID,
			ProjectID: pid,
			Tool:      tk.Tool,
			Model:     tk.Model,
			GitBranch: tk.GitBranch,
			StartedAt: tk.Timestamp,
		})
		if err != nil {
			return result, err
		}
		sessionsSeen[tk.SessionID] = struct{}{}
		result.SessionsTouched++
		validTokens = append(validTokens, tk)
	}

	n, err := s.InsertActions(ctx, actions)
	if err != nil {
		return result, err
	}
	result.ActionsInserted += n

	if opts.RecordFailures {
		for i := range actions {
			a := &actions[i]
			if a.ID == 0 || a.ActionType != models.ActionRunCommand {
				continue
			}
			if err := s.recordCommandOutcome(ctx, a); err != nil {
				return result, err
			}
		}
	}

	if opts.Indexer != nil {
		if err := s.indexOutputs(ctx, events, actions, opts.Indexer); err != nil {
			return result, err
		}
	}

	tn, err := s.InsertTokenEvents(ctx, validTokens)
	if err != nil {
		return result, err
	}
	result.TokensInserted = tn
	return result, nil
}

// indexOutputs records tool output excerpts in the FTS5 action_excerpts
// table. It matches inserted actions (ID != 0) back to their originating
// event by SourceEventID and skips events whose ToolOutput is empty.
func (s *Store) indexOutputs(
	ctx context.Context,
	events []models.ToolEvent,
	actions []models.Action,
	idx *indexing.Indexer,
) error {
	byID := make(map[string]*models.Action, len(actions))
	for i := range actions {
		a := &actions[i]
		if a.ID == 0 {
			continue
		}
		byID[a.SourceEventID] = a
	}
	for i := range events {
		e := &events[i]
		if e.ToolOutput == "" {
			continue
		}
		a, ok := byID[e.SourceEventID]
		if !ok {
			continue
		}
		if err := idx.Index(ctx, a.ID, e.RawToolName, a.Target, e.ToolOutput, a.ErrorMessage); err != nil {
			return err
		}
	}
	return nil
}

// recordCommandOutcome maintains failure_context for a single run_command
// action: failed commands get a new row with retry_count set to the number of
// prior failures of the same command_hash in this session, and succeeded
// commands flip eventually_succeeded on all prior matching failure rows.
func (s *Store) recordCommandOutcome(ctx context.Context, a *models.Action) error {
	cmdHash := failure.CommandHash(a.Target)
	if cmdHash == "" {
		return nil
	}
	if a.Success {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE failure_context SET eventually_succeeded = 1
			 WHERE session_id = ? AND command_hash = ? AND eventually_succeeded = 0`,
			a.SessionID, cmdHash,
		); err != nil {
			return fmt.Errorf("store.recordCommandOutcome: update succeeded: %w", err)
		}
		return nil
	}
	var retryCount int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM failure_context
		 WHERE session_id = ? AND command_hash = ?`,
		a.SessionID, cmdHash,
	).Scan(&retryCount)
	if err != nil {
		return fmt.Errorf("store.recordCommandOutcome: count prior: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO failure_context (
			action_id, session_id, project_id, timestamp,
			command_hash, command_summary, error_category, error_message,
			retry_count
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.SessionID, a.ProjectID, timestamp(a.Timestamp),
		cmdHash, failure.CommandSummary(a.Target),
		failure.Categorize(a.ErrorMessage),
		failure.TruncateErrorMessage(a.ErrorMessage),
		retryCount,
	)
	if err != nil {
		return fmt.Errorf("store.recordCommandOutcome: insert: %w", err)
	}
	return nil
}

// insertSingleAction is the per-row path used for file-typed actions so the
// freshness pipeline can read and write file_state between adjacent events in
// the same batch. Returns (true, nil) when a new row was inserted;
// (false, nil) means a duplicate (source_file, source_event_id) was skipped
// via INSERT OR IGNORE.
func (s *Store) insertSingleAction(ctx context.Context, a *models.Action) (bool, error) {
	res, err := s.db.ExecContext(ctx, insertActionSQL,
		a.SessionID,
		a.ProjectID,
		timestamp(a.Timestamp),
		a.TurnIndex,
		a.ActionType,
		boolToInt(a.IsNativeTool),
		a.Target,
		a.TargetHash,
		boolToInt(a.Success),
		a.ErrorMessage,
		a.DurationMs,
		nullableString(a.ContentHash),
		nullableTimestamp(a.FileMtime),
		nullableInt64(a.FileSizeBytes),
		nullableString(a.Freshness),
		nullableInt64(a.PriorActionID),
		boolToInt(a.ChangeDetected),
		nullableString(a.PrecedingReasoning),
		nullableString(a.RawToolName),
		nullableString(a.RawToolInput),
		a.Tool,
		a.SourceFile,
		a.SourceEventID,
		boolToInt(a.IsSidechain),
		nullableString(a.MessageID),
	)
	if err != nil {
		return false, fmt.Errorf("store.insertSingleAction: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if id, err := res.LastInsertId(); err == nil {
		a.ID = id
	}
	return true, nil
}

// isFileAction reports whether actionType classifies a file target.
func isFileAction(actionType string) bool {
	_, ok := fileActionTypes[actionType]
	return ok
}

// resolveAbs resolves a possibly project-relative target into an absolute
// filesystem path suitable for freshness hashing. Returns "" for
// "[external]/..." pseudo-paths (handled as unknown).
func resolveAbs(projectRoot, target string) string {
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "[external]/") {
		return ""
	}
	if filepath.IsAbs(target) {
		return target
	}
	if projectRoot == "" {
		return ""
	}
	return filepath.Join(projectRoot, target)
}

// CountActions returns the total number of rows in the actions table. Useful
// for tests and the status command.
func (s *Store) CountActions(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actions`).Scan(&n)
	return n, err
}

// InsertAPITurn records a single proxy-observed request/response pair. An
// empty SessionID becomes NULL; a zero ProjectID becomes NULL. Returns the
// new rowid.
//
// For successful turns Provider + Model are required. For error turns
// (HTTPStatus != 0) Model may be empty — the upstream sometimes
// rejects malformed requests before any model field is parsed, and
// a zero-token error row with empty model is still useful for
// surfacing the failure.
func (s *Store) InsertAPITurn(ctx context.Context, t models.APITurn) (int64, error) {
	if t.Provider == "" {
		return 0, errors.New("store.InsertAPITurn: Provider is required")
	}
	if t.Model == "" && t.HTTPStatus == 0 {
		return 0, errors.New("store.InsertAPITurn: Model is required for non-error turns")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_turns (
			session_id, project_id, timestamp,
			provider, model, request_id,
			input_tokens, output_tokens,
			cache_read_tokens, cache_creation_tokens, cache_creation_1h_tokens,
			cost_usd, message_count, tool_use_count,
			system_prompt_hash, message_prefix_hash,
			time_to_first_token_ms, total_response_ms,
			stop_reason,
			compression_original_bytes, compression_compressed_bytes,
			compression_count, compression_dropped_count, compression_marker_count,
			http_status, error_class, error_message
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullableString(t.SessionID),
		nullableInt64(t.ProjectID),
		timestamp(t.Timestamp),
		t.Provider,
		t.Model,
		nullableString(t.RequestID),
		t.InputTokens,
		t.OutputTokens,
		nullableInt64(t.CacheReadTokens),
		nullableInt64(t.CacheCreationTokens),
		nullableInt64(t.CacheCreation1hTokens),
		nullableFloat64(t.CostUSD),
		nullableInt(t.MessageCount),
		nullableInt(t.ToolUseCount),
		nullableString(t.SystemPromptHash),
		nullableString(t.MessagePrefixHash),
		nullableInt64(t.TimeToFirstTokenMS),
		nullableInt64(t.TotalResponseMS),
		nullableString(t.StopReason),
		nullableInt64(t.CompressionOriginalBytes),
		nullableInt64(t.CompressionCompressedBytes),
		nullableInt64(t.CompressionCount),
		nullableInt64(t.CompressionDroppedCount),
		nullableInt64(t.CompressionMarkerCount),
		nullableInt(t.HTTPStatus),
		nullableString(t.ErrorClass),
		nullableString(t.ErrorMessage),
	)
	if err != nil {
		return 0, fmt.Errorf("store.InsertAPITurn: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store.InsertAPITurn: last insert id: %w", err)
	}
	// Per-event compression detail (migration 009). Best-effort: if the
	// table doesn't exist (pre-migration test DB) or insert fails for a
	// single event, we log and continue rather than abort the turn —
	// the aggregate columns above already captured what cost.Engine
	// needs. The dashboard's mechanism breakdown depends on these rows
	// landing, but the cost calc doesn't.
	if len(t.CompressionEvents) > 0 {
		for _, ev := range t.CompressionEvents {
			ts := ev.Timestamp
			if ts.IsZero() {
				ts = t.Timestamp
			}
			var importance any
			if ev.Mechanism == "drop" {
				importance = ev.ImportanceScore
			}
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO compression_events
					(api_turn_id, timestamp, mechanism, original_bytes,
					 compressed_bytes, msg_index, importance_score)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, timestamp(ts), ev.Mechanism,
				ev.OriginalBytes, ev.CompressedBytes,
				nullableInt(ev.MsgIndex), importance,
			); err != nil {
				// Don't return — keep going so a partial schema (no 009
				// yet) doesn't break new turn ingestion.
				break
			}
		}
	}
	return id, nil
}

// CountAPITurns returns the total number of rows in api_turns. Useful for
// tests and the status command.
func (s *Store) CountAPITurns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_turns`).Scan(&n)
	return n, err
}

// --- helpers ---

func timestamp(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// InsertObserverLog records a single line in the observer_log table. Level
// should be one of "debug", "info", "warn", "error". Details is optional —
// callers typically pass a compact JSON blob or the empty string.
func (s *Store) InsertObserverLog(ctx context.Context, level, component, message, details string) error {
	if strings.TrimSpace(level) == "" || strings.TrimSpace(component) == "" {
		return errors.New("store.InsertObserverLog: level and component required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO observer_log (timestamp, level, component, message, details)
		 VALUES (?, ?, ?, ?, ?)`,
		timestamp(time.Now().UTC()), level, component, message, nullableString(details),
	)
	if err != nil {
		return fmt.Errorf("store.InsertObserverLog: %w", err)
	}
	return nil
}

func nullableTimestamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableFloat64(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func sha256Hex(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
