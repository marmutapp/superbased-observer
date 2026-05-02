package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/codegraph"
	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
	"github.com/marmutapp/superbased-observer/internal/freshness"
)

// builtinTools returns the set of tools registered by default. Each tool
// holds its own *sql.DB reference so invocations are thread-safe and don't
// need the server's mutex.
func builtinTools(db *sql.DB, cg *codegraph.Client) []Tool {
	return []Tool{
		newCheckFileFreshnessTool(db, cg),
		newGetFileHistoryTool(db, cg),
		newGetSessionSummaryTool(db),
		newSearchPastOutputsTool(db),
	}
}

// -----------------------------------------------------------------------------
// check_file_freshness
// -----------------------------------------------------------------------------

type checkFileFreshnessTool struct {
	db *sql.DB
	cg *codegraph.Client
}

func newCheckFileFreshnessTool(db *sql.DB, cg *codegraph.Client) Tool {
	return &checkFileFreshnessTool{db: db, cg: cg}
}

func (*checkFileFreshnessTool) Name() string { return "check_file_freshness" }
func (*checkFileFreshnessTool) Description() string {
	return "Report whether a file has changed since the observer last saw it. Returns current hash, last-observed hash, and a freshness classification. Use before re-reading to avoid redundant file I/O."
}
func (*checkFileFreshnessTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_root": map[string]any{
				"type":        "string",
				"description": "Absolute path to the project root (git root or working directory).",
			},
			"file_path": map[string]any{
				"type":        "string",
				"description": "Absolute path, or project-relative path, to the file to check.",
			},
		},
		"required": []string{"project_root", "file_path"},
	}
}

type checkFileFreshnessArgs struct {
	ProjectRoot string `json:"project_root"`
	FilePath    string `json:"file_path"`
}

// FileStructure holds codegraph enrichment attached to file-scoped tool
// results when the codebase-memory-mcp graph DB is available.
type FileStructure struct {
	Functions []string `json:"functions,omitempty"`
	Callers   []string `json:"callers,omitempty"`
	Imports   []string `json:"imports,omitempty"`
}

type checkFileFreshnessResult struct {
	File           string         `json:"file"`
	ProjectRoot    string         `json:"project_root"`
	Freshness      string         `json:"freshness"`
	ChangeDetected bool           `json:"change_detected"`
	CurrentHash    string         `json:"current_hash,omitempty"`
	LastHash       string         `json:"last_hash,omitempty"`
	LastSeenAt     time.Time      `json:"last_seen_at,omitempty"`
	LastActionType string         `json:"last_action_type,omitempty"`
	FileSizeBytes  int64          `json:"file_size_bytes,omitempty"`
	Structure      *FileStructure `json:"structure,omitempty"`
}

