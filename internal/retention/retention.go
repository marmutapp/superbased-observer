package retention

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"
)

// Result summarizes one Run.
type Result struct {
	ActionsDeleted          int
	ExcerptsDeleted         int
	FailureContextDeleted   int
	OrphanedSessionsDeleted int
	LogEntriesDeleted       int
	FileStateDeleted        int
	SizePassesRun           int
	DBSizeBytesBefore       int64
	DBSizeBytesAfter        int64
	DurationMs              int64
}

// Options parameterize Run.
type Options struct {
	// MaxAgeDays drops actions whose timestamp is older than this. Zero
	// disables age-based action pruning.
	MaxAgeDays int
	// MaxDBSizeMB caps the DB file size. When exceeded after age pruning,
	// Run shaves off oldest 30-day windows until under the cap. Zero
	// disables size-based pruning.
	MaxDBSizeMB int
	// ObserverLogMaxAgeDays drops observer_log rows older than this. Zero
	// disables log pruning.
	ObserverLogMaxAgeDays int
	// FileStateMaxAgeDays drops file_state rows whose last_seen_at is older
	// than this. Defaults to 30 (spec §19) when zero.
	FileStateMaxAgeDays int
	// DBPath is the path the SQLite db lives at, used to measure file size
	// for size-cap pruning. Required when MaxDBSizeMB > 0.
	DBPath string
}

// Pruner runs retention on a database.
type Pruner struct{ db *sql.DB }

// New wraps an open database.
func New(db *sql.DB) *Pruner { return &Pruner{db: db} }

// Run executes retention with opts and returns a summary. The pruner is
// safe to call on an empty database; missing data is silently ignored.
func (p *Pruner) Run(ctx context.Context, opts Options) (Result, error) {
	start := time.Now()
	res := Result{}
	if p.db == nil {
		return res, errors.New("retention.Run: nil DB")
	}
	res.DBSizeBytesBefore = sizeOf(opts.DBPath)

	if opts.MaxAgeDays > 0 {
		cutoff := nowUTC().AddDate(0, 0, -opts.MaxAgeDays).Format(time.RFC3339Nano)
		if err := p.deleteActionsOlder(ctx, cutoff, &res); err != nil {
			return res, err
		}
	}

	if err := p.deleteOrphanedSessions(ctx, &res); err != nil {
		return res, err
	}

	if opts.ObserverLogMaxAgeDays > 0 {
		cutoff := nowUTC().AddDate(0, 0, -opts.ObserverLogMaxAgeDays).Format(time.RFC3339Nano)
		n, err := p.exec(ctx, `DELETE FROM observer_log WHERE timestamp < ?`, cutoff)
		if err != nil {
			return res, err
		}
		res.LogEntriesDeleted = n
	}

	fileStateAge := opts.FileStateMaxAgeDays
	if fileStateAge <= 0 {
		fileStateAge = 30
	}
	cutoff := nowUTC().AddDate(0, 0, -fileStateAge).Format(time.RFC3339Nano)
	n, err := p.exec(ctx, `DELETE FROM file_state WHERE last_seen_at < ?`, cutoff)
	if err != nil {
		return res, err
	}
	res.FileStateDeleted = n

	// Reclaim WAL space — auto_vacuum isn't enabled, so PRAGMA
	// wal_checkpoint(TRUNCATE) is the cheapest way to shrink storage.
	_, _ = p.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)

	if opts.MaxDBSizeMB > 0 {
		if err := p.shrinkToCap(ctx, opts, &res); err != nil {
			return res, err
		}
	}

	res.DBSizeBytesAfter = sizeOf(opts.DBPath)
	res.DurationMs = time.Since(start).Milliseconds()
	return res, nil
}

