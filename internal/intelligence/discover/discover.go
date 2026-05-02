package discover

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Report is the full discover output.
type Report struct {
	Project          string             `json:"project,omitempty"`
	Days             int                `json:"days,omitempty"`
	Since            string             `json:"since,omitempty"`
	StaleReads       []StaleReadFile    `json:"stale_reads"`
	RepeatedCommands []RepeatedCommand  `json:"repeated_commands"`
	CrossToolFiles   []CrossToolFile    `json:"cross_tool_files"`
	NativeVsBash     []NativeBashBucket `json:"native_vs_bash"`
	Summary          ReportSummary      `json:"summary"`
}

// ReportSummary carries roll-up counters that the CLI prints as a header.
type ReportSummary struct {
	TotalActions   int `json:"total_actions"`
	StaleReadCount int `json:"stale_read_count"`
	// CrossThreadStaleCount is the subset of StaleReadCount where the
	// stale read crossed the parent/sub-agent boundary — the prior in-
	// session read of the same file was on the other side
	// (is_sidechain flipped). Different actionable advice from main-
	// thread stales: pass content via the Agent tool's prompt
	// parameter rather than letting the child re-read.
	CrossThreadStaleCount int   `json:"cross_thread_stale_count"`
	EstWastedTokens       int64 `json:"est_wasted_tokens"`
	RepeatedCmdGroups     int   `json:"repeated_command_groups"`
	CrossToolFileCount    int   `json:"cross_tool_file_count"`
	NativeActionCount     int   `json:"native_action_count"`
	BashActionCount       int   `json:"bash_action_count"`
}

// StaleReadFile is one file that was re-read in "stale" state multiple times.
type StaleReadFile struct {
	FilePath   string `json:"file_path"`
	Project    string `json:"project"`
	StaleCount int    `json:"stale_count"`
	// CrossThreadStaleCount is the subset of StaleCount that crossed
	// the parent/sub-agent boundary (this read on one side, the prior
	// in-session read on the other). When non-zero, the actionable
	// fix shifts from cache_control to "pass content via Agent's
	// prompt parameter".
	CrossThreadStaleCount int   `json:"cross_thread_stale_count"`
	TotalReads            int   `json:"total_reads"`
	EstWastedTokens       int64 `json:"est_wasted_tokens"`
	FileSizeBytes         int64 `json:"file_size_bytes"`
}

// RepeatedCommand is one command that ran multiple times in a project.
// NoChangeReruns is the subset of reruns where no file edits happened
// between consecutive runs — the ones truly "for free."
type RepeatedCommand struct {
	Command        string `json:"command"`
	CommandHash    string `json:"command_hash"`
	Project        string `json:"project"`
	TotalRuns      int    `json:"total_runs"`
	NoChangeReruns int    `json:"no_change_reruns"`
	Successful     int    `json:"successful_runs"`
	Failed         int    `json:"failed_runs"`
}

// CrossToolFile is one file path touched by multiple tools (spec §15.1:
// "cross-tool redundancy"). Tools list is deduped and sorted.
type CrossToolFile struct {
	FilePath string   `json:"file_path"`
	Project  string   `json:"project"`
	Tools    []string `json:"tools"`
	Accesses int      `json:"accesses"`
}

// CrossThreadStale is the cross-thread subset of a stale-rereads file
// row: same file, same session, but the prior read happened on the
// OTHER side of the parent/sub-agent boundary (the parent thread read
// it then a sub-agent re-read it, or vice versa).
type CrossThreadStale struct {
	FilePath         string `json:"file_path"`
	Project          string `json:"project"`
	CrossThreadCount int    `json:"cross_thread_count"`
	TotalReads       int    `json:"total_reads"`
	EstWastedTokens  int64  `json:"est_wasted_tokens"`
	FileSizeBytes    int64  `json:"file_size_bytes"`
}

// NativeBashBucket is action_type × is_native_tool bucket. Surfacing this
// is the easy win over RTK-style tools: we see native tool calls, not just
// Bash invocations (spec §15.1, §26 competitive table).
type NativeBashBucket struct {
	ActionType string `json:"action_type"`
	IsNative   bool   `json:"is_native"`
	Count      int    `json:"count"`
}

