package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/failure"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// extraBuiltinTools returns the second batch of MCP tools (spec §11.2).
// Combined with the four in tools.go they make the full 12-tool set.
func extraBuiltinTools(db *sql.DB, engine *cost.Engine) []Tool {
	return []Tool{
		newGetActionDetailsTool(db),
		newGetFailureContextTool(db),
		newGetLastTestResultTool(db),
		newGetCostSummaryTool(db, engine),
		newCheckCommandFreshnessTool(db),
		newGetSessionRecoveryContextTool(db),
		newGetProjectPatternsTool(db),
		newGetRedundancyReportTool(db),
	}
}

// -----------------------------------------------------------------------------
// get_action_details
// -----------------------------------------------------------------------------

type getActionDetailsTool struct{ db *sql.DB }

func newGetActionDetailsTool(db *sql.DB) Tool { return &getActionDetailsTool{db: db} }

func (*getActionDetailsTool) Name() string { return "get_action_details" }
func (*getActionDetailsTool) Description() string {
	return "Full row(s) for one or more action IDs (typically obtained from search_past_outputs hits). Returns target, raw input, error message, freshness, and the action_excerpt body when present."
}
func (*getActionDetailsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action_ids": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "integer"},
				"description": "List of action IDs to fetch. Max 25 per call.",
			},
		},
		"required": []string{"action_ids"},
	}
}

type actionDetail struct {
	ID            int64     `json:"id"`
	SessionID     string    `json:"session_id"`
	Tool          string    `json:"tool"`
	ActionType    string    `json:"action_type"`
	Target        string    `json:"target"`
	Timestamp     time.Time `json:"timestamp"`
	Success       bool      `json:"success"`
	Freshness     string    `json:"freshness,omitempty"`
	DurationMs    int64     `json:"duration_ms,omitempty"`
	ErrorMessage  string    `json:"error_message,omitempty"`
	RawToolName   string    `json:"raw_tool_name,omitempty"`
	RawToolInput  string    `json:"raw_tool_input,omitempty"`
	Excerpt       string    `json:"excerpt,omitempty"`
	ContentHash   string    `json:"content_hash,omitempty"`
	FileSizeBytes int64     `json:"file_size_bytes,omitempty"`
}