func (t *checkFileFreshnessTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args checkFileFreshnessArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ProjectRoot == "" || args.FilePath == "" {
		return nil, errors.New("project_root and file_path are required")
	}
	abs := args.FilePath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(args.ProjectRoot, abs)
	}

	var projectID int64
	err := t.db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE root_path = ?`, args.ProjectRoot,
	).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		// No observer history for this project — report unknown but still hash
		// the current file so the caller can compare in a subsequent turn.
		classifier := freshness.New(t.db, freshness.Options{})
		obs, _ := classifier.Classify(ctx, 0, "", "read_file", abs)
		return checkFileFreshnessResult{
			File:          abs,
			ProjectRoot:   args.ProjectRoot,
			Freshness:     "unknown",
			CurrentHash:   obs.ContentHash,
			FileSizeBytes: obs.FileSizeBytes,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup project: %w", err)
	}

	classifier := freshness.New(t.db, freshness.Options{})
	obs, err := classifier.Classify(ctx, projectID, "", "read_file", abs)
	if err != nil {
		return nil, fmt.Errorf("classify: %w", err)
	}

	res := checkFileFreshnessResult{
		File:           abs,
		ProjectRoot:    args.ProjectRoot,
		Freshness:      obs.Freshness,
		ChangeDetected: obs.ChangeDetected,
		CurrentHash:    obs.ContentHash,
		FileSizeBytes:  obs.FileSizeBytes,
	}

	// Fetch the most recent file_state entry for a last_hash / last_seen_at
	// hint. May not exist if the file is new.
	var lastHash, lastAction, lastSeen string
	err = t.db.QueryRowContext(ctx,
		`SELECT content_hash, last_action_type, last_seen_at
		 FROM file_state WHERE project_id = ? AND file_path = ?`,
		projectID, abs,
	).Scan(&lastHash, &lastAction, &lastSeen)
	if err == nil {
		res.LastHash = lastHash
		res.LastActionType = lastAction
		if t, err := time.Parse(time.RFC3339Nano, lastSeen); err == nil {
			res.LastSeenAt = t
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("lookup file_state: %w", err)
	}
	if st := enrichStructure(ctx, t.cg, abs); st != nil {
		res.Structure = st
	}
	return res, nil
}

// -----------------------------------------------------------------------------
// get_file_history
// -----------------------------------------------------------------------------

type getFileHistoryTool struct {
	db *sql.DB
	cg *codegraph.Client
}

func newGetFileHistoryTool(db *sql.DB, cg *codegraph.Client) Tool {
	return &getFileHistoryTool{db: db, cg: cg}
}

func (*getFileHistoryTool) Name() string { return "get_file_history" }
func (*getFileHistoryTool) Description() string {
	return "Recent read/edit/write actions on a specific file across all sessions. Use to see whether you (or a teammate) already touched this file recently and how."
}
func (*getFileHistoryTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_root": map[string]any{
				"type":        "string",
				"description": "Absolute path to the project root. Required when file_path is project-relative.",
			},
			"file_path": map[string]any{
				"type":        "string",
				"description": "Absolute or project-relative path.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum rows to return (default 20, max 100).",
				"minimum":     1,
				"maximum":     100,
			},
		},
		"required": []string{"file_path"},
	}
}

type getFileHistoryArgs struct {
	ProjectRoot string `json:"project_root"`
	FilePath    string `json:"file_path"`
	Limit       int    `json:"limit"`
}

type fileHistoryEntry struct {
	ActionID    int64     `json:"action_id"`
	SessionID   string    `json:"session_id"`
	Tool        string    `json:"tool"`
	ActionType  string    `json:"action_type"`
	Timestamp   time.Time `json:"timestamp"`
	Freshness   string    `json:"freshness,omitempty"`
	Success     bool      `json:"success"`
	ContentHash string    `json:"content_hash,omitempty"`
}

type getFileHistoryResult struct {
	File      string             `json:"file"`
	Entries   []fileHistoryEntry `json:"entries"`
	Count     int                `json:"count"`
	Structure *FileStructure     `json:"structure,omitempty"`
}

func (t *getFileHistoryTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args getFileHistoryArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.FilePath == "" {
		return nil, errors.New("file_path is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// Accept either absolute paths (stored as-is for files outside the repo)
	// or project-relative paths (how most actions are stored). Try both.
	var paths []string
	paths = append(paths, args.FilePath)
	if args.ProjectRoot != "" && filepath.IsAbs(args.FilePath) {
		if rel, err := filepath.Rel(args.ProjectRoot, args.FilePath); err == nil && !strings.HasPrefix(rel, "..") {
			paths = append(paths, rel)
		}
	}

	placeholders := make([]string, len(paths))
	queryArgs := make([]any, 0, len(paths)+1)
	for i, p := range paths {
		placeholders[i] = "?"
		queryArgs = append(queryArgs, p)
	}
	queryArgs = append(queryArgs, limit)

	query := fmt.Sprintf(
		`SELECT id, session_id, tool, action_type, timestamp, COALESCE(freshness,''), success, COALESCE(content_hash,'')
		 FROM actions WHERE target IN (%s)
		 ORDER BY timestamp DESC
		 LIMIT ?`,
		strings.Join(placeholders, ","),
	)

	rows, err := t.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	result := getFileHistoryResult{File: args.FilePath}
	for rows.Next() {
		var e fileHistoryEntry
		var ts string
		var success int
		if err := rows.Scan(&e.ActionID, &e.SessionID, &e.Tool, &e.ActionType, &ts, &e.Freshness, &success, &e.ContentHash); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		e.Success = success == 1
		if parsed, perr := time.Parse(time.RFC3339Nano, ts); perr == nil {
			e.Timestamp = parsed
		}
		result.Entries = append(result.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	result.Count = len(result.Entries)

	abs := args.FilePath
	if !filepath.IsAbs(abs) && args.ProjectRoot != "" {
		abs = filepath.Join(args.ProjectRoot, abs)
	}
	if st := enrichStructure(ctx, t.cg, abs); st != nil {
		result.Structure = st
	}
	return result, nil
}

// enrichStructure queries the codegraph client for structural info about
// a file and returns a FileStructure if any data was found. Returns nil
// when the client is unavailable or the file has no graph entries.
func enrichStructure(ctx context.Context, cg *codegraph.Client, absPath string) *FileStructure {
	if cg == nil || !cg.Available() || absPath == "" {
		return nil
	}
	fns, _ := cg.FunctionsInFile(ctx, absPath)
	imps, _ := cg.ImportsInFile(ctx, absPath)
	var callers []string
	for _, fn := range fns {
		cs, _ := cg.CallersOf(ctx, fn)
		callers = append(callers, cs...)
	}
	if len(fns) == 0 && len(imps) == 0 && len(callers) == 0 {
		return nil
	}
	return &FileStructure{
		Functions: fns,
		Callers:   callers,
		Imports:   imps,
	}
}

// -----------------------------------------------------------------------------
// get_session_summary
// -----------------------------------------------------------------------------

type getSessionSummaryTool struct{ db *sql.DB }

func newGetSessionSummaryTool(db *sql.DB) Tool { return &getSessionSummaryTool{db: db} }

func (*getSessionSummaryTool) Name() string { return "get_session_summary" }
func (*getSessionSummaryTool) Description() string {
	return "Summary of recent sessions on a project: ids, tools, timestamps, and per-session action counts. Use to orient yourself when joining an in-flight task."
}
func (*getSessionSummaryTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_root": map[string]any{
				"type":        "string",
				"description": "Absolute path to the project root. Filters sessions to this project when provided.",
			},
			"session_id": map[string]any{
				"type":        "string",
				"description": "Return details for exactly this session (overrides project_root).",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum rows to return when listing (default 10, max 50).",
				"minimum":     1,
				"maximum":     50,
			},
		},
	}
}

type getSessionSummaryArgs struct {
	ProjectRoot string `json:"project_root"`
	SessionID   string `json:"session_id"`
	Limit       int    `json:"limit"`
}

type sessionSummary struct {
	SessionID    string    `json:"session_id"`
	ProjectRoot  string    `json:"project_root"`
	Tool         string    `json:"tool"`
	Model        string    `json:"model,omitempty"`
	GitBranch    string    `json:"git_branch,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at,omitempty"`
	ActionCount  int       `json:"action_count"`
	FailureCount int       `json:"failure_count"`
}

type getSessionSummaryResult struct {
	Sessions []sessionSummary `json:"sessions"`
	Count    int              `json:"count"`
}

func (t *getSessionSummaryTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args getSessionSummaryArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	q := `SELECT s.id, p.root_path, s.tool, COALESCE(s.model,''), COALESCE(s.git_branch,''),
	             s.started_at, COALESCE(s.ended_at,''),
	             (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id) AS action_count,
	             (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id AND a.success = 0) AS failure_count
	      FROM sessions s JOIN projects p ON p.id = s.project_id`
	var queryArgs []any
	var where []string
	if args.SessionID != "" {
		where = append(where, "s.id = ?")
		queryArgs = append(queryArgs, args.SessionID)
	} else if args.ProjectRoot != "" {
		where = append(where, "p.root_path = ?")
		queryArgs = append(queryArgs, args.ProjectRoot)
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY s.started_at DESC LIMIT ?"
	queryArgs = append(queryArgs, limit)

	rows, err := t.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out getSessionSummaryResult
	for rows.Next() {
		var s sessionSummary
		var started, ended string
		if err := rows.Scan(&s.SessionID, &s.ProjectRoot, &s.Tool, &s.Model, &s.GitBranch, &started, &ended, &s.ActionCount, &s.FailureCount); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, started); err == nil {
			s.StartedAt = t
		}
		if ended != "" {
			if t, err := time.Parse(time.RFC3339Nano, ended); err == nil {
				s.EndedAt = t
			}
		}
		out.Sessions = append(out.Sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	out.Count = len(out.Sessions)
	return out, nil
}

// -----------------------------------------------------------------------------
// search_past_outputs
// -----------------------------------------------------------------------------

type searchPastOutputsTool struct {
	db  *sql.DB
	idx *indexing.Indexer
}

func newSearchPastOutputsTool(db *sql.DB) Tool {
	return &searchPastOutputsTool{
		db:  db,
		idx: indexing.New(db, 0),
	}
}

func (*searchPastOutputsTool) Name() string { return "search_past_outputs" }
func (*searchPastOutputsTool) Description() string {
	return "FTS5 search across stored tool-output excerpts from prior sessions. Use to find past test failures, error messages, or command outputs instead of re-running the command."
}
func (*searchPastOutputsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "FTS5 MATCH expression. Simple queries like 'FAIL' work; quote special characters.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum matches to return (default 10, max 50).",
				"minimum":     1,
				"maximum":     50,
			},
		},
		"required": []string{"query"},
	}
}

type searchPastOutputsArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

type searchHit struct {
	ActionID     int64   `json:"action_id"`
	ToolName     string  `json:"tool_name,omitempty"`
	Target       string  `json:"target,omitempty"`
	Excerpt      string  `json:"excerpt,omitempty"`
	ErrorMessage string  `json:"error_message,omitempty"`
	Rank         float64 `json:"rank"`
}

type searchPastOutputsResult struct {
	Query string      `json:"query"`
	Hits  []searchHit `json:"hits"`
	Count int         `json:"count"`
}

func (t *searchPastOutputsTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args searchPastOutputsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return nil, errors.New("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	results, err := t.idx.Search(ctx, args.Query, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	hits := make([]searchHit, 0, len(results))
	for _, r := range results {
		hits = append(hits, searchHit{
			ActionID:     r.ActionID,
			ToolName:     r.ToolName,
			Target:       r.Target,
			Excerpt:      r.Excerpt,
			ErrorMessage: r.ErrorMessage,
			Rank:         r.Rank,
		})
	}
	return searchPastOutputsResult{Query: args.Query, Hits: hits, Count: len(hits)}, nil
}
