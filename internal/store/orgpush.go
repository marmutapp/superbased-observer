package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// schema_meta keys holding the per-table push cursor (the highest table id /
// sessions.rowid already shipped to the org server). Using insertion-ordered
// ids — not event timestamps — means backfilled rows, which receive fresh
// high ids regardless of their event time, are still captured by a later push.
const (
	pushCursorKeySessions   = "org_push_cursor_sessions"
	pushCursorKeyActions    = "org_push_cursor_actions"
	pushCursorKeyAPITurns   = "org_push_cursor_api_turns"
	pushCursorKeyTokenUsage = "org_push_cursor_token_usage" //nolint:gosec // G101: schema_meta cursor key name, not a credential.
	// lastPushPayloadKey holds the JSON of the most recent successfully-pushed
	// envelope (the content-free rollup, exactly as marshalled before gzip), so
	// the dashboard can show the developer precisely what was shared. Overwritten
	// each successful push; cleared on unenrol.
	lastPushPayloadKey = "org_last_push_payload"
)

// PushCursor is the agent's per-table push position. Each field is the
// highest row id (sessions: rowid) already accepted by the server. Rows above
// it are candidates for the next batch.
type PushCursor struct {
	Sessions   int64
	Actions    int64
	APITurns   int64
	TokenUsage int64
}

// PushBatch is one batch of content-free rows read from the agent DB, plus the
// cursor the agent should persist if the server accepts it.
type PushBatch struct {
	Cursor     PushCursor
	Sessions   []orgcontract.SessionRow
	Actions    []orgcontract.ActionRow
	APITurns   []orgcontract.APITurnRow
	TokenUsage []orgcontract.TokenUsageRow
	EstBytes   int64
}

// RowCount is the total number of rows across all four tables in the batch.
func (b PushBatch) RowCount() int {
	return len(b.Sessions) + len(b.Actions) + len(b.APITurns) + len(b.TokenUsage)
}

// Empty reports whether the batch carries no rows.
func (b PushBatch) Empty() bool { return b.RowCount() == 0 }

// PushLogEntry is one row of org_push_log, surfaced to the dashboard.
type PushLogEntry struct {
	ID       int64
	PushedAt string
	RowCount int64
	Bytes    int64
	Status   string
	Error    string
}

// LoadPushCursor reads the per-table push cursor from schema_meta. Missing
// keys (a never-pushed agent) read as 0.
func (s *Store) LoadPushCursor(ctx context.Context) (PushCursor, error) {
	var c PushCursor
	for key, dst := range map[string]*int64{
		pushCursorKeySessions:   &c.Sessions,
		pushCursorKeyActions:    &c.Actions,
		pushCursorKeyAPITurns:   &c.APITurns,
		pushCursorKeyTokenUsage: &c.TokenUsage,
	} {
		v, err := s.readMeta(ctx, key)
		if err != nil {
			return PushCursor{}, fmt.Errorf("store.LoadPushCursor: %w", err)
		}
		if v != "" {
			n, perr := strconv.ParseInt(v, 10, 64)
			if perr != nil {
				return PushCursor{}, fmt.Errorf("store.LoadPushCursor: parse %s=%q: %w", key, v, perr)
			}
			*dst = n
		}
	}
	return c, nil
}

