package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"

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

// SelectUnpushedSince reads the next batch of content-free rows above the
// given cursor, in table order (sessions, actions, api_turns, token_usage),
// stopping once the estimated JSON size would exceed maxBytes (a single
// oversized row is still included if the batch is otherwise empty, to
// guarantee forward progress). orgID/userEmail are the enrolled identity and
// are stamped onto every row — the push is attributed to the enrolled user
// regardless of the row's locally-stored attribution columns. The returned
// Cursor carries each table's new high-water id for the rows included.
//
// Only the allowed, content-free columns are selected; raw_tool_input,
// raw_tool_output, preceding_reasoning, error_message, and prompt bodies are
// never read here. This is the single SQL seam where the privacy posture is
// enforced (spec §1.5); the privacy invariant test asserts it.
func (s *Store) SelectUnpushedSince(ctx context.Context, cur PushCursor, maxBytes int64, orgID, userEmail string) (PushBatch, error) {
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

	// --- sessions ---
	if !budgetHit {
		rows, err := s.db.QueryContext(ctx,
			`SELECT s.rowid, s.id, p.root_path, COALESCE(p.git_remote,''), s.tool,
			        COALESCE(s.model,''), COALESCE(s.git_branch,''), s.started_at,
			        COALESCE(s.ended_at,''), COALESCE(s.total_actions,0)
			   FROM sessions s JOIN projects p ON s.project_id = p.id
			  WHERE s.rowid > ? ORDER BY s.rowid ASC`, cur.Sessions)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: sessions: %w", err)
		}
		for rows.Next() {
			var rowid int64
			var r orgcontract.SessionRow
			if err := rows.Scan(&rowid, &r.ID, &r.ProjectRoot, &r.GitRemote, &r.Tool,
				&r.Model, &r.GitBranch, &r.StartedAt, &r.EndedAt, &r.TotalActions); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan session: %w", err)
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
		rows, err := s.db.QueryContext(ctx,
			`SELECT a.id, a.session_id, COALESCE(a.source_file,''), COALESCE(a.source_event_id,''),
			        a.timestamp, a.tool, a.action_type, COALESCE(a.target,''),
			        COALESCE(a.turn_index,0), COALESCE(a.success,1), COALESCE(a.duration_ms,0),
			        COALESCE(a.is_sidechain,0)
			   FROM actions a WHERE a.id > ? ORDER BY a.id ASC`, cur.Actions)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: actions: %w", err)
		}
		for rows.Next() {
			var id, success, sidechain int64
			var r orgcontract.ActionRow
			if err := rows.Scan(&id, &r.SessionID, &r.SourceFile, &r.SourceEventID,
				&r.Timestamp, &r.Tool, &r.ActionType, &r.Target, &r.TurnIndex,
				&success, &r.DurationMs, &sidechain); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan action: %w", err)
			}
			r.Success = success != 0
			r.IsSidechain = sidechain != 0
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
		rows, err := s.db.QueryContext(ctx,
			`SELECT t.id, COALESCE(t.session_id,''), COALESCE(p.root_path,''), t.timestamp,
			        t.provider, COALESCE(t.model,''), COALESCE(t.request_id,''),
			        t.input_tokens, t.output_tokens, COALESCE(t.cache_read_tokens,0),
			        COALESCE(t.cache_creation_tokens,0), COALESCE(t.cache_creation_1h_tokens,0),
			        COALESCE(t.web_search_requests,0), COALESCE(t.cost_usd,0),
			        COALESCE(t.message_count,0), COALESCE(t.tool_use_count,0),
			        COALESCE(t.system_prompt_hash,''), COALESCE(t.message_prefix_hash,''),
			        COALESCE(t.time_to_first_token_ms,0), COALESCE(t.total_response_ms,0),
			        COALESCE(t.stop_reason,''), COALESCE(t.http_status,0), COALESCE(t.error_class,'')
			   FROM api_turns t LEFT JOIN projects p ON t.project_id = p.id
			  WHERE t.id > ? ORDER BY t.id ASC`, cur.APITurns)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: api_turns: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.APITurnRow
			if err := rows.Scan(&id, &r.SessionID, &r.ProjectRoot, &r.Timestamp, &r.Provider,
				&r.Model, &r.RequestID, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens,
				&r.CacheCreationTokens, &r.CacheCreation1hTokens, &r.WebSearchRequests, &r.CostUSD,
				&r.MessageCount, &r.ToolUseCount, &r.SystemPromptHash, &r.MessagePrefixHash,
				&r.TimeToFirstTokenMS, &r.TotalResponseMS, &r.StopReason, &r.HTTPStatus,
				&r.ErrorClass); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan api_turn: %w", err)
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
		rows, err := s.db.QueryContext(ctx,
			`SELECT tu.id, tu.session_id, COALESCE(p.root_path,''), tu.timestamp, tu.tool,
			        COALESCE(tu.model,''), COALESCE(tu.input_tokens,0), COALESCE(tu.output_tokens,0),
			        COALESCE(tu.cache_read_tokens,0), COALESCE(tu.cache_creation_tokens,0),
			        COALESCE(tu.cache_creation_1h_tokens,0), COALESCE(tu.reasoning_tokens,0),
			        COALESCE(tu.web_search_requests,0), COALESCE(tu.estimated_cost_usd,0),
			        tu.source, COALESCE(tu.reliability,'unknown'),
			        COALESCE(tu.source_file,''), COALESCE(tu.source_event_id,'')
			   FROM token_usage tu
			   LEFT JOIN sessions s ON tu.session_id = s.id
			   LEFT JOIN projects p ON s.project_id = p.id
			  WHERE tu.id > ? ORDER BY tu.id ASC`, cur.TokenUsage)
		if err != nil {
			return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: token_usage: %w", err)
		}
		for rows.Next() {
			var id int64
			var r orgcontract.TokenUsageRow
			if err := rows.Scan(&id, &r.SessionID, &r.ProjectRoot, &r.Timestamp, &r.Tool,
				&r.Model, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens,
				&r.CacheCreation1hTokens, &r.ReasoningTokens, &r.WebSearchRequests, &r.EstimatedCostUSD,
				&r.Source, &r.Reliability, &r.SourceFile, &r.SourceEventID); err != nil {
				_ = rows.Close()
				return PushBatch{}, fmt.Errorf("store.SelectUnpushedSince: scan token_usage: %w", err)
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
