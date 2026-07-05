package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ListRow is one mcp_audit row in the read-side shape. Maps the
// schema (migration 030) into Go-friendly types — duration in
// time.Duration, response_ok in bool, etc. — so CLI / dashboard
// consumers don't repeat the conversion.
type ListRow struct {
	Ts                time.Time
	SessionID         string
	Tool              string
	RequestHash       string
	PathRequested     string
	ResponseBytes     int
	ResponseTruncated bool
	ResponseOK        bool
	Reason            string
	Duration          time.Duration
}

// ListOptions filters [List]. Empty zero values mean "no filter."
type ListOptions struct {
	// SessionID restricts to one session when non-empty.
	SessionID string
	// Since trims to rows newer than now-Since. Zero = no time floor.
	Since time.Duration
	// Tool restricts to one tool when non-empty (e.g. "get_file").
	Tool string
	// OnlyDenied = true returns only response_ok = 0 rows.
	OnlyDenied bool
	// Limit caps the result set. Zero falls back to defaultListLimit.
	// Operators wanting unbounded results use a deliberately large N.
	Limit int
}

const defaultListLimit = 50

// List returns rows from mcp_audit matching opts, sorted newest-first.
// The default limit (50) keeps CLI output tractable; bump explicitly
// for pagination needs.
//
// Read-only — does NOT mutate the DB.
func List(ctx context.Context, db *sql.DB, opts ListOptions) ([]ListRow, error) {
	if db == nil {
		return nil, errors.New("audit.List: db is required")
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}

	q := strings.Builder{}
	q.WriteString(`SELECT ts, COALESCE(session_id, ''), tool_name, request_hash,
	                  COALESCE(path_requested, ''),
	                  response_size_bytes, response_truncated, response_ok,
	                  COALESCE(reason, ''), duration_us
	             FROM mcp_audit WHERE 1=1`)
	var args []any
	if opts.Since > 0 {
		q.WriteString(` AND ts >= ?`)
		args = append(args, time.Now().Add(-opts.Since).UTC().Format(time.RFC3339Nano))
	}
	if opts.SessionID != "" {
		q.WriteString(` AND session_id = ?`)
		args = append(args, opts.SessionID)
	}
	if opts.Tool != "" {
		q.WriteString(` AND tool_name = ?`)
		args = append(args, opts.Tool)
	}
	if opts.OnlyDenied {
		q.WriteString(` AND response_ok = 0`)
	}
	q.WriteString(` ORDER BY ts DESC LIMIT ?`)
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("audit.List: query: %w", err)
	}
	defer rows.Close()

	var out []ListRow
	for rows.Next() {
		var r ListRow
		var tsStr string
		var truncatedInt, okInt int
		var durationUS int64
		if err := rows.Scan(
			&tsStr, &r.SessionID, &r.Tool, &r.RequestHash,
			&r.PathRequested, &r.ResponseBytes, &truncatedInt, &okInt,
			&r.Reason, &durationUS,
		); err != nil {
			return nil, fmt.Errorf("audit.List: scan: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
			r.Ts = t
		}
		r.ResponseTruncated = truncatedInt != 0
		r.ResponseOK = okInt != 0
		r.Duration = time.Duration(durationUS) * time.Microsecond
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit.List: rows: %w", err)
	}
	return out, nil
}

// Stats summarises mcp_audit activity over a time window. Reported
// counts are exact (a single COUNT query) — no sampling.
type Stats struct {
	// Since is the time-window the caller asked for, echoed for
	// display. Zero means "all time."
	Since time.Duration
	// Total is the number of rows in the window.
	Total int
	// OK is the count of response_ok = 1 rows.
	OK int
	// Denied is the count of response_ok = 0 rows.
	Denied int
	// Truncated is the count of rows with response_truncated = 1
	// (regardless of OK).
	Truncated int
	// ByTool maps tool_name to call count in the window.
	ByTool map[string]int
}

