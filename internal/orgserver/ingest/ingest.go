package ingest

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Result reports how a push batch landed: rows newly stored vs rows already
// present (deduplicated by composite key).
type Result struct {
	Accepted int64
	Deduped  int64
}

// Push ingests a push envelope into the server data tables under one
// transaction. Every row is attributed to userID (the dedup-key user_id
// component and pushed_by_user_id) and stamped pushedAt. Rows whose composite
// key already exists are ignored. The org_id / user_email carried on each row
// (stamped by the agent at read time) are persisted as-is for org-scoped
// queries; user_id is always the authenticated pusher, never client-supplied.
func Push(ctx context.Context, db *sql.DB, env orgcontract.PushEnvelope, userID, pushedAt string) (Result, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("ingest.Push: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var res Result
	total := int64(len(env.Sessions) + len(env.Actions) + len(env.APITurns) + len(env.TokenUsage))

	for _, s := range env.Sessions {
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO sessions
			   (id, user_id, org_id, user_email, project_root, git_remote, tool, model,
			    git_branch, started_at, ended_at, total_actions, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.ID, userID, s.OrgID, s.UserEmail, s.ProjectRoot, s.GitRemote, s.Tool, s.Model,
			s.GitBranch, s.StartedAt, nullIfEmpty(s.EndedAt), s.TotalActions, pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: session %s: %w", s.ID, err)
		}
		res.Accepted += n
	}

	for _, a := range env.Actions {
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO actions
			   (source_file, source_event_id, user_id, session_id, org_id, user_email, timestamp,
			    tool, action_type, target, turn_index, success, duration_ms, is_sidechain,
			    pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.SourceFile, a.SourceEventID, userID, a.SessionID, a.OrgID, a.UserEmail, a.Timestamp,
			a.Tool, a.ActionType, a.Target, a.TurnIndex, boolToInt(a.Success), a.DurationMs,
			boolToInt(a.IsSidechain), pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: action %s/%s: %w", a.SourceFile, a.SourceEventID, err)
		}
		res.Accepted += n
	}

	for _, t := range env.APITurns {
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO api_turns
			   (user_id, org_id, user_email, session_id, project_root, timestamp, provider, model,
			    request_id, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
			    cache_creation_1h_tokens, web_search_requests, cost_usd, message_count, tool_use_count,
			    system_prompt_hash, message_prefix_hash, time_to_first_token_ms, total_response_ms,
			    stop_reason, http_status, error_class, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			userID, t.OrgID, t.UserEmail, t.SessionID, t.ProjectRoot, t.Timestamp, t.Provider, t.Model,
			t.RequestID, t.InputTokens, t.OutputTokens, t.CacheReadTokens, t.CacheCreationTokens,
			t.CacheCreation1hTokens, t.WebSearchRequests, t.CostUSD, t.MessageCount, t.ToolUseCount,
			t.SystemPromptHash, t.MessagePrefixHash, t.TimeToFirstTokenMS, t.TotalResponseMS,
			t.StopReason, t.HTTPStatus, t.ErrorClass, pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: api_turn %s: %w", t.RequestID, err)
		}
		res.Accepted += n
	}

	for _, u := range env.TokenUsage {
		n, err := exec(ctx, tx,
			`INSERT OR IGNORE INTO token_usage
			   (source_file, source_event_id, user_id, org_id, user_email, session_id, project_root,
			    timestamp, tool, model, input_tokens, output_tokens, cache_read_tokens,
			    cache_creation_tokens, cache_creation_1h_tokens, reasoning_tokens, web_search_requests,
			    estimated_cost_usd, source, reliability, pushed_at, pushed_by_user_id)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			u.SourceFile, u.SourceEventID, userID, u.OrgID, u.UserEmail, u.SessionID, u.ProjectRoot,
			u.Timestamp, u.Tool, u.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens,
			u.CacheCreationTokens, u.CacheCreation1hTokens, u.ReasoningTokens, u.WebSearchRequests,
			u.EstimatedCostUSD, u.Source, u.Reliability, pushedAt, userID)
		if err != nil {
			return Result{}, fmt.Errorf("ingest.Push: token_usage %s/%s: %w", u.SourceFile, u.SourceEventID, err)
		}
		res.Accepted += n
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("ingest.Push: commit: %w", err)
	}
	res.Deduped = total - res.Accepted
	return res, nil
}

// exec runs an INSERT OR IGNORE and returns 1 if a row was inserted, 0 if the
// composite key already existed (the dedup signal).
func exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	r, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