func (t *getActionDetailsTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ActionIDs []int64 `json:"action_ids"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.ActionIDs) == 0 {
		return nil, errors.New("action_ids is required and must be non-empty")
	}
	if len(args.ActionIDs) > 25 {
		args.ActionIDs = args.ActionIDs[:25]
	}
	placeholders := make([]string, len(args.ActionIDs))
	queryArgs := make([]any, len(args.ActionIDs))
	for i, id := range args.ActionIDs {
		placeholders[i] = "?"
		queryArgs[i] = id
	}
	q := fmt.Sprintf(
		`SELECT a.id, a.session_id, a.tool, a.action_type, COALESCE(a.target,''),
		        a.timestamp, a.success, COALESCE(a.freshness,''), COALESCE(a.duration_ms,0),
		        COALESCE(a.error_message,''), COALESCE(a.raw_tool_name,''), COALESCE(a.raw_tool_input,''),
		        COALESCE(ae.excerpt,''), COALESCE(a.content_hash,''), COALESCE(a.file_size_bytes,0)
		 FROM actions a LEFT JOIN action_excerpts ae ON ae.action_id = a.id
		 WHERE a.id IN (%s)
		 ORDER BY a.timestamp DESC`,
		strings.Join(placeholders, ","),
	)
	rows, err := t.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []actionDetail
	for rows.Next() {
		var d actionDetail
		var ts string
		var success int
		if err := rows.Scan(&d.ID, &d.SessionID, &d.Tool, &d.ActionType, &d.Target,
			&ts, &success, &d.Freshness, &d.DurationMs,
			&d.ErrorMessage, &d.RawToolName, &d.RawToolInput,
			&d.Excerpt, &d.ContentHash, &d.FileSizeBytes); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		d.Success = success == 1
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			d.Timestamp = t
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return map[string]any{"actions": out, "count": len(out)}, nil
}

// -----------------------------------------------------------------------------
// get_failure_context
// -----------------------------------------------------------------------------

type getFailureContextTool struct{ db *sql.DB }

func newGetFailureContextTool(db *sql.DB) Tool { return &getFailureContextTool{db: db} }

func (*getFailureContextTool) Name() string { return "get_failure_context" }
func (*getFailureContextTool) Description() string {
	return "Previous failures of a command: error category, error message, retry count, and whether it eventually succeeded. Use to learn from past mistakes before re-running a flaky or expensive command."
}
func (*getFailureContextTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The exact command string to look up — hashed and normalized internally.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max rows to return (default 10, max 50).",
				"minimum":     1,
				"maximum":     50,
			},
		},
		"required": []string{"command"},
	}
}

type failureRow struct {
	ID                  int64     `json:"id"`
	SessionID           string    `json:"session_id"`
	Timestamp           time.Time `json:"timestamp"`
	CommandSummary      string    `json:"command_summary"`
	ErrorCategory       string    `json:"error_category,omitempty"`
	ErrorMessage        string    `json:"error_message,omitempty"`
	RetryCount          int       `json:"retry_count"`
	EventuallySucceeded bool      `json:"eventually_succeeded"`
}

func (t *getFailureContextTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Command string `json:"command"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return nil, errors.New("command is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	hash := failure.CommandHash(args.Command)
	rows, err := t.db.QueryContext(ctx,
		`SELECT id, session_id, timestamp, command_summary, COALESCE(error_category,''),
		        COALESCE(error_message,''), retry_count, eventually_succeeded
		 FROM failure_context
		 WHERE command_hash = ?
		 ORDER BY timestamp DESC LIMIT ?`,
		hash, limit)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []failureRow
	for rows.Next() {
		var r failureRow
		var ts string
		var es int
		if err := rows.Scan(&r.ID, &r.SessionID, &ts, &r.CommandSummary, &r.ErrorCategory, &r.ErrorMessage, &r.RetryCount, &es); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.EventuallySucceeded = es == 1
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			r.Timestamp = t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return map[string]any{
		"command":      args.Command,
		"command_hash": hash,
		"failures":     out,
		"count":        len(out),
	}, nil
}

// -----------------------------------------------------------------------------
// get_last_test_result
// -----------------------------------------------------------------------------

type getLastTestResultTool struct{ db *sql.DB }

func newGetLastTestResultTool(db *sql.DB) Tool { return &getLastTestResultTool{db: db} }

func (*getLastTestResultTool) Name() string { return "get_last_test_result" }
func (*getLastTestResultTool) Description() string {
	return "Most recent test-runner action and its outcome: command, success, error_message, freshness. Heuristic match against common test runners (go test, pytest, jest, cargo test, rspec, mocha, bun test, npm test, yarn test)."
}
func (*getLastTestResultTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_root": map[string]any{
				"type":        "string",
				"description": "Optional — restrict to one project.",
			},
		},
	}
}

type lastTestResult struct {
	ActionID     int64     `json:"action_id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	Timestamp    time.Time `json:"timestamp,omitempty"`
	Command      string    `json:"command,omitempty"`
	Success      bool      `json:"success"`
	ErrorMessage string    `json:"error_message,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	Found        bool      `json:"found"`
}

// testRunnerPatterns is the set of substrings considered "running tests".
// The list is intentionally conservative — false positives waste a tool
// call but false negatives mean the model re-runs the test.
var testRunnerPatterns = []string{
	"go test", "pytest", "jest", "cargo test", "rspec", "mocha",
	"bun test", "npm test", "npm run test", "yarn test", "yarn run test",
	"phpunit", "rails test", "mvn test", "gradle test", "ctest",
}