// deleteActionsOlder deletes actions with timestamp < cutoff, plus the
// dependent FTS5 excerpts and failure_context rows (which carry FK to
// actions but no ON DELETE CASCADE).
func (p *Pruner) deleteActionsOlder(ctx context.Context, cutoff string, res *Result) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("retention: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	exc, err := tx.ExecContext(ctx,
		`DELETE FROM action_excerpts
		 WHERE action_id IN (SELECT id FROM actions WHERE timestamp < ?)`, cutoff)
	if err != nil {
		return fmt.Errorf("retention: delete excerpts: %w", err)
	}
	if n, err := exc.RowsAffected(); err == nil {
		res.ExcerptsDeleted += int(n)
	}

	fc, err := tx.ExecContext(ctx,
		`DELETE FROM failure_context
		 WHERE action_id IN (SELECT id FROM actions WHERE timestamp < ?)`, cutoff)
	if err != nil {
		return fmt.Errorf("retention: delete failure_context: %w", err)
	}
	if n, err := fc.RowsAffected(); err == nil {
		res.FailureContextDeleted += int(n)
	}

	// file_state references actions(id) via last_action_id; null it out
	// rather than delete the file_state row (which may still be useful for
	// recent freshness checks even after the originating action is pruned).
	if _, err := tx.ExecContext(ctx,
		`UPDATE file_state SET last_action_id = NULL
		 WHERE last_action_id IN (SELECT id FROM actions WHERE timestamp < ?)`, cutoff); err != nil {
		return fmt.Errorf("retention: null file_state.last_action_id: %w", err)
	}

	a, err := tx.ExecContext(ctx, `DELETE FROM actions WHERE timestamp < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("retention: delete actions: %w", err)
	}
	if n, err := a.RowsAffected(); err == nil {
		res.ActionsDeleted += int(n)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("retention: commit: %w", err)
	}
	return nil
}

// deleteOrphanedSessions removes sessions whose action set is empty after
// the actions delete pass AND that no other table still references. Other
// tables (token_usage, failure_context, compaction_events) carry FK NOT
// NULL references to sessions(id) without ON DELETE CASCADE, so a session
// with surviving rows in any of them must stay; deleting it would trip
// the foreign-key constraint.
//
// In practice this matters because subagent compaction turns can land
// token_usage rows for sessions whose JSONL adapter never produced a
// tool_use block (see decision log 2026-04-16). Those sessions look
// orphaned by the actions table alone, but their token_usage data is
// still load-bearing for cost rollups.
func (p *Pruner) deleteOrphanedSessions(ctx context.Context, res *Result) error {
	r, err := p.db.ExecContext(ctx,
		`DELETE FROM sessions
		 WHERE id NOT IN (SELECT DISTINCT session_id FROM actions)
		   AND id NOT IN (SELECT DISTINCT session_id FROM token_usage)
		   AND id NOT IN (SELECT DISTINCT session_id FROM failure_context)
		   AND id NOT IN (SELECT DISTINCT session_id FROM compaction_events)`)
	if err != nil {
		return fmt.Errorf("retention: orphaned sessions: %w", err)
	}
	if n, err := r.RowsAffected(); err == nil {
		res.OrphanedSessionsDeleted = int(n)
	}
	return nil
}

// shrinkToCap iteratively drops the oldest 30-day window of actions until
// the DB file is under the size cap. Capped at 12 iterations as a safety
// rail against an unexpectedly small max_db_size_mb (e.g. 1MB while
// individual sessions stay open).
func (p *Pruner) shrinkToCap(ctx context.Context, opts Options, res *Result) error {
	cap := int64(opts.MaxDBSizeMB) * 1024 * 1024
	for i := 0; i < 12; i++ {
		size := sizeOf(opts.DBPath)
		if size <= cap {
			return nil
		}
		var minTS sql.NullString
		if err := p.db.QueryRowContext(ctx,
			`SELECT MIN(timestamp) FROM actions`).Scan(&minTS); err != nil {
			return fmt.Errorf("retention: min timestamp: %w", err)
		}
		if !minTS.Valid {
			return nil // no actions to shave
		}
		min, err := time.Parse(time.RFC3339Nano, minTS.String)
		if err != nil {
			return fmt.Errorf("retention: parse min timestamp %q: %w", minTS.String, err)
		}
		cutoff := min.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
		if err := p.deleteActionsOlder(ctx, cutoff, res); err != nil {
			return err
		}
		if err := p.deleteOrphanedSessions(ctx, res); err != nil {
			return err
		}
		_, _ = p.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
		// VACUUM compacts the main file to reflect deletes; this is the
		// only way to shrink the .db file without auto_vacuum enabled.
		if _, err := p.db.ExecContext(ctx, `VACUUM`); err != nil {
			// VACUUM can fail under heavy concurrent load; non-fatal.
			break
		}
		res.SizePassesRun++
	}
	return nil
}

// exec runs a DELETE / UPDATE and returns rows affected. Wraps the boilerplate
// the per-pass deletes don't need transactions for.
func (p *Pruner) exec(ctx context.Context, q string, args ...any) (int, error) {
	r, err := p.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("retention.exec: %w", err)
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// sizeOf returns the file size at path, or 0 on any error. Returns 0 for
// ":memory:" databases too (the path won't exist).
func sizeOf(path string) int64 {
	if path == "" || path == ":memory:" {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// nowUTC is a var so tests can inject a fixed clock.
var nowUTC = func() time.Time { return time.Now().UTC() }