// StatsSummary computes a window summary. Since=0 means "all time."
func StatsSummary(ctx context.Context, db *sql.DB, since time.Duration) (Stats, error) {
	if db == nil {
		return Stats{}, errors.New("audit.StatsSummary: db is required")
	}
	stats := Stats{Since: since, ByTool: map[string]int{}}

	timeFilter, args := buildTimeFilter(since)

	// Aggregate counts in one row. timeFilter is a closed-set literal
	// from buildTimeFilter ("" or "WHERE ts >= ?"); args are
	// parameter-bound. nolint scope kept tight.
	//nolint:gosec // G202: timeFilter is a closed-set literal, args parameter-bound
	row := db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        SUM(CASE WHEN response_ok = 1 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN response_ok = 0 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN response_truncated = 1 THEN 1 ELSE 0 END)
		   FROM mcp_audit `+timeFilter, args...)
	var ok, denied, truncated sql.NullInt64
	if err := row.Scan(&stats.Total, &ok, &denied, &truncated); err != nil {
		return Stats{}, fmt.Errorf("audit.StatsSummary: aggregate: %w", err)
	}
	stats.OK = int(ok.Int64)
	stats.Denied = int(denied.Int64)
	stats.Truncated = int(truncated.Int64)

	// Per-tool breakdown. Same closed-set timeFilter rationale.
	//nolint:gosec // G202: timeFilter is a closed-set literal, args parameter-bound
	rows, err := db.QueryContext(ctx,
		`SELECT tool_name, COUNT(*) FROM mcp_audit `+timeFilter+
			` GROUP BY tool_name ORDER BY 2 DESC`, args...)
	if err != nil {
		return Stats{}, fmt.Errorf("audit.StatsSummary: by-tool: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return Stats{}, fmt.Errorf("audit.StatsSummary: scan: %w", err)
		}
		stats.ByTool[name] = n
	}
	return stats, rows.Err()
}

// TopPath is one entry of the top-requested-paths roll-up.
type TopPath struct {
	Path       string
	Calls      int
	TotalBytes int64
	// OKCalls + DeniedCalls help operators see the success/denial
	// ratio per path without a second roll-up query.
	OKCalls     int
	DeniedCalls int
}

// TopPaths returns the most-frequently-requested path_requested
// values in the window. Useful for capacity planning ("which file is
// the agent re-reading 200×?") and for detecting agent re-read
// loops.
//
// Excludes rows where path_requested IS NULL (tools that don't take
// a path) — they'd cluster as one giant empty group and hide the
// signal.
func TopPaths(ctx context.Context, db *sql.DB, since time.Duration, limit int) ([]TopPath, error) {
	if db == nil {
		return nil, errors.New("audit.TopPaths: db is required")
	}
	if limit <= 0 {
		limit = 20
	}
	timeFilter, args := buildTimeFilter(since)
	args = append(args, limit)
	// q is composed from closed-set literals (timeFilter is from
	// buildTimeFilter; the path-NULL filter is a hardcoded suffix);
	// args are parameter-bound.
	//nolint:gosec // G202: closed-set literals + parameter binding
	q := `SELECT COALESCE(path_requested, ''),
	             COUNT(*) AS calls,
	             COALESCE(SUM(response_size_bytes), 0),
	             SUM(CASE WHEN response_ok = 1 THEN 1 ELSE 0 END),
	             SUM(CASE WHEN response_ok = 0 THEN 1 ELSE 0 END)
	        FROM mcp_audit ` + timeFilter
	if timeFilter == "" {
		q += ` WHERE path_requested IS NOT NULL AND path_requested != ''`
	} else {
		q += ` AND path_requested IS NOT NULL AND path_requested != ''`
	}
	q += ` GROUP BY path_requested ORDER BY calls DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("audit.TopPaths: query: %w", err)
	}
	defer rows.Close()
	var out []TopPath
	for rows.Next() {
		var t TopPath
		if err := rows.Scan(&t.Path, &t.Calls, &t.TotalBytes, &t.OKCalls, &t.DeniedCalls); err != nil {
			return nil, fmt.Errorf("audit.TopPaths: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Purge deletes mcp_audit rows older than olderThan and returns the
// deleted row count. Caller controls whether to prompt-confirm.
//
// olderThan must be > 0 — passing zero would delete the entire table
// and is a footgun. Callers wanting that behavior should pass a
// huge duration explicitly, or use TRUNCATE-style SQL directly.
func Purge(ctx context.Context, db *sql.DB, olderThan time.Duration) (int, error) {
	if db == nil {
		return 0, errors.New("audit.Purge: db is required")
	}
	if olderThan <= 0 {
		return 0, errors.New("audit.Purge: olderThan must be > 0 (refusing to delete everything)")
	}
	cutoff := time.Now().Add(-olderThan).UTC().Format(time.RFC3339Nano)
	res, err := db.ExecContext(ctx, `DELETE FROM mcp_audit WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("audit.Purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// buildTimeFilter returns a `WHERE ts >= ?` clause when since > 0,
// plus the matching args slice. Callers append their own args after.
func buildTimeFilter(since time.Duration) (string, []any) {
	if since <= 0 {
		return "", nil
	}
	cutoff := time.Now().Add(-since).UTC().Format(time.RFC3339Nano)
	return `WHERE ts >= ?`, []any{cutoff}
}
