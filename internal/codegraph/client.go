package codegraph

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // sqlite driver registration
)

// Symbol is a code symbol extracted from the codebase-memory-mcp
// graph DB. Returned by [Client.SymbolsInFile]. Pure data — the
// conversation package mirrors this shape as
// conversation.CompressorSymbol so it can stay free of any codegraph
// dependency.
type Symbol struct {
	// Name is the symbol's identifier as it appears in the source
	// (function name, class name, etc.).
	Name string
	// Kind is the symbol's category — one of `function`, `method`,
	// `class`, `interface`, `type`. Other kinds are filtered out at
	// the SQL layer.
	Kind string
	// StartLine + EndLine carry the symbol's location in the source.
	StartLine int
	EndLine   int
}

// staleSlackSeconds is the wall-clock tolerance the [Client.Stale]
// check applies before declaring the file newer than the index.
// File-system mtime resolution + codebase-memory-mcp's indexing
// latency can produce a few seconds of jitter that should NOT
// invalidate the index. Keep small so a real edit is caught quickly.
const staleSlackSeconds = 5

// Client is a read-only handle to a codebase-memory-mcp graph database.
// The zero value is unusable — call Open or NewMissing.
type Client struct {
	db   *sql.DB
	path string
	// startupMTime is the DB file's mtime captured at [Open]. Zero
	// when the path is missing or the stat failed. Used by V7-13
	// Gap 5 (b) to detect when codebase-memory-mcp has re-indexed
	// the DB between observer startup and a subsequent query.
	startupMTime time.Time
	// warnedReindex gates the re-index warning to fire AT MOST ONCE
	// per Client lifetime. Atomic CAS so concurrent queries don't
	// log a flood. False (zero value) on a fresh Client; flips true
	// the first time [checkReindex] notices the DB mtime differs
	// from startupMTime.
	warnedReindex atomic.Bool
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
	// V7-13 Gap 5 (v1.7.7): mode=ro WITHOUT immutable=1. The immutable
	// hint tells SQLite to skip locking — safe only when the file is
	// guaranteed unchanged. codebase-memory-mcp's indexer can re-write
	// the graph DB concurrently with our read-only queries, so the
	// hint was unsafe (torn-page reads possible). Plain mode=ro uses
	// SQLite's normal page-level locking and is concurrent-write
	// safe. Throughput cost: a few percent on read latency — well
	// worth eliminating the torn-read failure mode. See
	// docs/v4-codex-compression-recipe-and-issues.md V7-13 Gap 5.
	dsn := fmt.Sprintf("file:%s?mode=ro", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("codegraph.Open: open: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("codegraph.Open: ping: %w", err)
	}
	// V7-13 Gap 5 (b): capture startup mtime for the re-index
	// detection. Failure to stat here just leaves the field zero;
	// checkReindex treats zero as "no baseline" and is a no-op.
	c := &Client{db: db, path: path}
	if info, err := os.Stat(path); err == nil {
		c.startupMTime = info.ModTime()
	}
	return c, nil
}

// checkReindex emits ONE warning per Client lifetime when the
// codebase-memory-mcp DB file's mtime has changed since [Open]. Public
// query methods call this at the top so the warning travels with the
// query that spans the re-index boundary, not at some unrelated
// later point. (V7-13 Gap 5 b.)
//
// No-op when: client has no path (NewMissing), startupMTime is zero
// (Open couldn't stat), stat fails now, mtime unchanged, or warning
// already fired. Uses [slog.Default()] — production wires the observer
// logger into slog.Default at startup.
func (c *Client) checkReindex() {
	if c == nil || c.path == "" || c.startupMTime.IsZero() || c.warnedReindex.Load() {
		return
	}
	info, err := os.Stat(c.path)
	if err != nil || info.ModTime().Equal(c.startupMTime) {
		return
	}
	if c.warnedReindex.CompareAndSwap(false, true) {
		slog.Default().Warn(
			"codegraph: index modified since startup; some queries may span the re-index boundary",
			"startup_mtime", c.startupMTime,
			"current_mtime", info.ModTime(),
			"path", c.path,
		)
	}
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
	c.checkReindex()
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
	c.checkReindex()
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

// SymbolsInFile returns the user-facing symbols defined in absPath:
// functions, methods, classes, interfaces, and types. Sorted by
// start_line ASC for deterministic output (required for the v1.7.7
// LogsCompressor marker enrichment — repeated compressions of
// identical bodies must produce byte-identical markers for the
// proxy's prefix cache to hit). Capped at LIMIT 50 — markers cap
// rendering at top-10, so 50 is plenty of headroom even with
// kind-mix variation.
//
// Same schema-tolerance rules as [FunctionsInFile]: an unavailable
// client, missing schema, or query error returns an empty slice
// without an error. Callers fail open. (V7-11 mitigation (e).)
func (c *Client) SymbolsInFile(ctx context.Context, absPath string) ([]Symbol, error) {
	if !c.Available() || absPath == "" {
		return nil, nil
	}
	c.checkReindex()
	rows, err := c.db.QueryContext(ctx,
		`SELECT COALESCE(name, ''), COALESCE(kind, ''),
		        COALESCE(start_line, 0), COALESCE(end_line, 0)
		 FROM nodes
		 WHERE file_path = ?
		   AND kind IN ('function', 'method', 'class', 'interface', 'type')
		 ORDER BY start_line ASC LIMIT 50`, absPath)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		var s Symbol
		if err := rows.Scan(&s.Name, &s.Kind, &s.StartLine, &s.EndLine); err != nil {
			return nil, fmt.Errorf("codegraph.SymbolsInFile: scan: %w", err)
		}
		if s.Name == "" {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Stale reports whether absPath's mtime is meaningfully newer than
// the codegraph DB's mtime. Returns false (fail-open) when the
// client is unavailable, the file can't be stat'd, the DB path
// can't be stat'd, or any other error path — callers that need the
// codegraph data trust the result of an empty-list query rather
// than gating on Stale().
//
// `Meaningfully newer` = file mtime > DB mtime + [staleSlackSeconds].
// The slack absorbs file-system mtime resolution jitter + the
// indexer's natural latency between file change and DB write.
//
// Caller pattern (V7-11 marker enrichment): SKIP the symbol pre-
// fetch when Stale==true. Stale codegraph data fed into a self-
// documenting marker would mislead the agent (claiming `handleClick`
// exists at line 50 when the file just moved it to line 80).
// Filename-only marker is the right fallback. (V7-13 Gap 3.)
func (c *Client) Stale(absPath string) bool {
	if !c.Available() || absPath == "" || c.path == "" {
		return false
	}
	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return false
	}
	dbInfo, err := os.Stat(c.path)
	if err != nil {
		return false
	}
	slack := time.Duration(staleSlackSeconds) * time.Second
	return fileInfo.ModTime().After(dbInfo.ModTime().Add(slack))
}

// SymbolMatch is the richer per-symbol shape returned by [FindSymbols].
// Carries enough metadata for the V7-12 get_symbols MCP tool to render
// matches without a second query round-trip: V7-15 ranking sorts on
// these fields, and the MCP layer mirrors them into its response.
//
// Pure data — no codegraph internals leak to the MCP layer. The MCP
// package does not import codegraph types directly; it converts to its
// own response shape at the tool boundary.
type SymbolMatch struct {
	ID        int64
	Name      string
	FQN       string
	Kind      string
	File      string
	Language  string
	StartLine int
	EndLine   int
}

// Caller is one row in a symbol's caller-or-callee list. Returned by
// [CallersOfSymbol] and [CalleesOfSymbol] for the V7-12 get_symbols
// `include_relations: true` payload.
type Caller struct {
	ID        int64
	Name      string
	FQN       string
	Kind      string
	File      string
	StartLine int
}

// userFacingKindsSQL is the SQL fragment that filters out noise kinds
// (variable, parameter, module, etc.) from symbol queries. Matches the
// set used by [SymbolsInFile] so the discovery-mode get_symbols
// response is consistent with what marker enrichment returned.
const userFacingKindsSQL = "kind IN ('function', 'method', 'class', 'interface', 'type')"

// FindSymbols returns matches in absFile filtered by the optional
// (name, fqn, kind) selectors. Empty values for any selector skip
// that filter:
//
//   - All three empty                  → discovery mode: every
//     user-facing symbol in the file (functions, methods, classes,
//     interfaces, types).
//   - name set, fqn empty              → name-only lookup; may
//     match multiple symbols (overloads, scoped vs. module level).
//   - fqn set                          → exact-fqn lookup; at most
//     one match in well-formed code.
//   - kind set                         → ANDed with the others;
//     restricts the kind column.
//
// LIMIT 50 caps query work (matches [SymbolsInFile]'s rationale —
// markers cap rendering at top-10 and 50 is headroom for kind-mix
// variation; V7-15 ranking applies in the caller before any
// per-batch body-size cap kicks in).
//
// Same schema-tolerance rules as the other query methods: an
// unavailable client, missing schema, or SQL error returns nil + nil.
// Callers fail open. (V7-12 / V7-15.)
func (c *Client) FindSymbols(ctx context.Context, absFile, name, fqn, kind string) ([]SymbolMatch, error) {
	if !c.Available() || absFile == "" {
		return nil, nil
	}
	c.checkReindex()
	q := `SELECT COALESCE(id, 0), COALESCE(name, ''), COALESCE(fqn, ''),
	             COALESCE(kind, ''), COALESCE(file_path, ''),
	             COALESCE(language, ''),
	             COALESCE(start_line, 0), COALESCE(end_line, 0)
	      FROM nodes
	      WHERE file_path = ?`
	args := []any{absFile}
	switch {
	case fqn != "":
		q += ` AND fqn = ?`
		args = append(args, fqn)
	case name != "":
		q += ` AND name = ?`
		args = append(args, name)
	default:
		q += ` AND ` + userFacingKindsSQL
	}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY start_line ASC LIMIT 50`

	rows, err := c.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []SymbolMatch
	for rows.Next() {
		var s SymbolMatch
		if err := rows.Scan(&s.ID, &s.Name, &s.FQN, &s.Kind, &s.File,
			&s.Language, &s.StartLine, &s.EndLine); err != nil {
			return nil, fmt.Errorf("codegraph.FindSymbols: scan: %w", err)
		}
		if s.Name == "" {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CallersOfSymbol returns symbols that call symbolID via CALLS edges,
// with enough metadata for the agent to follow up via get_symbols
// without a second discovery call. Sorted by start_line ASC, name ASC,
// id ASC (deterministic — required for OpenAI prefix-cache stability).
// LIMIT capped by the caller-supplied limit (V7-12 / config-driven
// max_callers; 20 by default).
//
// Pre-v1.7.9 the codegraph only exposed [CallersOf] returning
// []string — names only. This is the richer variant for the V7-12
// include_relations payload. Old method is kept for the existing
// marker-enrichment caller which only needs names.
func (c *Client) CallersOfSymbol(ctx context.Context, symbolID int64, limit int) ([]Caller, error) {
	if !c.Available() || symbolID == 0 {
		return nil, nil
	}
	c.checkReindex()
	if limit <= 0 {
		limit = 20
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT COALESCE(src.id, 0), COALESCE(src.name, ''),
		        COALESCE(src.fqn, ''), COALESCE(src.kind, ''),
		        COALESCE(src.file_path, ''), COALESCE(src.start_line, 0)
		 FROM edges e
		 JOIN nodes src ON src.id = e.source_id
		 WHERE e.kind = 'CALLS' AND e.target_id = ?
		 ORDER BY src.start_line ASC, src.name ASC, src.id ASC
		 LIMIT ?`, symbolID, limit)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []Caller
	for rows.Next() {
		var c Caller
		if err := rows.Scan(&c.ID, &c.Name, &c.FQN, &c.Kind, &c.File, &c.StartLine); err != nil {
			return nil, fmt.Errorf("codegraph.CallersOfSymbol: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CalleesOfSymbol is the inverse of [CallersOfSymbol]: symbols that
// symbolID calls. Same sort + LIMIT semantics.
func (c *Client) CalleesOfSymbol(ctx context.Context, symbolID int64, limit int) ([]Caller, error) {
	if !c.Available() || symbolID == 0 {
		return nil, nil
	}
	c.checkReindex()
	if limit <= 0 {
		limit = 20
	}
	rows, err := c.db.QueryContext(ctx,
		`SELECT COALESCE(tgt.id, 0), COALESCE(tgt.name, ''),
		        COALESCE(tgt.fqn, ''), COALESCE(tgt.kind, ''),
		        COALESCE(tgt.file_path, ''), COALESCE(tgt.start_line, 0)
		 FROM edges e
		 JOIN nodes tgt ON tgt.id = e.target_id
		 WHERE e.kind = 'CALLS' AND e.source_id = ?
		 ORDER BY tgt.start_line ASC, tgt.name ASC, tgt.id ASC
		 LIMIT ?`, symbolID, limit)
	if err != nil {
		return nil, nil
	}
	defer rows.Close()
	var out []Caller
	for rows.Next() {
		var c Caller
		if err := rows.Scan(&c.ID, &c.Name, &c.FQN, &c.Kind, &c.File, &c.StartLine); err != nil {
			return nil, fmt.Errorf("codegraph.CalleesOfSymbol: scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountCallers returns the unlimited count of symbols calling
// symbolID. Used alongside [CallersOfSymbol] so the agent sees
// `callers_count: 47, callers: [...top 20...]` rather than being
// misled by the LIMIT.
func (c *Client) CountCallers(ctx context.Context, symbolID int64) (int, error) {
	return c.countEdges(ctx, symbolID, "CALLS", "target_id")
}

// CountCallees returns the unlimited count of symbols called BY
// symbolID. Pair with [CalleesOfSymbol].
func (c *Client) CountCallees(ctx context.Context, symbolID int64) (int, error) {
	return c.countEdges(ctx, symbolID, "CALLS", "source_id")
}

func (c *Client) countEdges(ctx context.Context, symbolID int64, kind, column string) (int, error) {
	if !c.Available() || symbolID == 0 {
		return 0, nil
	}
	c.checkReindex()
	// SQL is composed from fixed identifiers (column ∈ {target_id,
	// source_id}, kind ∈ {CALLS}); symbolID is parameter-bound.
	q := `SELECT COUNT(*) FROM edges WHERE kind = ? AND ` + column + ` = ?`
	var n int
	if err := c.db.QueryRowContext(ctx, q, kind, symbolID).Scan(&n); err != nil {
		return 0, nil
	}
	return n, nil
}

// RelationDirection picks which side of the edge the anchor sits
// on, plus which edge kind to traverse. Used by [Reachable] to drive
// the V7-12 get_relations MCP tool.
type RelationDirection int

const (
	// RelationCallers traverses CALLS edges from the anchor as
	// target_id back to source_id. Answers "who calls X?".
	RelationCallers RelationDirection = iota
	// RelationCallees traverses CALLS edges from the anchor as
	// source_id forward to target_id. Answers "what does X call?".
	RelationCallees
	// RelationContains traverses CONTAINS edges from the anchor as
	// source_id forward to target_id (parent → child). Answers
	// "what does X contain?". Note: CONTAINS edges may not be
	// populated by every codebase-memory-mcp release; an empty
	// result is normal and surfaced via the MCP tool's
	// `degraded: true` flag.
	RelationContains
)

// Reachable is one row returned by [Client.Reachable]. Carries the
// reached symbol's metadata plus its BFS depth from the anchor and
// the edge kind that brought it in.
type Reachable struct {
	ID        int64
	Name      string
	FQN       string
	Kind      string
	File      string
	Language  string
	StartLine int
	Depth     int    // hops from anchor (>= 1)
	ViaEdge   string // edge kind that brought it in (e.g. "CALLS")
}

// Reachable returns symbols reachable from anchorID via the chosen
// relation, up to maxDepth hops, capped at maxResults. Implemented
// as a single recursive-CTE query — cycle-safe (UNION dedups by
// (id, depth, via_edge); depth bound enforced in the recursive step)
// and shortest-path-wins (outer GROUP BY n.id MIN(depth)).
//
// Returns (results, truncated, err). results is sorted by
// (depth ASC, start_line ASC, name ASC, id ASC) for byte-stable
// output across re-calls — required for OpenAI prefix-cache
// preservation. truncated is true when the LIMIT was hit (the true
// reachable set may be larger).
//
// Same fail-open contract as the rest of the package: unavailable
// client, missing schema, SQL errors all return (nil, false, nil).
// Callers translate the empty result into a degraded MCP response.
//
// maxDepth and maxResults must both be > 0; non-positive values
// are clamped to (1, 1) defensively. Callers should clamp to their
// configured ceilings before invoking.
func (c *Client) Reachable(ctx context.Context, anchorID int64, direction RelationDirection, maxDepth, maxResults int) ([]Reachable, bool, error) {
	if !c.Available() || anchorID == 0 {
		return nil, false, nil
	}
	c.checkReindex()
	if maxDepth <= 0 {
		maxDepth = 1
	}
	if maxResults <= 0 {
		maxResults = 1
	}
	edgeKind, seedCol, recurseCol := edgeTraversal(direction)
	if edgeKind == "" {
		return nil, false, nil
	}
	// SQL is composed from fixed identifiers (edgeKind ∈ {CALLS,
	// CONTAINS}, seedCol/recurseCol ∈ {source_id, target_id});
	// anchorID, maxDepth, maxResults are all parameter-bound.
	//nolint:gosec // G202: identifiers from closed enum (edgeTraversal), all user values param-bound
	q := `WITH RECURSIVE reach(id, depth, via_edge) AS (
		SELECT e.` + recurseCol + `, 1, ?
		FROM edges e
		WHERE e.` + seedCol + ` = ?
		  AND e.kind = ?
		UNION
		SELECT e.` + recurseCol + `, r.depth + 1, ?
		FROM reach r
		JOIN edges e ON e.` + seedCol + ` = r.id
		WHERE e.kind = ?
		  AND r.depth < ?
	)
	SELECT n.id, COALESCE(n.name, ''), COALESCE(n.fqn, ''),
	       COALESCE(n.kind, ''), COALESCE(n.file_path, ''),
	       COALESCE(n.language, ''), COALESCE(n.start_line, 0),
	       MIN(r.depth) AS first_depth, MAX(r.via_edge)
	FROM reach r
	JOIN nodes n ON n.id = r.id
	GROUP BY n.id
	ORDER BY first_depth ASC, n.start_line ASC, n.name ASC, n.id ASC
	LIMIT ?`
	rows, err := c.db.QueryContext(ctx, q,
		edgeKind, anchorID, edgeKind,
		edgeKind, edgeKind, maxDepth,
		maxResults)
	if err != nil {
		return nil, false, nil
	}
	defer rows.Close()
	var out []Reachable
	for rows.Next() {
		var r Reachable
		if err := rows.Scan(&r.ID, &r.Name, &r.FQN, &r.Kind, &r.File,
			&r.Language, &r.StartLine, &r.Depth, &r.ViaEdge); err != nil {
			return nil, false, fmt.Errorf("codegraph.Reachable: scan: %w", err)
		}
		if r.Name == "" {
			continue
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("codegraph.Reachable: rows: %w", err)
	}
	truncated := len(out) >= maxResults
	return out, truncated, nil
}

// edgeTraversal maps a RelationDirection into the (edge_kind,
// seed-side column, traversal-side column) tuple used by the
// Reachable CTE.
//
//	RelationCallers   → CALLS, target_id → source_id  (walk back)
//	RelationCallees   → CALLS, source_id → target_id  (walk forward)
//	RelationContains  → CONTAINS, source_id → target_id (parent → child)
func edgeTraversal(d RelationDirection) (edgeKind, seedCol, recurseCol string) {
	switch d {
	case RelationCallers:
		return "CALLS", "target_id", "source_id"
	case RelationCallees:
		return "CALLS", "source_id", "target_id"
	case RelationContains:
		return "CONTAINS", "source_id", "target_id"
	default:
		return "", "", ""
	}
}

// CountEdgesByKind returns the total number of edges with the given
// kind across the whole graph. Used by the get_relations MCP tool to
// disambiguate "this symbol genuinely has zero relations" from
// "codebase-memory-mcp didn't populate this edge kind" — the latter
// becomes a `degraded: true` response with an upstream-population
// hint, the former a clean `results: []` success.
//
// Returns 0 on unavailable client / missing schema / SQL error.
func (c *Client) CountEdgesByKind(ctx context.Context, kind string) (int, error) {
	if !c.Available() || kind == "" {
		return 0, nil
	}
	c.checkReindex()
	var n int
	err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE kind = ?`, kind).Scan(&n)
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// CallersOf returns symbol names that call functionName via CALLS edges.
// Same availability + schema-tolerance rules.
func (c *Client) CallersOf(ctx context.Context, functionName string) ([]string, error) {
	if !c.Available() || functionName == "" {
		return nil, nil
	}
	c.checkReindex()
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