// Options parameterize Report.
type Options struct {
	// ProjectRoot filters to one project. Empty means all projects.
	ProjectRoot string
	// Days restricts to the last N days. Zero disables.
	Days int
	// Since overrides Days when non-zero.
	Since time.Time
	// Limit caps each section's row count (stale reads, repeats, cross-tool).
	// Zero means 20.
	Limit int
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// Discoverer runs a pass against a DB.
type Discoverer struct{ db *sql.DB }

// New wraps db.
func New(db *sql.DB) *Discoverer { return &Discoverer{db: db} }

// Run builds the report.
func (d *Discoverer) Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Limit <= 0 {
		opts.Limit = 20
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	since := opts.Since
	if since.IsZero() && opts.Days > 0 {
		since = opts.Now().Add(-time.Duration(opts.Days) * 24 * time.Hour)
	}

	report := Report{
		Project: opts.ProjectRoot,
		Days:    opts.Days,
	}
	if !since.IsZero() {
		report.Since = since.UTC().Format(time.RFC3339)
	}

	total, err := d.totalActions(ctx, opts.ProjectRoot, since)
	if err != nil {
		return Report{}, err
	}
	report.Summary.TotalActions = total

	stale, err := d.staleReads(ctx, opts.ProjectRoot, since, opts.Limit)
	if err != nil {
		return Report{}, err
	}
	report.StaleReads = stale
	for _, s := range stale {
		report.Summary.StaleReadCount += s.StaleCount
		report.Summary.CrossThreadStaleCount += s.CrossThreadStaleCount
		report.Summary.EstWastedTokens += s.EstWastedTokens
	}

	reps, err := d.repeatedCommands(ctx, opts.ProjectRoot, since, opts.Limit)
	if err != nil {
		return Report{}, err
	}
	report.RepeatedCommands = reps
	report.Summary.RepeatedCmdGroups = len(reps)

	cross, err := d.crossToolFiles(ctx, opts.ProjectRoot, since, opts.Limit)
	if err != nil {
		return Report{}, err
	}
	report.CrossToolFiles = cross
	report.Summary.CrossToolFileCount = len(cross)

	nb, err := d.nativeVsBash(ctx, opts.ProjectRoot, since)
	if err != nil {
		return Report{}, err
	}
	report.NativeVsBash = nb
	for _, b := range nb {
		if b.IsNative {
			report.Summary.NativeActionCount += b.Count
		} else {
			report.Summary.BashActionCount += b.Count
		}
	}
	return report, nil
}

func (d *Discoverer) totalActions(ctx context.Context, projectRoot string, since time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM actions a
	      LEFT JOIN projects p ON p.id = a.project_id`
	var where []string
	var args []any
	if projectRoot != "" {
		where = append(where, "p.root_path = ?")
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		where = append(where, "a.timestamp >= ?")
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := d.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("discover: total actions: %w", err)
	}
	return n, nil
}

func (d *Discoverer) staleReads(ctx context.Context, projectRoot string, since time.Time, limit int) ([]StaleReadFile, error) {
	// Join actions to file_state for sizes. The heuristic for wasted tokens
	// is stale_count * ceil(file_size / 4) — Claude's tokenizer averages
	// ~4 bytes/token for typical source code. When file_size is unknown,
	// fall back to 2KB per read (excerpt size).
	//
	// Cross-session scoping: only count a read as stale when there's an
	// earlier read of the same file in the same session. The freshness
	// engine tags reads as stale by comparing against file_state, which
	// is project-wide — so a fresh session re-reading a file changed by
	// a previous session would be tagged stale even though the new
	// session has no memory of the prior read. That's a misleading signal
	// and the EXISTS subquery filters it out.
	q := `SELECT a.target, COALESCE(p.root_path, ''),
	             SUM(CASE WHEN a.freshness = 'stale'
	                       AND EXISTS (
	                         SELECT 1 FROM actions a2
	                         WHERE a2.session_id = a.session_id
	                           AND a2.project_id = a.project_id
	                           AND a2.target     = a.target
	                           AND a2.action_type = 'read_file'
	                           AND a2.timestamp  < a.timestamp
	                       )
	                      THEN 1 ELSE 0 END) AS stale_count,
	             SUM(CASE WHEN a.freshness = 'stale'
	                       AND EXISTS (
	                         SELECT 1 FROM actions a2
	                         WHERE a2.session_id = a.session_id
	                           AND a2.project_id = a.project_id
	                           AND a2.target     = a.target
	                           AND a2.action_type = 'read_file'
	                           AND a2.timestamp  < a.timestamp
	                           AND a2.is_sidechain != a.is_sidechain
	                       )
	                      THEN 1 ELSE 0 END) AS cross_thread_count,
	             COUNT(*) AS total_reads,
	             COALESCE(MAX(fs.file_size_bytes), 0) AS size_bytes
	      FROM actions a
	      LEFT JOIN projects p ON p.id = a.project_id
	      LEFT JOIN file_state fs ON fs.project_id = a.project_id
	                              AND fs.file_path = a.target
	      WHERE a.action_type = 'read_file' AND a.target != ''`
	var args []any
	if projectRoot != "" {
		q += " AND p.root_path = ?"
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		q += " AND a.timestamp >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` GROUP BY a.target, a.project_id
	       HAVING stale_count > 0
	       ORDER BY stale_count DESC, total_reads DESC
	       LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("discover: stale reads: %w", err)
	}
	defer rows.Close()
	var out []StaleReadFile
	for rows.Next() {
		var r StaleReadFile
		if err := rows.Scan(&r.FilePath, &r.Project, &r.StaleCount, &r.CrossThreadStaleCount, &r.TotalReads, &r.FileSizeBytes); err != nil {
			return nil, fmt.Errorf("discover: scan stale: %w", err)
		}
		tokens := r.FileSizeBytes / 4
		if tokens == 0 {
			tokens = 512 // fallback: 2KB excerpt ≈ 512 tokens.
		}
		r.EstWastedTokens = tokens * int64(r.StaleCount)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *Discoverer) repeatedCommands(ctx context.Context, projectRoot string, since time.Time, limit int) ([]RepeatedCommand, error) {
	// Group run_command actions by (project, target). Pull success mix from
	// the actions themselves. "No-change reruns" are intervals between two
	// consecutive runs of the same command where no edit_file / write_file
	// action occurred in the same session — the cheap "did you forget you
	// already ran this" signal.
	q := `SELECT a.target, COALESCE(p.root_path, ''),
	             COUNT(*) AS total_runs,
	             SUM(CASE WHEN a.success THEN 1 ELSE 0 END) AS successful,
	             SUM(CASE WHEN NOT a.success THEN 1 ELSE 0 END) AS failed
	      FROM actions a
	      LEFT JOIN projects p ON p.id = a.project_id
	      WHERE a.action_type = 'run_command' AND a.target != ''`
	var args []any
	if projectRoot != "" {
		q += " AND p.root_path = ?"
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		q += " AND a.timestamp >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` GROUP BY a.target, a.project_id
	       HAVING total_runs > 1
	       ORDER BY total_runs DESC
	       LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("discover: repeated commands: %w", err)
	}
	defer rows.Close()
	var out []RepeatedCommand
	for rows.Next() {
		var r RepeatedCommand
		if err := rows.Scan(&r.Command, &r.Project, &r.TotalRuns, &r.Successful, &r.Failed); err != nil {
			return nil, fmt.Errorf("discover: scan repeated: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Second pass: count no-change reruns per row. Separate query so the
	// GROUP BY stays cheap.
	for i := range out {
		n, err := d.countNoChangeReruns(ctx, out[i].Project, out[i].Command, since)
		if err != nil {
			return nil, err
		}
		out[i].NoChangeReruns = n
	}
	return out, nil
}

// countNoChangeReruns walks every pair of consecutive runs of (project, cmd)
// and counts those with no edit_file/write_file action in the same session
// between them.
func (d *Discoverer) countNoChangeReruns(ctx context.Context, projectRoot, command string, since time.Time) (int, error) {
	q := `SELECT a.session_id, a.timestamp
	      FROM actions a
	      LEFT JOIN projects p ON p.id = a.project_id
	      WHERE a.action_type = 'run_command' AND a.target = ?
	            AND p.root_path = ?`
	args := []any{command, projectRoot}
	if !since.IsZero() {
		q += " AND a.timestamp >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY a.session_id, a.timestamp"

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("discover: runs: %w", err)
	}
	defer rows.Close()
	type runKey struct {
		session string
		ts      time.Time
	}
	var runs []runKey
	for rows.Next() {
		var k runKey
		var ts string
		if err := rows.Scan(&k.session, &ts); err != nil {
			return 0, fmt.Errorf("discover: scan run: %w", err)
		}
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			k.ts = parsed
			runs = append(runs, k)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	count := 0
	for i := 1; i < len(runs); i++ {
		prev, cur := runs[i-1], runs[i]
		if prev.session != cur.session {
			continue
		}
		var edits int
		if err := d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM actions
			 WHERE session_id = ? AND timestamp > ? AND timestamp < ?
			       AND action_type IN ('edit_file', 'write_file')`,
			cur.session,
			prev.ts.UTC().Format(time.RFC3339Nano),
			cur.ts.UTC().Format(time.RFC3339Nano)).Scan(&edits); err != nil {
			return 0, fmt.Errorf("discover: count edits: %w", err)
		}
		if edits == 0 {
			count++
		}
	}
	return count, nil
}

func (d *Discoverer) crossToolFiles(ctx context.Context, projectRoot string, since time.Time, limit int) ([]CrossToolFile, error) {
	q := `SELECT a.target, COALESCE(p.root_path, ''),
	             GROUP_CONCAT(DISTINCT a.tool) AS tools,
	             COUNT(*) AS accesses,
	             COUNT(DISTINCT a.tool) AS tool_count
	      FROM actions a
	      LEFT JOIN projects p ON p.id = a.project_id
	      WHERE a.action_type IN ('read_file', 'edit_file', 'write_file')
	            AND a.target != ''`
	var args []any
	if projectRoot != "" {
		q += " AND p.root_path = ?"
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		q += " AND a.timestamp >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += ` GROUP BY a.target, a.project_id
	       HAVING tool_count > 1
	       ORDER BY accesses DESC
	       LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("discover: cross-tool: %w", err)
	}
	defer rows.Close()
	var out []CrossToolFile
	for rows.Next() {
		var r CrossToolFile
		var toolsCSV string
		var toolCount int
		if err := rows.Scan(&r.FilePath, &r.Project, &toolsCSV, &r.Accesses, &toolCount); err != nil {
			return nil, fmt.Errorf("discover: scan cross-tool: %w", err)
		}
		r.Tools = splitAndSort(toolsCSV)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *Discoverer) nativeVsBash(ctx context.Context, projectRoot string, since time.Time) ([]NativeBashBucket, error) {
	q := `SELECT a.action_type, COALESCE(a.is_native_tool, 0), COUNT(*)
	      FROM actions a
	      LEFT JOIN projects p ON p.id = a.project_id`
	var where []string
	var args []any
	if projectRoot != "" {
		where = append(where, "p.root_path = ?")
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		where = append(where, "a.timestamp >= ?")
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " GROUP BY a.action_type, a.is_native_tool ORDER BY COUNT(*) DESC"

	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("discover: native vs bash: %w", err)
	}
	defer rows.Close()
	var out []NativeBashBucket
	for rows.Next() {
		var b NativeBashBucket
		var native int
		if err := rows.Scan(&b.ActionType, &native, &b.Count); err != nil {
			return nil, fmt.Errorf("discover: scan native: %w", err)
		}
		b.IsNative = native != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

func splitAndSort(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	sort.Strings(parts)
	return parts
}