// SavePushCursor persists the per-table push cursor to schema_meta in one tx.
func (s *Store) SavePushCursor(ctx context.Context, c PushCursor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.SavePushCursor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for key, val := range map[string]int64{
		pushCursorKeySessions:   c.Sessions,
		pushCursorKeyActions:    c.Actions,
		pushCursorKeyAPITurns:   c.APITurns,
		pushCursorKeyTokenUsage: c.TokenUsage,
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_meta(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			key, strconv.FormatInt(val, 10)); err != nil {
			return fmt.Errorf("store.SavePushCursor: set %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.SavePushCursor: commit: %w", err)
	}
	return nil
}

// CurrentMaxIDs returns the current high-water id of each table. Enrolment
// seeds the push cursor from this so only activity *after* enrolment is
// shared — the agent never retroactively pushes a developer's pre-enrolment
// history.
func (s *Store) CurrentMaxIDs(ctx context.Context) (PushCursor, error) {
	var c PushCursor
	for q, dst := range map[string]*int64{
		`SELECT COALESCE(MAX(rowid), 0) FROM sessions`: &c.Sessions,
		`SELECT COALESCE(MAX(id), 0) FROM actions`:     &c.Actions,
		`SELECT COALESCE(MAX(id), 0) FROM api_turns`:   &c.APITurns,
		`SELECT COALESCE(MAX(id), 0) FROM token_usage`: &c.TokenUsage,
	} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			return PushCursor{}, fmt.Errorf("store.CurrentMaxIDs: %w", err)
		}
	}
	return c, nil
}

// RecordPush appends a row to org_push_log. status is 'ok' | 'retry' | 'failed'.
func (s *Store) RecordPush(ctx context.Context, rowCount, byteCount int64, status, errMsg string) error {
	var errVal any
	if errMsg != "" {
		errVal = errMsg
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO org_push_log(pushed_at, row_count, byte_count, status, error)
		 VALUES (datetime('now'), ?, ?, ?, ?)`,
		rowCount, byteCount, status, errVal)
	if err != nil {
		return fmt.Errorf("store.RecordPush: %w", err)
	}
	return nil
}

// SaveLastPushPayload records the JSON of the most recent successfully-pushed
// envelope (the content-free rollup) so the dashboard can show what was shared.
func (s *Store) SaveLastPushPayload(ctx context.Context, payload []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO schema_meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		lastPushPayloadKey, string(payload)); err != nil {
		return fmt.Errorf("store.SaveLastPushPayload: %w", err)
	}
	return nil
}

// LoadLastPushPayload returns the JSON of the last pushed envelope, or nil when
// the agent has never pushed (or has unenrolled).
func (s *Store) LoadLastPushPayload(ctx context.Context) ([]byte, error) {
	v, err := s.readMeta(ctx, lastPushPayloadKey)
	if err != nil {
		return nil, fmt.Errorf("store.LoadLastPushPayload: %w", err)
	}
	if v == "" {
		return nil, nil
	}
	return []byte(v), nil
}

// LastPushLog returns the most recent org_push_log row, or (nil, nil) when the
// agent has never pushed.
func (s *Store) LastPushLog(ctx context.Context) (*PushLogEntry, error) {
	var e PushLogEntry
	var errVal sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, pushed_at, row_count, byte_count, status, error
		   FROM org_push_log ORDER BY id DESC LIMIT 1`).
		Scan(&e.ID, &e.PushedAt, &e.RowCount, &e.Bytes, &e.Status, &errVal)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store.LastPushLog: %w", err)
	}
	e.Error = errVal.String
	return &e, nil
}

// ClearLastPushState removes the prior-run last-push payload + log so
// `observer org status` after a re-enroll shows "(none yet)" instead of
// a stale timestamp. Called by orgclient.Enroll alongside the cursor
// seed; idempotent on a never-pushed agent. N5 in
// docs/teams-test-regression-2026-06-03.md.
func (s *Store) ClearLastPushState(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store.ClearLastPushState: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM schema_meta WHERE key = ?`, lastPushPayloadKey); err != nil {
		return fmt.Errorf("store.ClearLastPushState: clear payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM org_push_log`); err != nil {
		return fmt.Errorf("store.ClearLastPushState: clear log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store.ClearLastPushState: commit: %w", err)
	}
	return nil
}

// ScopeOptions restricts which projects feed the push by exact
// project_root path. Empty Allowlist + empty Denylist (the zero value)
// means "every project ships" — preserving v1.7.x behaviour. When
// Allowlist is non-empty, only listed roots are eligible; Denylist
// then strips anything matching from the result.
//
// Implementation note: SelectUnpushedSince resolves the list of project
// IDs once at the start of the query and inlines them into the SQL
// filters. modernc/sqlite has no native array-IN binding, so we render
// the IN-list inline (safe because every value came from a config-file
// allow/denylist, not user input).
type ScopeOptions struct {
	ProjectRootAllowlist []string
	ProjectRootDenylist  []string
}

