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
	// SizeCapUnmet is set when the size-cap shrink stopped while the DB
	// was still over MaxDBSizeMB — because shedding aged `actions` (the
	// only table the size loop prunes) couldn't bring it under cap. That
	// means the bulk is in OTHER tables (token_usage / cache_*), and the
	// operator should raise max_db_size_mb or add token retention rather
	// than have the size loop destroy the actions table. The caller logs
	// a WARN. Pre-fix the loop kept deleting actions to (near) zero
	// chasing an unreachable cap; now it respects an action-keep floor
	// and stops here instead.
	SizeCapUnmet bool
	// CacheRowsDeleted is the count of cache_* table rows removed
	// by the cachetrack sweep (spec §9). The retention package
	// itself doesn't touch cache_* tables (sql.DB-only by design);
	// the orchestration layer (cmd/observer/prune.go::runRetention)
	// calls store.PruneCacheRows after Run and sets this field on
	// the returned Result. Zero when [cachetrack].retention_days
	// is ≤ 0 (sweep disabled) or no eligible rows existed.
	CacheRowsDeleted int
	// RouterDecisionsDeleted is the count of router_decisions rows
	// removed by the routing decision-log sweep
	// ([routing].decision_log_retention_days, model-routing spec
	// §R21). Same orchestration pattern as CacheRowsDeleted: the
	// retention package never touches the table; runRetention calls
	// store.PruneRouterDecisions and sets this field.
	RouterDecisionsDeleted int64
	// GuardRowsDeleted is the count of guard_* rows removed by the
	// guard §10.3 sweep — same orchestration pattern as
	// CacheRowsDeleted (store.PruneGuardRows runs after Run; the
	// chain checkpoint keeps the audit chain verifiable across the
	// prune). Zero when [guard].retention_days is ≤ 0.
	GuardRowsDeleted int
	// ProcessRowsDeleted is the count of process_runs + process_events rows
	// removed by the process-observability sweep
	// ([observer.process].retention_days, docs/process-observability.md §11).
	// Same orchestration pattern as CacheRowsDeleted: the retention package
	// never touches the tables; runRetention calls store.PruneProcessRows
	// and sets this field. Zero when the feature is disabled or the horizon
	// is ≤ 0.
	ProcessRowsDeleted int
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

	// Later migrations added more nullable action_id FKs to actions, none
	// with ON DELETE CASCADE: retrieval_signals (014), guard_events (040),
	// process_runs + process_events (044). A surviving reference to a pruned
	// action trips "FOREIGN KEY constraint failed (787)" and aborts the whole
	// DELETE — the live regression that left the actions table un-pruned
	// (2026-06-18). Null each ref rather than delete the row, matching the
	// file_state handling above: retrieval_signals preserves the K43 long
	// tail (014 comment), guard_events is an append-only audit chain with its
	// own retention, and process_runs/process_events carry their own
	// retention horizon — each row stays useful without the action link.
	for _, c := range []struct{ table, stmt string }{
		{"retrieval_signals", `UPDATE retrieval_signals SET action_id = NULL WHERE action_id IN (SELECT id FROM actions WHERE timestamp < ?)`},
		{"guard_events", `UPDATE guard_events SET action_id = NULL WHERE action_id IN (SELECT id FROM actions WHERE timestamp < ?)`},
		{"process_runs", `UPDATE process_runs SET action_id = NULL WHERE action_id IN (SELECT id FROM actions WHERE timestamp < ?)`},
		{"process_events", `UPDATE process_events SET action_id = NULL WHERE action_id IN (SELECT id FROM actions WHERE timestamp < ?)`},
	} {
		if _, err := tx.ExecContext(ctx, c.stmt, cutoff); err != nil {
			return fmt.Errorf("retention: null %s.action_id: %w", c.table, err)
		}
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

// sizeCapActionFloorDays is the keep-floor for the size-cap shrink: it
// will never delete `actions` newer than this many days, no matter how
// far over the size cap the DB is. The size loop only sheds `actions`, so
// without a floor a DB bloated by OTHER tables (token_usage / cache_*)
// drives it to delete the entire actions history chasing a cap it can't
// reach — which is exactly what nuked an operator's 68K-row actions table
// down to ~90 (to reclaim ~87MB on a 521MB DB). With the floor, recent
// actions (and their user_prompt boundaries, FTS excerpts, process links)
// are protected; if the floor isn't enough to hit the cap, the loop stops
// and sets SizeCapUnmet so the caller can WARN that the cap is too low for
// the non-actions bulk.
const sizeCapActionFloorDays = 30

// shrinkToCap iteratively drops the oldest 30-day window of actions until
// the DB file is under the size cap OR the action-keep floor is reached.
// Capped at 12 iterations as a safety rail against an unexpectedly small
// max_db_size_mb (e.g. 1MB while individual sessions stay open).
func (p *Pruner) shrinkToCap(ctx context.Context, opts Options, res *Result) error {
	cap := int64(opts.MaxDBSizeMB) * 1024 * 1024
	floor := nowUTC().AddDate(0, 0, -sizeCapActionFloorDays)
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
			// No actions left to shave, yet still over cap → the bulk is
			// in other tables. Stop and flag rather than no-op silently.
			res.SizeCapUnmet = true
			return nil
		}
		min, err := time.Parse(time.RFC3339Nano, minTS.String)
		if err != nil {
			return fmt.Errorf("retention: parse min timestamp %q: %w", minTS.String, err)
		}
		// Floor guard: if the oldest action is already within the keep-
		// floor, we must not delete it — actions can't be shed further, so
		// the over-cap condition is owned by other tables.
		if !min.Before(floor) {
			res.SizeCapUnmet = true
			return nil
		}
		cutoff := min.Add(30 * 24 * time.Hour)
		if cutoff.After(floor) {
			cutoff = floor // clamp: never cross the keep-floor in one pass
		}
		actionsBefore := res.ActionsDeleted
		if err := p.deleteActionsOlder(ctx, cutoff.Format(time.RFC3339Nano), res); err != nil {
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
		// If a pass deleted no actions OR the file didn't shrink, actions
		// aren't the bulk — stop churning and flag rather than loop to the
		// floor pointlessly.
		if res.ActionsDeleted == actionsBefore || sizeOf(opts.DBPath) >= size {
			res.SizeCapUnmet = true
			return nil
		}
	}
	if sizeOf(opts.DBPath) > cap {
		res.SizeCapUnmet = true
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