func (t *getLastTestResultTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ProjectRoot string `json:"project_root"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	q := `SELECT a.id, a.session_id, a.timestamp, COALESCE(a.target,''), a.success, COALESCE(a.error_message,''), a.tool
	      FROM actions a JOIN projects p ON p.id = a.project_id
	      WHERE a.action_type = 'run_command' AND (`
	var conds []string
	var queryArgs []any
	for _, pat := range testRunnerPatterns {
		conds = append(conds, "a.target LIKE ?")
		queryArgs = append(queryArgs, "%"+pat+"%")
	}
	q += strings.Join(conds, " OR ") + ")"
	if args.ProjectRoot != "" {
		q += " AND p.root_path = ?"
		queryArgs = append(queryArgs, args.ProjectRoot)
	}
	q += " ORDER BY a.timestamp DESC LIMIT 1"

	row := t.db.QueryRowContext(ctx, q, queryArgs...)
	var res lastTestResult
	var ts string
	var success int
	err := row.Scan(&res.ActionID, &res.SessionID, &ts, &res.Command, &success, &res.ErrorMessage, &res.Tool)
	if errors.Is(err, sql.ErrNoRows) {
		return lastTestResult{Found: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	res.Success = success == 1
	res.Found = true
	if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		res.Timestamp = parsed
	}
	return res, nil
}

// -----------------------------------------------------------------------------
// get_cost_summary
// -----------------------------------------------------------------------------

type getCostSummaryTool struct {
	db     *sql.DB
	engine *cost.Engine
}

func newGetCostSummaryTool(db *sql.DB, engine *cost.Engine) Tool {
	return &getCostSummaryTool{db: db, engine: engine}
}

func (*getCostSummaryTool) Name() string { return "get_cost_summary" }
func (*getCostSummaryTool) Description() string {
	return "Token / cost totals from api_turns (proxy = accurate) and token_usage (logs = approximate). Group by session, model, day, project, or tool. Use to spot expensive models or runaway sessions. Costs are computed via the embedded pricing table (spec §15.4) so rows are non-zero even when api_turns.cost_usd is NULL."
}
func (*getCostSummaryTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"group_by": map[string]any{
				"type":        "string",
				"description": "One of: session, model, day, project, tool, none. Default: model.",
			},
			"days": map[string]any{
				"type":        "integer",
				"description": "Restrict to the last N days. Default 30.",
				"minimum":     1,
				"maximum":     365,
			},
			"project_root": map[string]any{
				"type":        "string",
				"description": "Optional project filter.",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "Token source: auto (default — prefer proxy, fall back to JSONL), proxy, jsonl.",
			},
		},
	}
}

func (t *getCostSummaryTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		GroupBy     string `json:"group_by"`
		Days        int    `json:"days"`
		ProjectRoot string `json:"project_root"`
		Source      string `json:"source"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.Days <= 0 {
		args.Days = 30
	}
	if args.Days > 365 {
		args.Days = 365
	}

	groupBy, err := parseMCPGroupBy(args.GroupBy)
	if err != nil {
		return nil, err
	}
	source, err := parseMCPSource(args.Source)
	if err != nil {
		return nil, err
	}

	summary, err := t.engine.Summary(ctx, t.db, cost.Options{
		Days:        args.Days,
		GroupBy:     groupBy,
		Source:      source,
		ProjectRoot: args.ProjectRoot,
	})
	if err != nil {
		return nil, err
	}

	rows := make([]map[string]any, 0, len(summary.Rows))
	for _, r := range summary.Rows {
		rows = append(rows, map[string]any{
			"key":                   r.Key,
			"input_tokens":          r.Tokens.Input,
			"output_tokens":         r.Tokens.Output,
			"cache_read_tokens":     r.Tokens.CacheRead,
			"cache_creation_tokens": r.Tokens.CacheCreation,
			"cost_usd":              r.CostUSD,
			"turn_count":            r.TurnCount,
			"source":                r.Source,
			"reliability":           r.Reliability,
			"unknown_models":        r.UnknownModels,
		})
	}
	return map[string]any{
		"group_by":            args.GroupBy,
		"days":                args.Days,
		"source":              string(summary.Source),
		"rows":                rows,
		"total_input_tokens":  summary.TotalTokens.Input,
		"total_output_tokens": summary.TotalTokens.Output,
		"total_cost_usd":      summary.TotalCost,
		"reliability":         summary.Reliability,
		"unknown_model_count": summary.UnknownModelCount,
	}, nil
}

func parseMCPGroupBy(s string) (cost.GroupBy, error) {
	switch s {
	case "", "model":
		return cost.GroupByModel, nil
	case "session":
		return cost.GroupBySession, nil
	case "day":
		return cost.GroupByDay, nil
	case "project":
		return cost.GroupByProject, nil
	case "tool":
		return cost.GroupByTool, nil
	case "none":
		return cost.GroupByNone, nil
	default:
		return "", fmt.Errorf("group_by must be one of: model, session, day, project, tool, none; got %q", s)
	}
}

func parseMCPSource(s string) (cost.Source, error) {
	switch s {
	case "", "auto":
		return cost.SourceAuto, nil
	case "proxy":
		return cost.SourceProxy, nil
	case "jsonl":
		return cost.SourceJSONL, nil
	default:
		return "", fmt.Errorf("source must be one of: auto, proxy, jsonl; got %q", s)
	}
}

// -----------------------------------------------------------------------------
// check_command_freshness
// -----------------------------------------------------------------------------

type checkCommandFreshnessTool struct{ db *sql.DB }

func newCheckCommandFreshnessTool(db *sql.DB) Tool {
	return &checkCommandFreshnessTool{db: db}
}

func (*checkCommandFreshnessTool) Name() string { return "check_command_freshness" }
func (*checkCommandFreshnessTool) Description() string {
	return "Has this command been run before, and if so when? Returns the most recent invocation's timestamp, success, and how many file edits have happened in this project since then. Use to decide whether to re-run an expensive command."
}
func (*checkCommandFreshnessTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The exact command string.",
			},
			"project_root": map[string]any{
				"type":        "string",
				"description": "Optional project filter.",
			},
		},
		"required": []string{"command"},
	}
}

