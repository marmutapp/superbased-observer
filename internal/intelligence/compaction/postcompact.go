package compaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/learn"
)

// PostCompactQueryLimits caps the per-section row counts in the
// generated synthetic system message. Tuned to fit comfortably under a
// few hundred tokens without losing the load-bearing recovery signal.
const (
	postCompactReadLimit    = 10
	postCompactEditLimit    = 5
	postCompactFailureLimit = 3
	postCompactRuleLimit    = 5
	postCompactRuleDays     = 30
)

// BuildPostCompactContext returns a synthetic content string summarising
// the session's pre-compaction state (last reads, last edits, recent
// failures, learn corrections). Empty when the session has no
// compaction event or no recovery data.
//
// All queries are scoped to `timestamp <= compactionAt` so the output
// is **stable across turns** for the same compaction event — turn N+1
// builds the byte-identical content as turn N as long as no new
// compaction lands. This is the cross-turn invariance predicate that
// Anthropic's prefix cache depends on once the proxy injects the
// content into the request envelope.
func BuildPostCompactContext(ctx context.Context, db *sql.DB, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}

	var compactionAt string
	var pid int64
	err := db.QueryRowContext(ctx,
		`SELECT timestamp, project_id FROM compaction_events
		 WHERE session_id = ? ORDER BY timestamp DESC LIMIT 1`,
		sessionID).Scan(&compactionAt, &pid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("compaction.BuildPostCompactContext: lookup event: %w", err)
	}

	reads, err := queryDistinctTargets(ctx, db, sessionID, []string{"read_file"}, compactionAt, postCompactReadLimit)
	if err != nil {
		return "", err
	}
	edits, err := queryDistinctTargets(ctx, db, sessionID, []string{"edit_file", "write_file"}, compactionAt, postCompactEditLimit)
	if err != nil {
		return "", err
	}
	fails, err := queryRecentFailures(ctx, db, sessionID, compactionAt, postCompactFailureLimit)
	if err != nil {
		return "", err
	}

	var rules []learn.Rule
	var projectRoot string
	if pid != 0 {
		_ = db.QueryRowContext(ctx, `SELECT root_path FROM projects WHERE id = ?`, pid).Scan(&projectRoot)
	}
	if projectRoot != "" {
		rules, _ = learn.New(db).Derive(ctx, learn.Options{
			ProjectRoot: projectRoot,
			Days:        postCompactRuleDays,
			Limit:       postCompactRuleLimit,
		})
	}

	if len(reads) == 0 && len(edits) == 0 && len(fails) == 0 && len(rules) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("<observer-compaction-recovery>\n")
	fmt.Fprintf(&b, "Session %s underwent context compaction. Below is the most recent activity from before the compaction so you can re-orient without re-reading every file.\n", sessionID)

	if len(reads) > 0 {
		b.WriteString("\nRecently read files:\n")
		for _, p := range reads {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if len(edits) > 0 {
		b.WriteString("\nRecently edited files:\n")
		for _, p := range edits {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}
	if len(fails) > 0 {
		b.WriteString("\nRecent failures (in this session):\n")
		for _, f := range fails {
			fmt.Fprintf(&b, "  - %s — %s\n", f.summary, f.message)
		}
	}
	if len(rules) > 0 {
		b.WriteString("\nProject-specific learned rules (from prior sessions):\n")
		for _, r := range rules {
			fmt.Fprintf(&b, "  - %s [%s] — failed %d times, recovered %d times\n",
				r.CommandSummary, r.ErrorCategory, r.FailureCount, r.RecoveryCount)
		}
	}
	b.WriteString("</observer-compaction-recovery>")
	return b.String(), nil
}

// queryDistinctTargets returns up to `limit` distinct target paths
// touched by `actionTypes` in the session, ordered by most-recent
// timestamp at-or-before `cutoff`.
func queryDistinctTargets(ctx context.Context, db *sql.DB, sessionID string, actionTypes []string, cutoff string, limit int) ([]string, error) {
	if len(actionTypes) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(actionTypes))
	args := []any{sessionID}
	for i, at := range actionTypes {
		placeholders[i] = "?"
		args = append(args, at)
	}
	args = append(args, cutoff, limit)
	//nolint:gosec // G201: the only format arg is a code-built placeholder list (?,?,…); all values are bound via ? args.
	q := fmt.Sprintf(
		`SELECT target FROM (
		   SELECT target, MAX(timestamp) AS ts FROM actions
		   WHERE session_id = ? AND action_type IN (%s)
		         AND timestamp <= ? AND target != ''
		   GROUP BY target
		 ) ORDER BY ts DESC LIMIT ?`,
		strings.Join(placeholders, ","),
	)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("compaction: queryDistinctTargets: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("compaction: queryDistinctTargets scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// failureSummary holds the columns we surface in the synthetic message.
type failureSummary struct {
	summary string
	message string
}

// queryRecentFailures returns up to `limit` failures from
// failure_context for the session, on or before `cutoff`, ordered
// most-recent first.
func queryRecentFailures(ctx context.Context, db *sql.DB, sessionID, cutoff string, limit int) ([]failureSummary, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT command_summary, COALESCE(error_message, '') FROM failure_context
		 WHERE session_id = ? AND timestamp <= ?
		 ORDER BY timestamp DESC LIMIT ?`,
		sessionID, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("compaction: queryRecentFailures: %w", err)
	}
	defer rows.Close()
	var out []failureSummary
	for rows.Next() {
		var f failureSummary
		if err := rows.Scan(&f.summary, &f.message); err != nil {
			return nil, fmt.Errorf("compaction: queryRecentFailures scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Injector caches the synthetic post-compact content per session so
// the proxy can prepend the same byte-identical block on every turn
// of a post-compact conversation. Cross-turn invariance follows from
// (a) [BuildPostCompactContext] being a pure function of the DB rows
// at-or-before the compaction timestamp and (b) the cache keying on
// the compaction timestamp itself — a new compaction event invalidates
// the cached content automatically.
type Injector struct {
	db *sql.DB

	mu    sync.Mutex
	cache map[string]injectorCacheEntry
}

type injectorCacheEntry struct {
	compactionAt string
	content      string
}

// NewInjector wraps db.
func NewInjector(db *sql.DB) *Injector {
	return &Injector{db: db, cache: map[string]injectorCacheEntry{}}
}

// Get returns the synthetic post-compact content for sessionID, or ""
// when the session has no compaction event or no recovery data. Safe
// to call every turn — the result is cached per compaction event so
// repeated calls within the same compaction window are O(1) DB reads.
func (i *Injector) Get(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return "", nil
	}

	var compactionAt string
	err := i.db.QueryRowContext(ctx,
		`SELECT timestamp FROM compaction_events
		 WHERE session_id = ? ORDER BY timestamp DESC LIMIT 1`,
		sessionID).Scan(&compactionAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("compaction.Injector.Get: lookup event: %w", err)
	}

	i.mu.Lock()
	if entry, ok := i.cache[sessionID]; ok && entry.compactionAt == compactionAt {
		i.mu.Unlock()
		return entry.content, nil
	}
	i.mu.Unlock()

	content, err := BuildPostCompactContext(ctx, i.db, sessionID)
	if err != nil {
		return "", err
	}

	i.mu.Lock()
	i.cache[sessionID] = injectorCacheEntry{compactionAt: compactionAt, content: content}
	i.mu.Unlock()

	// Mark the compaction_events row as injected. Best-effort —
	// failure to update is logged silently because losing visibility
	// telemetry must never break the actual injection flow. The
	// `injected_at IS NULL` guard makes this idempotent: repeat
	// invocations within the same compaction window are no-ops at the
	// SQL level, so the column captures the FIRST-injection timestamp
	// rather than the most-recent.
	if content != "" {
		_, _ = i.db.ExecContext(ctx,
			`UPDATE compaction_events SET injected_at = ?
			 WHERE id = (SELECT id FROM compaction_events
			             WHERE session_id = ? AND timestamp = ?
			             ORDER BY id DESC LIMIT 1)
			   AND injected_at IS NULL`,
			time.Now().UTC().Format(time.RFC3339Nano),
			sessionID, compactionAt)
	}
	return content, nil
}
