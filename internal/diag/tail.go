package diag

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// TailEntry is one streamed action.
type TailEntry struct {
	ID         int64
	Timestamp  time.Time
	Tool       string
	ActionType string
	Target     string
	Success    bool
	SessionID  string
}

// TailOptions parameterizes Tail.
type TailOptions struct {
	// Interval between polls. Defaults to 1 second.
	Interval time.Duration
	// PageSize caps each poll's read. Defaults to 100.
	PageSize int
	// Since, when non-zero, returns rows newer than this timestamp.
	// Defaults to time.Now() at start.
	Since time.Time
}

// Tail streams new actions to w until ctx is cancelled. It polls the
// actions table every Interval and emits any rows with id > the last
// observed id. The implementation is poll-based (not fsnotify or DB
// triggers) to keep the contract simple and to work across processes that
// share the WAL.
func Tail(ctx context.Context, database *sql.DB, w io.Writer, opts TailOptions) error {
	if database == nil {
		return errors.New("diag.Tail: nil DB")
	}
	if opts.Interval <= 0 {
		opts.Interval = 1 * time.Second
	}
	if opts.PageSize <= 0 {
		opts.PageSize = 100
	}
	since := opts.Since
	if since.IsZero() {
		since = time.Now().UTC()
	}

	// We track lastID so a row inserted within the same wall-second still
	// advances. Bootstrap by reading the latest id at-or-before `since`.
	var lastID int64
	_ = database.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM actions WHERE timestamp <= ?`,
		since.Format(time.RFC3339Nano),
	).Scan(&lastID)

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	for {
		entries, newLast, err := fetchSince(ctx, database, lastID, opts.PageSize)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fmt.Fprintln(w, formatEntry(e))
		}
		if newLast > lastID {
			lastID = newLast
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func fetchSince(ctx context.Context, database *sql.DB, afterID int64, limit int) ([]TailEntry, int64, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT id, timestamp, tool, action_type, COALESCE(target,''), success, session_id
		 FROM actions
		 WHERE id > ?
		 ORDER BY id
		 LIMIT ?`,
		afterID, limit,
	)
	if err != nil {
		return nil, afterID, fmt.Errorf("diag.Tail: %w", err)
	}
	defer rows.Close()

	var entries []TailEntry
	last := afterID
	for rows.Next() {
		var e TailEntry
		var ts string
		var success int
		if err := rows.Scan(&e.ID, &ts, &e.Tool, &e.ActionType, &e.Target, &success, &e.SessionID); err != nil {
			return nil, afterID, err
		}
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			e.Timestamp = t
		}
		e.Success = success == 1
		entries = append(entries, e)
		last = e.ID
	}
	return entries, last, rows.Err()
}

func formatEntry(e TailEntry) string {
	mark := "✓"
	if !e.Success {
		mark = "✗"
	}
	target := e.Target
	if len(target) > 80 {
		target = target[:77] + "..."
	}
	return fmt.Sprintf("%s  %s  %-12s  %-14s  %s  [%s]",
		mark,
		e.Timestamp.Format("15:04:05"),
		e.Tool,
		e.ActionType,
		strings.ReplaceAll(target, "\n", " "),
		e.SessionID,
	)
}