type commandFreshnessResult struct {
	Command           string    `json:"command"`
	CommandHash       string    `json:"command_hash"`
	LastRanAt         time.Time `json:"last_ran_at,omitempty"`
	LastSuccess       bool      `json:"last_success"`
	LastSessionID     string    `json:"last_session_id,omitempty"`
	EditsSinceLastRun int       `json:"edits_since_last_run"`
	FileChangesSeen   bool      `json:"file_changes_seen"`
	NeverRun          bool      `json:"never_run"`
}

func (t *checkCommandFreshnessTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		Command     string `json:"command"`
		ProjectRoot string `json:"project_root"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return nil, errors.New("command is required")
	}
	hash := failure.CommandHash(args.Command)
	res := commandFreshnessResult{Command: args.Command, CommandHash: hash}

	q := `SELECT a.timestamp, a.success, a.session_id, a.project_id
	      FROM actions a
	      WHERE a.action_type = 'run_command' AND a.target_hash = (
	          SELECT lower(hex(?))  -- placeholder; SHA256(target) hex below
	      )`
	// We don't store the SHA256 of the *normalized command* anywhere
	// directly — actions.target_hash is sha256(target). For Phase 3 we
	// use the literal target match instead. Build the actual query.
	q = `SELECT a.timestamp, a.success, a.session_id, a.project_id
	     FROM actions a JOIN projects p ON p.id = a.project_id
	     WHERE a.action_type = 'run_command' AND a.target = ?`
	queryArgs := []any{args.Command}
	if args.ProjectRoot != "" {
		q += " AND p.root_path = ?"
		queryArgs = append(queryArgs, args.ProjectRoot)
	}
	q += " ORDER BY a.timestamp DESC LIMIT 1"

	var ts string
	var success int
	var pid int64
	err := t.db.QueryRowContext(ctx, q, queryArgs...).Scan(&ts, &success, &res.LastSessionID, &pid)
	if errors.Is(err, sql.ErrNoRows) {
		res.NeverRun = true
		return res, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query last run: %w", err)
	}
	res.LastSuccess = success == 1
	if parsed, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
		res.LastRanAt = parsed
	}

	// Count file edits in this project since LastRanAt.
	var n int
	err = t.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions
		 WHERE project_id = ?
		   AND action_type IN ('edit_file', 'write_file')
		   AND timestamp > ?`,
		pid, ts,
	).Scan(&n)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("count edits: %w", err)
	}
	res.EditsSinceLastRun = n
	res.FileChangesSeen = n > 0
	return res, nil
}

// -----------------------------------------------------------------------------
// get_session_recovery_context
// -----------------------------------------------------------------------------

type getSessionRecoveryContextTool struct{ db *sql.DB }

func newGetSessionRecoveryContextTool(db *sql.DB) Tool {
	return &getSessionRecoveryContextTool{db: db}
}

func (*getSessionRecoveryContextTool) Name() string { return "get_session_recovery_context" }
func (*getSessionRecoveryContextTool) Description() string {
	return "Post-compaction rebuild context: most recently modified files, recent failures, the latest user prompt, and (if a compaction event was captured) the file_state snapshot at compaction time. Use right after a context compaction to rebuild your view of the session."
}
func (*getSessionRecoveryContextTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type": "string",
			},
		},
		"required": []string{"session_id"},
	}
}