// IsScoped reports whether either list is non-empty (the query path
// must compute project IDs).
func (s ScopeOptions) IsScoped() bool {
	return len(s.ProjectRootAllowlist) > 0 || len(s.ProjectRootDenylist) > 0
}

// ShareOptions controls which content-bearing columns the org-push seam
// includes in the wire payload.
//
// The default zero value (FullContent=false, no TargetActionAllowlist) is
// the v1.8.0 metadata-only posture: only sha256-hex hashes ship for the
// content-bearing columns (target/source_file/project_root/git_remote),
// with the raw values withheld. The node operator can opt the local
// daemon into full-content sharing by setting
// [org_client.share].full_content = true in their TOML config; the org
// admin cannot force this on remotely.
//
// TargetActionAllowlist, when non-empty, restricts which action types may
// carry a raw `target` even when FullContent is false. Use this to ship
// human-readable file paths for safe action types (read_file, edit_file,
// write_file) while withholding shell command bodies (run_command) and
// assistant prose (task_complete). Empty list means: no exceptions —
// when FullContent is false, NO action ships a raw target.
type ShareOptions struct {
	FullContent           bool
	TargetActionAllowlist []string
}

// targetAllowed reports whether the given action type may ship a raw
// target column under these options. Always true when FullContent is on;
// otherwise true only when actionType appears in the allowlist (exact
// string match; the action_type vocabulary is models.ActionXxx constants
// like "read_file" / "edit_file" / "run_command").
func (o ShareOptions) targetAllowed(actionType string) bool {
	if o.FullContent {
		return true
	}
	for _, a := range o.TargetActionAllowlist {
		if a == actionType {
			return true
		}
	}
	return false
}

