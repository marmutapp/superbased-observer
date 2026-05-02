package indexing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DefaultMaxExcerptBytes is the default cap for stored excerpts.
const DefaultMaxExcerptBytes = 2048

// Indexer records tool output excerpts in the FTS5 action_excerpts virtual
// table so the MCP server's search_past_outputs tool can retrieve them.
//
// The zero value is unusable — call New.
type Indexer struct {
	db              *sql.DB
	maxExcerptBytes int
}

// New returns an Indexer bound to db. maxExcerptBytes ≤ 0 falls back to
// DefaultMaxExcerptBytes.
func New(db *sql.DB, maxExcerptBytes int) *Indexer {
	if maxExcerptBytes <= 0 {
		maxExcerptBytes = DefaultMaxExcerptBytes
	}
	return &Indexer{db: db, maxExcerptBytes: maxExcerptBytes}
}

// Index records an excerpt for an action. The stored excerpt is the first
// half of maxExcerptBytes plus the last half when raw exceeds the cap, joined
// by a "…" marker — mirroring the spec's "first 1KB + last 1KB" rule.
//
// Idempotent: re-indexing the same action_id overwrites the prior excerpt.
func (i *Indexer) Index(ctx context.Context, actionID int64, toolName, target, raw, errorMessage string) error {
	if actionID == 0 {
		return errors.New("indexing.Index: actionID required")
	}
	excerpt := TrimExcerpt(raw, i.maxExcerptBytes)
	// FTS5 with content='actions' is a contentless view; we insert into
	// its rowid-keyed virtual table directly. Replace any prior row with
	// the same action_id so re-parsing doesn't multiply entries.
	_, err := i.db.ExecContext(ctx,
		`DELETE FROM action_excerpts WHERE action_id = ?`, actionID)
	if err != nil {
		return fmt.Errorf("indexing.Index: delete prior: %w", err)
	}
	_, err = i.db.ExecContext(ctx,
		`INSERT INTO action_excerpts (action_id, tool_name, target, excerpt, error_message)
		 VALUES (?, ?, ?, ?, ?)`,
		actionID, toolName, target, excerpt, errorMessage,
	)
	if err != nil {
		return fmt.Errorf("indexing.Index: insert: %w", err)
	}
	return nil
}

// SearchResult is a single FTS5 match returned by Search.
type SearchResult struct {
	ActionID     int64
	ToolName     string
	Target       string
	Excerpt      string
	ErrorMessage string
	Rank         float64
}

// Search runs an FTS5 MATCH query against action_excerpts. query uses FTS5
// syntax — quote terms with special characters. Results are ordered by rank
// (ascending; more negative = more relevant in SQLite FTS5).
func (i *Indexer) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := i.db.QueryContext(ctx,
		`SELECT action_id, tool_name, target, excerpt, error_message, rank
		 FROM action_excerpts
		 WHERE action_excerpts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("indexing.Search: query: %w", err)
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ActionID, &r.ToolName, &r.Target, &r.Excerpt, &r.ErrorMessage, &r.Rank); err != nil {
			return nil, fmt.Errorf("indexing.Search: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TrimExcerpt reduces raw to at most maxBytes by keeping the first and last
// halves separated by a "…[truncated]…" marker when the input exceeds the
// cap. Short inputs are returned unchanged.
func TrimExcerpt(raw string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxExcerptBytes
	}
	if len(raw) <= maxBytes {
		return raw
	}
	const marker = "…[truncated]…"
	budget := maxBytes - len(marker)
	if budget <= 0 {
		return raw[:maxBytes]
	}
	head := budget / 2
	tail := budget - head
	return raw[:head] + marker + raw[len(raw)-tail:]
}
