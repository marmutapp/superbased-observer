package learn

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Rule is one derived correction pattern.
type Rule struct {
	Project        string   `json:"project"`
	CommandHash    string   `json:"command_hash"`
	CommandSummary string   `json:"command_summary"`
	ErrorCategory  string   `json:"error_category"`
	ErrorMessage   string   `json:"error_message"`
	FailureCount   int      `json:"failure_count"`
	RecoveryCount  int      `json:"recovery_count"`
	EditedFiles    []string `json:"edited_files"`
	FailureToolMix []string `json:"failure_tool_mix"`
	LastObservedAt string   `json:"last_observed_at,omitempty"`
}

// Options parameterize Derive.
type Options struct {
	ProjectRoot string
	Since       time.Time
	Days        int
	// MinFailures drops rules whose underlying failure count is below N.
	// Defaults to 2 — one-off failures shouldn't become rules.
	MinFailures int
	// Limit caps the returned rule count. Zero means 50.
	Limit int
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// Learner scans a DB for rules.
type Learner struct{ db *sql.DB }

// New wraps db.
func New(db *sql.DB) *Learner { return &Learner{db: db} }

// failure is one row from failure_context joined with project + session tool
// and the originating action's target (the raw command string).
type failure struct {
	projectID           int64
	projectRoot         string
	commandHash         string
	commandSummary      string
	errorCategory       string
	errorMessage        string
	sessionID           string
	actionID            int64
	actionTarget        string
	timestamp           time.Time
	eventuallySucceeded bool
	retryCount          int
	tool                string
}

// Derive returns the rules visible to opts. Sorted by FailureCount desc.
func (l *Learner) Derive(ctx context.Context, opts Options) ([]Rule, error) {
	if opts.MinFailures <= 0 {
		opts.MinFailures = 2
	}
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	since := opts.Since
	if since.IsZero() && opts.Days > 0 {
		since = opts.Now().Add(-time.Duration(opts.Days) * 24 * time.Hour)
	}

	fails, err := l.loadFailures(ctx, opts.ProjectRoot, since)
	if err != nil {
		return nil, err
	}

	// Group by (projectID, commandHash).
	type groupKey struct {
		pid  int64
		hash string
	}
	groups := map[groupKey][]failure{}
	for _, f := range fails {
		groups[groupKey{f.projectID, f.commandHash}] = append(groups[groupKey{f.projectID, f.commandHash}], f)
	}

	var out []Rule
	for _, fs := range groups {
		if len(fs) < opts.MinFailures {
			continue
		}
		r, err := l.ruleFromGroup(ctx, fs)
		if err != nil {
			return nil, err
		}
		if r != nil {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FailureCount != out[j].FailureCount {
			return out[i].FailureCount > out[j].FailureCount
		}
		return out[i].RecoveryCount > out[j].RecoveryCount
	})
	if len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (l *Learner) loadFailures(ctx context.Context, projectRoot string, since time.Time) ([]failure, error) {
	q := `SELECT fc.project_id, COALESCE(p.root_path, ''),
	             fc.command_hash, fc.command_summary,
	             COALESCE(fc.error_category, ''), COALESCE(fc.error_message, ''),
	             fc.session_id, fc.action_id, COALESCE(a.target, ''), fc.timestamp,
	             fc.eventually_succeeded, COALESCE(fc.retry_count, 0),
	             COALESCE(s.tool, '')
	      FROM failure_context fc
	      LEFT JOIN projects p ON p.id = fc.project_id
	      LEFT JOIN sessions s ON s.id = fc.session_id
	      LEFT JOIN actions a ON a.id = fc.action_id
	      WHERE 1 = 1`
	var args []any
	if projectRoot != "" {
		q += " AND p.root_path = ?"
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		q += " AND fc.timestamp >= ?"
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	q += " ORDER BY fc.project_id, fc.command_hash, fc.timestamp"

	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("learn: query failures: %w", err)
	}
	defer rows.Close()
	var out []failure
	for rows.Next() {
		var f failure
		var ts string
		var recovered int
		if err := rows.Scan(&f.projectID, &f.projectRoot, &f.commandHash,
			&f.commandSummary, &f.errorCategory, &f.errorMessage,
			&f.sessionID, &f.actionID, &f.actionTarget, &ts,
			&recovered, &f.retryCount, &f.tool); err != nil {
			return nil, fmt.Errorf("learn: scan: %w", err)
		}
		f.timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		f.eventuallySucceeded = recovered != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

func (l *Learner) ruleFromGroup(ctx context.Context, fs []failure) (*Rule, error) {
	if len(fs) == 0 {
		return nil, nil
	}
	sample := fs[0]
	recovery := 0
	latest := time.Time{}
	toolSet := map[string]bool{}
	editSet := map[string]bool{}

	// Cache successful run timestamps of this command hash per session so
	// we only have to query once per session.
	successesBySession := map[string][]time.Time{}

	for _, f := range fs {
		if f.timestamp.After(latest) {
			latest = f.timestamp
		}
		if f.tool != "" {
			toolSet[f.tool] = true
		}
		// For each failure, find the next success of the same command in
		// the same session and collect the edits between them.
		sts, err := l.sessionSuccesses(ctx, f.sessionID, f.actionTarget, successesBySession)
		if err != nil {
			return nil, err
		}
		var nextSuccess time.Time
		for _, s := range sts {
			if s.After(f.timestamp) {
				nextSuccess = s
				break
			}
		}
		if nextSuccess.IsZero() {
			continue
		}
		recovery++
		edits, err := l.editsBetween(ctx, f.sessionID, f.timestamp, nextSuccess)
		if err != nil {
			return nil, err
		}
		for _, e := range edits {
			editSet[e] = true
		}
	}

	// Skip groups with no recoveries — there's no correction pattern to
	// encode if the command never succeeded.
	if recovery == 0 {
		return nil, nil
	}

	edits := keysOf(editSet)
	if len(edits) > 8 {
		edits = edits[:8] // cap to keep emitted rules terse
	}

	return &Rule{
		Project:        sample.projectRoot,
		CommandHash:    sample.commandHash,
		CommandSummary: sample.commandSummary,
		ErrorCategory:  sample.errorCategory,
		ErrorMessage:   sample.errorMessage,
		FailureCount:   len(fs),
		RecoveryCount:  recovery,
		EditedFiles:    edits,
		FailureToolMix: keysOf(toolSet),
		LastObservedAt: latest.UTC().Format(time.RFC3339),
	}, nil
}

// sessionSuccesses returns the ascending timestamps of successful
// run_command actions with the given target (command string) in the given
// session. Result is cached so repeated calls for the same group don't
// re-query.
func (l *Learner) sessionSuccesses(ctx context.Context, sessionID, target string, cache map[string][]time.Time) ([]time.Time, error) {
	cacheKey := sessionID + "\x00" + target
	if v, ok := cache[cacheKey]; ok {
		return v, nil
	}
	rows, err := l.db.QueryContext(ctx,
		`SELECT a.timestamp FROM actions a
		 WHERE a.session_id = ? AND a.action_type = 'run_command' AND a.success = 1
		       AND a.target = ?
		 ORDER BY a.timestamp`, sessionID, target)
	if err != nil {
		return nil, fmt.Errorf("learn: session successes: %w", err)
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var ts string
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			out = append(out, parsed)
		}
	}
	cache[cacheKey] = out
	return out, rows.Err()
}

func (l *Learner) editsBetween(ctx context.Context, sessionID string, lo, hi time.Time) ([]string, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT DISTINCT target FROM actions
		 WHERE session_id = ? AND action_type IN (?, ?)
		       AND timestamp > ? AND timestamp < ? AND target != ''`,
		sessionID, models.ActionEditFile, models.ActionWriteFile,
		lo.UTC().Format(time.RFC3339Nano),
		hi.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("learn: edits between: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