// SelectUnpushedSince reads the next batch of content-free rows above the
// given cursor, in table order (sessions, actions, api_turns, token_usage),
// stopping once the estimated JSON size would exceed maxBytes (a single
// oversized row is still included if the batch is otherwise empty, to
// guarantee forward progress). orgID/userEmail are the enrolled identity and
// are stamped onto every row — the push is attributed to the enrolled user
// regardless of the row's locally-stored attribution columns. The returned
// Cursor carries each table's new high-water id for the rows included.
//
// Privacy posture (v1.8.0): only the allowed, content-free columns are
// selected; raw_tool_input, raw_tool_output, preceding_reasoning,
// error_message, and prompt bodies are NEVER read here. The
// content-bearing columns target / source_file / project_root / git_remote
// are scanned (so the hash counterpart is also scanned in the same query),
// but the raw fields are zeroed in Go before the row enters the batch
// unless ShareOptions.FullContent is true (or per-action permitted via
// TargetActionAllowlist). This is the single SQL seam where the privacy
// posture is enforced (spec §1.5); the privacy invariant test asserts it.
//
// share is the v1.8.0 ShareOptions; passing a zero value preserves the
// pre-v1.8 behavior would have been (raw fields shipped) but with the
// inverted default: zero value now means metadata-only. Existing callers
// that don't opt into share get the safe behavior automatically.
//
// scope is the v1.8.0 ScopeOptions; passing a zero value (both lists
// empty) means "every project ships" — the v1.7 behaviour. A non-empty
// Allowlist restricts to those project roots; a non-empty Denylist
// strips any roots matching from the result.
// nolint:gocyclo // four near-identical per-table loops (sessions, actions,
// api_turns, token_usage) each with their own SQL + per-row mapping + per-row
// privacy-strip rules; extracting helpers obscures the regular shape and
// breaks the budgetHit threading. The complexity is structural, not branchy.
func (s *Store) SelectUnpushedSince(ctx context.Context, cur PushCursor, maxBytes int64, orgID, userEmail string, share ShareOptions, scope ScopeOptions) (PushBatch, error) {
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	batch := PushBatch{Cursor: cur}
	var est int64
	budgetHit := false

	// fits reports whether a row of the given size may be added: always true
	// for the first row (forward progress), otherwise only within budget.
	fits := func(rowBytes int64) bool {
		if batch.RowCount() == 0 {
			return true
		}
		return est+rowBytes <= maxBytes
	}

	// Scope: resolve allow/deny project roots to the set of allowed
	// project_ids once, then inline the IN-list into each table's
	// WHERE clause. When neither list is set, scopeFilter == "" and the
	// queries run with no project filter. Empty allowed set after
	// resolution (e.g. allowlist had only paths that aren't in the DB)
	// means no rows are eligible — return an empty batch immediately.
	scopeFilter, scopeNoMatch, err := s.resolveScopeFilter(ctx, scope)
	if err != nil {
		return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: resolve scope: %w", err)
	}
	if scopeNoMatch {
		batch.EstBytes = 0
		return batch, nil
	}

	// --- sessions ---
	if !budgetHit {
		q := `SELECT s.rowid, s.id,
		             COALESCE(p.root_path_hash,''), COALESCE(p.git_remote_hash,''),
		             COALESCE(p.root_path,''),      COALESCE(p.git_remote,''),
		             s.tool,
		             COALESCE(s.model,''), COALESCE(s.git_branch,''), s.started_at,
		             COALESCE(s.ended_at,''), COALESCE(s.total_actions,0)
		        FROM sessions s JOIN projects p ON s.project_id = p.id
		       WHERE s.rowid > ?`
		if scopeFilter != "" {
			q += ` AND p.id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY s.rowid ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.Sessions)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: sessions: %w", err)
		}
		for rows.Next() {
			var rowid int64
			var r orgcontract.SessionRow
			if err := rows.Scan(&rowid, &r.ID,
				&r.ProjectRootHash, &r.GitRemoteHash,
				&r.ProjectRoot, &r.GitRemote,
				&r.Tool,
				&r.Model, &r.GitBranch, &r.StartedAt, &r.EndedAt, &r.TotalActions); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan session: %w", err)
			}
			// Privacy seam: strip raw paths when not opted into full-content
			// sharing. The hash counterparts (already scanned) carry the
			// signal the server needs.
			if !share.FullContent {
				r.ProjectRoot = ""
				r.GitRemote = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				budgetHit = true
				break
			}
			batch.Sessions = append(batch.Sessions, r)
			batch.Cursor.Sessions = rowid
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: sessions rows: %w", err)
		}
	}

	// --- actions ---
	if !budgetHit {
		q := `SELECT a.id, a.session_id,
		             COALESCE(a.target_hash,''), COALESCE(a.source_file_hash,''),
		             COALESCE(a.source_file,''), COALESCE(a.source_event_id,''),
		             a.timestamp, a.tool, a.action_type,
		             COALESCE(a.target,''),
		             COALESCE(a.turn_index,0), COALESCE(a.success,1), COALESCE(a.duration_ms,0),
		             COALESCE(a.is_sidechain,0)
		        FROM actions a WHERE a.id > ?`
		if scopeFilter != "" {
			q += ` AND a.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY a.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.Actions)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: actions: %w", err)
		}
		for rows.Next() {
			var id, success, sidechain int64
			var r orgcontract.ActionRow
			if err := rows.Scan(&id, &r.SessionID,
				&r.TargetHash, &r.SourceFileHash,
				&r.SourceFile, &r.SourceEventID,
				&r.Timestamp, &r.Tool, &r.ActionType,
				&r.Target, &r.TurnIndex,
				&success, &r.DurationMs, &sidechain); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan action: %w", err)
			}
			r.Success = success != 0
			r.IsSidechain = sidechain != 0
			// Privacy seam:
			//   - SourceFile is a filesystem path → strip when not opted in.
			//   - Target is per-action: in full-content mode, always ship;
			//     in metadata-only mode, ship only when the action type is
			//     in the explicit TargetActionAllowlist (e.g. read_file).
			if !share.FullContent {
				r.SourceFile = ""
			}
			if !share.targetAllowed(r.ActionType) {
				r.Target = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				budgetHit = true
				break
			}
			batch.Actions = append(batch.Actions, r)
			batch.Cursor.Actions = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: actions rows: %w", err)
		}
	}

	// --- api_turns ---
	if !budgetHit {
		q := `SELECT t.id, COALESCE(t.session_id,''),
		             COALESCE(p.root_path_hash,''), COALESCE(p.root_path,''),
		             t.timestamp,
		             t.provider, COALESCE(t.model,''), COALESCE(t.request_id,''),
		             t.input_tokens, t.output_tokens, COALESCE(t.cache_read_tokens,0),
		             COALESCE(t.cache_creation_tokens,0), COALESCE(t.cache_creation_1h_tokens,0),
		             COALESCE(t.web_search_requests,0), COALESCE(t.cost_usd,0),
		             COALESCE(t.message_count,0), COALESCE(t.tool_use_count,0),
		             COALESCE(t.system_prompt_hash,''), COALESCE(t.message_prefix_hash,''),
		             COALESCE(t.time_to_first_token_ms,0), COALESCE(t.total_response_ms,0),
		             COALESCE(t.stop_reason,''), COALESCE(t.http_status,0), COALESCE(t.error_class,'')
		        FROM api_turns t LEFT JOIN projects p ON t.project_id = p.id
		       WHERE t.id > ?`
		if scopeFilter != "" {
			q += ` AND t.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY t.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.APITurns)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: api_turns: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.APITurnRow
			if err := rows.Scan(&id, &r.SessionID,
				&r.ProjectRootHash, &r.ProjectRoot,
				&r.Timestamp, &r.Provider,
				&r.Model, &r.RequestID, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
				&r.CacheCreationTokens, &r.CacheCreation1hTokens, &r.WebSearchRequests, &r.CostUSD,
				&r.MessageCount, &r.ToolUseCount, &r.SystemPromptHash, &r.MessagePrefixHash,
				&r.TimeToFirstTokenMS, &r.TotalResponseMS, &r.StopReason, &r.HTTPStatus,
				&r.ErrorClass); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan api_turn: %w", err)
			}
			if !share.FullContent {
				r.ProjectRoot = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				budgetHit = true
				break
			}
			batch.APITurns = append(batch.APITurns, r)
			batch.Cursor.APITurns = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: api_turns rows: %w", err)
		}
	}

	// --- token_usage ---
	if !budgetHit {
		q := `SELECT tu.id, tu.session_id,
		             COALESCE(p.root_path_hash,''), COALESCE(p.root_path,''),
		             tu.timestamp, tu.tool,
		             COALESCE(tu.model,''), COALESCE(tu.input_tokens,0), COALESCE(tu.output_tokens,0),
		             COALESCE(tu.cache_read_tokens,0), COALESCE(tu.cache_creation_tokens,0),
		             COALESCE(tu.cache_creation_1h_tokens,0), COALESCE(tu.reasoning_tokens,0),
		             COALESCE(tu.web_search_requests,0), COALESCE(tu.estimated_cost_usd,0),
		             tu.source, COALESCE(tu.reliability,'unknown'),
		             COALESCE(tu.source_file_hash,''), COALESCE(tu.source_file,''),
		             COALESCE(tu.source_event_id,'')
		        FROM token_usage tu
		        LEFT JOIN sessions s ON tu.session_id = s.id
		        LEFT JOIN projects p ON s.project_id = p.id
		       WHERE tu.id > ?`
		if scopeFilter != "" {
			q += ` AND s.project_id IN (` + scopeFilter + `)`
		}
		q += ` ORDER BY tu.id ASC`
		rows, err := s.db.QueryContext(ctx, q, cur.TokenUsage)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: token_usage: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.TokenUsageRow
			if err := rows.Scan(&id, &r.SessionID,
				&r.ProjectRootHash, &r.ProjectRoot,
				&r.Timestamp, &r.Tool,
				&r.Model, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
				&r.CacheCreation1hTokens, &r.ReasoningTokens, &r.WebSearchRequests, &r.EstimatedCostUSD,
				&r.Source, &r.Reliability,
				&r.SourceFileHash, &r.SourceFile,
				&r.SourceEventID); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan token_usage: %w", err)
			}
			if !share.FullContent {
				r.ProjectRoot = ""
				r.SourceFile = ""
			}
			r.OrgID, r.UserEmail = orgID, userEmail
			sz := jsonSize(r)
			if !fits(sz) {
				// token_usage is the last table; no need to set budgetHit.
				break
			}
			batch.TokenUsage = append(batch.TokenUsage, r)
			batch.Cursor.TokenUsage = id
			est += sz
		}
		if err := closeRows(rows); err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: token_usage rows: %w", err)
		}
	}

	batch.EstBytes = est
	return batch, nil
}

// resolveScopeFilter turns the configured project_root allowlist /
// denylist into a SQL fragment that can be AND-ed into per-table WHERE
// clauses. Returns:
//
//   - filter: an empty string when no scope is configured (everything
//     ships), or e.g. "AND p.id IN (1,3,5)" otherwise. The caller's
//     queries use alias `p` for the projects table; for the actions
//     and api_turns paths, swap `p.id` for `a.project_id` /
//     `t.project_id` directly.
//   - noMatch: true when an allowlist was configured but no project
//     root in the DB matches; the caller should short-circuit to an
//     empty batch (the operator asked for nothing).
//   - err: a real I/O error reading the projects table.
//
// Values come from a config-file allow/denylist (not user input), so
// the IN-list is rendered inline — modernc/sqlite has no native array
// binding and a single per-token bind would explode for hundreds of
// roots. Project IDs are integers, so injection is impossible by
// construction.
func (s *Store) resolveScopeFilter(ctx context.Context, scope ScopeOptions) (filter string, noMatch bool, err error) {
	if !scope.IsScoped() {
		return "", false, nil
	}
	roots, err := s.projectIDsByRoot(ctx)
	if err != nil {
		return "", false, err
	}
	var ids []int64
	if len(scope.ProjectRootAllowlist) > 0 {
		seen := make(map[int64]bool)
		for _, rp := range scope.ProjectRootAllowlist {
			if id, ok := roots[rp]; ok && !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		if len(ids) == 0 {
			return "", true, nil
		}
	} else {
		// No allowlist → start from every project, then subtract denied.
		for _, id := range roots {
			ids = append(ids, id)
		}
	}
	if len(scope.ProjectRootDenylist) > 0 {
		denied := make(map[int64]bool)
		for _, rp := range scope.ProjectRootDenylist {
			if id, ok := roots[rp]; ok {
				denied[id] = true
			}
		}
		filtered := ids[:0]
		for _, id := range ids {
			if !denied[id] {
				filtered = append(filtered, id)
			}
		}
		ids = filtered
		if len(ids) == 0 {
			return "", true, nil
		}
	}
	var b strings.Builder
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatInt(id, 10))
	}
	return b.String(), false, nil
}

// projectIDsByRoot returns the {root_path → project_id} map.
func (s *Store) projectIDsByRoot(ctx context.Context) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, root_path FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id int64
		var rp string
		if err := rows.Scan(&id, &rp); err != nil {
			return nil, err
		}
		out[rp] = id
	}
	return out, rows.Err()
}

// jsonSize returns the marshalled byte length of v, used to budget a batch.
func jsonSize(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// closeRows closes rows and returns the first of any iteration or close error.
func closeRows(rows *sql.Rows) error {
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	return rows.Close()
}

// readMeta returns the schema_meta value for key, or "" if absent.
func (s *Store) readMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}