type recoveryContextResult struct {
	SessionID         string             `json:"session_id"`
	LastUserPrompt    string             `json:"last_user_prompt,omitempty"`
	RecentEditedFiles []string           `json:"recent_edited_files"`
	RecentFailures    []failureRow       `json:"recent_failures"`
	CompactionAt      time.Time          `json:"compaction_at,omitempty"`
	FileSnapshotJSON  string             `json:"file_snapshot_json,omitempty"`
	Counts            recoveryCounts     `json:"counts"`
	UnchangedFiles    []recoveryFileInfo `json:"unchanged_files,omitempty"`
}

type recoveryCounts struct {
	TotalActions int `json:"total_actions"`
	Failures     int `json:"failures"`
	EditedFiles  int `json:"edited_files"`
}

type recoveryFileInfo struct {
	Path        string    `json:"path"`
	ContentHash string    `json:"content_hash"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

func (t *getSessionRecoveryContextTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.SessionID == "" {
		return nil, errors.New("session_id is required")
	}
	res := recoveryContextResult{SessionID: args.SessionID}

	// Total actions + failures.
	_ = t.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN success=0 THEN 1 ELSE 0 END), 0)
		 FROM actions WHERE session_id = ?`, args.SessionID,
	).Scan(&res.Counts.TotalActions, &res.Counts.Failures)

	// Last user_prompt action's target (truncated text).
	var prompt sql.NullString
	_ = t.db.QueryRowContext(ctx,
		`SELECT target FROM actions
		 WHERE session_id = ? AND action_type = 'user_prompt'
		 ORDER BY timestamp DESC LIMIT 1`, args.SessionID,
	).Scan(&prompt)
	if prompt.Valid {
		res.LastUserPrompt = prompt.String
	}

	// Recent edited files (deduplicated, last 10).
	rows, err := t.db.QueryContext(ctx,
		`SELECT DISTINCT target FROM actions
		 WHERE session_id = ? AND action_type IN ('edit_file', 'write_file')
		 ORDER BY MAX(timestamp) OVER (PARTITION BY target) DESC LIMIT 10`,
		args.SessionID,
	)
	if err == nil {
		defer rows.Close()
		seen := map[string]bool{}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err == nil && !seen[p] {
				seen[p] = true
				res.RecentEditedFiles = append(res.RecentEditedFiles, p)
			}
		}
	}
	res.Counts.EditedFiles = len(res.RecentEditedFiles)

	// Recent failures in this session.
	frows, err := t.db.QueryContext(ctx,
		`SELECT id, session_id, timestamp, command_summary, COALESCE(error_category,''),
		        COALESCE(error_message,''), retry_count, eventually_succeeded
		 FROM failure_context WHERE session_id = ?
		 ORDER BY timestamp DESC LIMIT 5`, args.SessionID)
	if err == nil {
		defer frows.Close()
		for frows.Next() {
			var f failureRow
			var ts string
			var es int
			if err := frows.Scan(&f.ID, &f.SessionID, &ts, &f.CommandSummary, &f.ErrorCategory, &f.ErrorMessage, &f.RetryCount, &es); err != nil {
				continue
			}
			f.EventuallySucceeded = es == 1
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				f.Timestamp = t
			}
			res.RecentFailures = append(res.RecentFailures, f)
		}
	}

	// Latest compaction event for this session (if any).
	var compTS, snapJSON sql.NullString
	_ = t.db.QueryRowContext(ctx,
		`SELECT timestamp, COALESCE(file_state_snapshot,'')
		 FROM compaction_events WHERE session_id = ?
		 ORDER BY timestamp DESC LIMIT 1`,
		args.SessionID,
	).Scan(&compTS, &snapJSON)
	if compTS.Valid {
		if t, err := time.Parse(time.RFC3339Nano, compTS.String); err == nil {
			res.CompactionAt = t
		}
		res.FileSnapshotJSON = snapJSON.String
	}

	// Files in this project whose last_seen_at predates the latest action
	// in this session (i.e. not touched after the session started). These
	// are the "unchanged" files the model can skip re-reading.
	unrows, err := t.db.QueryContext(ctx,
		`SELECT fs.file_path, fs.content_hash, fs.last_seen_at
		 FROM file_state fs
		 JOIN actions a ON a.session_id = ? AND a.project_id = fs.project_id
		 WHERE fs.last_seen_at < (SELECT MAX(timestamp) FROM actions WHERE session_id = ?)
		 GROUP BY fs.file_path, fs.content_hash, fs.last_seen_at
		 ORDER BY fs.last_seen_at DESC LIMIT 20`,
		args.SessionID, args.SessionID)
	if err == nil {
		defer unrows.Close()
		for unrows.Next() {
			var fi recoveryFileInfo
			var ts string
			if err := unrows.Scan(&fi.Path, &fi.ContentHash, &ts); err == nil {
				if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					fi.LastSeenAt = t
				}
				res.UnchangedFiles = append(res.UnchangedFiles, fi)
			}
		}
	}
	return res, nil
}

