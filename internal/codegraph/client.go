package codegraph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite" // sqlite driver registration
)

// Client is a read-only handle to a codebase-memory-mcp graph database.
// The zero value is unusable — call Open or NewMissing.
type Client struct {
	db   *sql.DB
	path string
}

// Open opens the codebase-memory-mcp SQLite at path in read-only mode.
// A missing file is not an error — the returned Client reports
// Available()==false and every query returns an empty result.
func Open(path string) (*Client, error) {
	if path == "" {
		return NewMissing(), nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &Client{path: path}, nil
	} else if err != nil {
		return nil, fmt.Errorf("codegraph.Open: stat %s: %w", path, err)
	}
	// mode=ro plus the immutable=1 hint tells SQLite to skip locking — safe
	// because the companion writes via its own process and we never mutate.
	dsn := fmt.Sprintf("file:%s?mode=ro&immutable=1", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("codegraph.Open: open: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("codegraph.Open: ping: %w", err)
	}
	return &Client{db: db, path: path}, nil
}

// NewMissing returns a Client whose Available() is false. Useful for
// dependency injection in tests and in MCP tool wiring before the
// companion has been installed.
func NewMissing() *Client { return &Client{} }

// Close releases the database handle. Safe to call on an unavailable client.
func (c *Client) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Available reports whether the underlying graph DB is open and reachable.
// MCP tools use this as the gate before calling any of the query methods.
func (c *Client) Available() bool {
	return c != nil && c.db != nil
}

// Path returns the configured DB path, even when Available() is false.
// Useful for diagnostics ("we looked here and didn't find it").
func (c *Client) Path() string {
	if c == nil {
		return ""
	}
	return c.path
}

// FunctionsInFile returns the function names defined in absPath. Returns
// an empty slice when the client is unavailable or the schema doesn't
// match — never an error in those cases.
func (c *Client) FunctionsInFile(ctx context.Context, absPath string) ([]string, error) {
	if !c.Available() || absPath == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT name FROM nodes WHERE file_path = ? AND kind = 'function'
		 ORDER BY start_line LIMIT 200`, absPath)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("codegraph.FunctionsInFile: scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ImportsInFile returns module/import paths referenced from absPath via
// IMPORTS edges in the graph. Same availability + schema-tolerance rules
// as FunctionsInFile.
func (c *Client) ImportsInFile(ctx context.Context, absPath string) ([]string, error) {
	if !c.Available() || absPath == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT DISTINCT tgt.name
		 FROM edges e
		 JOIN nodes src ON src.id = e.source_id
		 JOIN nodes tgt ON tgt.id = e.target_id
		 WHERE e.kind = 'IMPORTS' AND src.file_path = ?
		 ORDER BY tgt.name LIMIT 200`, absPath)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("codegraph.ImportsInFile: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CallersOf returns symbol names that call functionName via CALLS edges.
// Same availability + schema-tolerance rules.
func (c *Client) CallersOf(ctx context.Context, functionName string) ([]string, error) {
	if !c.Available() || functionName == "" {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT DISTINCT src.name
		 FROM edges e
		 JOIN nodes src ON src.id = e.source_id
		 JOIN nodes tgt ON tgt.id = e.target_id
		 WHERE e.kind = 'CALLS' AND tgt.name = ?
		 ORDER BY src.name LIMIT 200`, functionName)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("codegraph.CallersOf: scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