// -----------------------------------------------------------------------------
// get_project_patterns
// -----------------------------------------------------------------------------

type getProjectPatternsTool struct{ db *sql.DB }

func newGetProjectPatternsTool(db *sql.DB) Tool { return &getProjectPatternsTool{db: db} }

func (*getProjectPatternsTool) Name() string { return "get_project_patterns" }
func (*getProjectPatternsTool) Description() string {
	return "Hot files (most-accessed paths), common commands (most-run command strings), and any patterns the intelligence layer has derived for this project. Use as a quick orientation when starting work in an unfamiliar repo."
}
func (*getProjectPatternsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_root": map[string]any{
				"type": "string",
			},
			"limit": map[string]any{
				"type":    "integer",
				"minimum": 1,
				"maximum": 50,
			},
		},
		"required": []string{"project_root"},
	}
}

type patternRow struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

type derivedPattern struct {
	Type           string  `json:"type"`
	Data           string  `json:"data"`
	Confidence     float64 `json:"confidence"`
	LastReinforced string  `json:"last_reinforced_at,omitempty"`
}

func (t *getProjectPatternsTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ProjectRoot string `json:"project_root"`
		Limit       int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ProjectRoot == "" {
		return nil, errors.New("project_root is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	var pid int64
	if err := t.db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE root_path = ?`, args.ProjectRoot,
	).Scan(&pid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]any{
				"project_root":     args.ProjectRoot,
				"hot_files":        []patternRow{},
				"common_commands":  []patternRow{},
				"derived_patterns": []derivedPattern{},
			}, nil
		}
		return nil, fmt.Errorf("project lookup: %w", err)
	}

	hot, err := topKeys(ctx, t.db,
		`SELECT target, COUNT(*) FROM actions
		 WHERE project_id = ? AND action_type IN ('read_file', 'edit_file', 'write_file') AND target != ''
		 GROUP BY target ORDER BY COUNT(*) DESC LIMIT ?`,
		pid, limit)
	if err != nil {
		return nil, err
	}
	commands, err := topKeys(ctx, t.db,
		`SELECT target, COUNT(*) FROM actions
		 WHERE project_id = ? AND action_type = 'run_command' AND target != ''
		 GROUP BY target ORDER BY COUNT(*) DESC LIMIT ?`,
		pid, limit)
	if err != nil {
		return nil, err
	}

	prows, err := t.db.QueryContext(ctx,
		`SELECT pattern_type, pattern_data, COALESCE(confidence,0), COALESCE(last_reinforced_at,'')
		 FROM project_patterns WHERE project_id = ?
		 ORDER BY confidence DESC LIMIT ?`,
		pid, limit)
	if err != nil {
		return nil, fmt.Errorf("derived patterns: %w", err)
	}
	defer prows.Close()
	var derived []derivedPattern
	for prows.Next() {
		var d derivedPattern
		if err := prows.Scan(&d.Type, &d.Data, &d.Confidence, &d.LastReinforced); err != nil {
			return nil, fmt.Errorf("scan derived: %w", err)
		}
		derived = append(derived, d)
	}

	return map[string]any{
		"project_root":     args.ProjectRoot,
		"hot_files":        hot,
		"common_commands":  commands,
		"derived_patterns": derived,
	}, nil
}

func topKeys(ctx context.Context, db *sql.DB, query string, queryArgs ...any) ([]patternRow, error) {
	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("topKeys: %w", err)
	}
	defer rows.Close()
	var out []patternRow
	for rows.Next() {
		var p patternRow
		if err := rows.Scan(&p.Key, &p.Count); err != nil {
			return nil, fmt.Errorf("topKeys scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// -----------------------------------------------------------------------------
// get_redundancy_report
// -----------------------------------------------------------------------------

type getRedundancyReportTool struct{ db *sql.DB }

func newGetRedundancyReportTool(db *sql.DB) Tool { return &getRedundancyReportTool{db: db} }

func (*getRedundancyReportTool) Name() string { return "get_redundancy_report" }
func (*getRedundancyReportTool) Description() string {
	return "Counts of stale file reads (read again with no intervening change) and repeated commands within recent sessions. Use to identify wasted tool calls. Phase 4 will add token-cost estimates."
}
func (*getRedundancyReportTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_root": map[string]any{
				"type":        "string",
				"description": "Optional project filter.",
			},
			"days": map[string]any{
				"type":        "integer",
				"description": "Look back N days. Default 7.",
				"minimum":     1,
				"maximum":     90,
			},
		},
	}
}

type redundancyResult struct {
	ProjectRoot      string       `json:"project_root,omitempty"`
	Days             int          `json:"days"`
	StaleReads       int          `json:"stale_reads"`
	ChangedBySelf    int          `json:"changed_by_self_reads"`
	RepeatedCommands int          `json:"repeated_commands"`
	TopStaleFiles    []patternRow `json:"top_stale_files"`
	TopRepeatedCmds  []patternRow `json:"top_repeated_commands"`
}

func (t *getRedundancyReportTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args struct {
		ProjectRoot string `json:"project_root"`
		Days        int    `json:"days"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.Days <= 0 {
		args.Days = 7
	}
	if args.Days > 90 {
		args.Days = 90
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -args.Days).Format(time.RFC3339Nano)
	res := redundancyResult{ProjectRoot: args.ProjectRoot, Days: args.Days}

	whereProj := ""
	queryArgs := []any{cutoff}
	if args.ProjectRoot != "" {
		whereProj = " AND p.root_path = ?"
		queryArgs = append(queryArgs, args.ProjectRoot)
	}

	// Stale reads.
	_ = t.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions a JOIN projects p ON p.id = a.project_id
		 WHERE a.action_type = 'read_file' AND a.freshness = 'stale' AND a.timestamp >= ?`+whereProj,
		queryArgs...,
	).Scan(&res.StaleReads)
	_ = t.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM actions a JOIN projects p ON p.id = a.project_id
		 WHERE a.action_type = 'read_file' AND a.freshness = 'changed_by_self' AND a.timestamp >= ?`+whereProj,
		queryArgs...,
	).Scan(&res.ChangedBySelf)

	// Repeated commands within the same session (count - 1 per group).
	_ = t.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(c - 1), 0) FROM (
		   SELECT COUNT(*) AS c FROM actions a JOIN projects p ON p.id = a.project_id
		   WHERE a.action_type = 'run_command' AND a.timestamp >= ?`+whereProj+`
		   GROUP BY a.session_id, a.target_hash
		 )`,
		queryArgs...,
	).Scan(&res.RepeatedCommands)

	// Top offenders (limited to 10 each).
	stale, err := topKeys(ctx, t.db,
		`SELECT a.target, COUNT(*) FROM actions a JOIN projects p ON p.id = a.project_id
		 WHERE a.action_type = 'read_file' AND a.freshness = 'stale' AND a.timestamp >= ?`+whereProj+`
		 GROUP BY a.target ORDER BY COUNT(*) DESC LIMIT 10`,
		queryArgs...,
	)
	if err != nil {
		return nil, err
	}
	res.TopStaleFiles = stale
	repeated, err := topKeys(ctx, t.db,
		`SELECT a.target, COUNT(*) FROM actions a JOIN projects p ON p.id = a.project_id
		 WHERE a.action_type = 'run_command' AND a.timestamp >= ?`+whereProj+`
		 GROUP BY a.session_id, a.target_hash, a.target HAVING COUNT(*) > 1
		 ORDER BY COUNT(*) DESC LIMIT 10`,
		queryArgs...,
	)
	if err != nil {
		return nil, err
	}
	res.TopRepeatedCmds = repeated
	return res, nil
}
