package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// launchseed.go — SQL seams for the launch_seeds table (migration 086).
//
// Ownership (CLAUDE.md rule 4): a pending row is written by the launcher at
// spawn and consumed (deleted) by the daemon sweep's atomic claim;
// unconsumed rows past the match window are expired by the sweep. The
// matching rule itself is pure — processobs.MatchLaunchSeeds — this file is
// I/O only.

// InsertLaunchSeed records a launched child pid so the daemon sweep can bind
// it to the session the watcher will ingest. Upsert on pid: a recycled pid
// from an earlier, already-retracted launch must not collide.
func (s *Store) InsertLaunchSeed(ctx context.Context, seed processobs.LaunchSeed) error {
	if seed.PID <= 0 {
		return errors.New("store.InsertLaunchSeed: PID must be > 0")
	}
	if seed.Tool == "" {
		return errors.New("store.InsertLaunchSeed: Tool required")
	}
	now := timestamp(time.Now().UTC())
	started := timestamp(seed.StartedAt)
	if seed.StartedAt.IsZero() {
		started = now
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO launch_seeds (pid, tool, cwd, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(pid) DO UPDATE SET
		  tool       = excluded.tool,
		  cwd        = excluded.cwd,
		  started_at = excluded.started_at,
		  updated_at = excluded.updated_at`,
		seed.PID, seed.Tool, seed.CWD, started, now)
	if err != nil {
		return fmt.Errorf("store.InsertLaunchSeed: %w", err)
	}
	return nil
}

// PendingLaunchSeeds returns unconsumed seeds whose started_at falls inside
// the match window (younger than maxAge). Older unconsumed seeds are the
// expiry pass's job, never this query's concern.
func (s *Store) PendingLaunchSeeds(ctx context.Context, maxAge time.Duration) ([]processobs.LaunchSeed, error) {
	since := timestamp(time.Now().UTC().Add(-maxAge))
	rows, err := s.db.QueryContext(ctx, `
		SELECT pid, tool, cwd, started_at
		  FROM launch_seeds
		 WHERE started_at >= ?`,
		since)
	if err != nil {
		return nil, fmt.Errorf("store.PendingLaunchSeeds: %w", err)
	}
	defer rows.Close()
	var out []processobs.LaunchSeed
	for rows.Next() {
		var seed processobs.LaunchSeed
		var started string
		if err := rows.Scan(&seed.PID, &seed.Tool, &seed.CWD, &started); err != nil {
			return nil, fmt.Errorf("store.PendingLaunchSeeds: scan: %w", err)
		}
		seed.StartedAt = parseStamp(started)
		out = append(out, seed)
	}
	return out, rows.Err()
}

// ClaimLaunchSeed atomically consumes a pending seed: the row is deleted and
// true reports that THIS caller won the claim (a row already retracted by the
// launcher's exit path reports false). The winner writes the
// session_pid_bridge row; the bridge row's later lifecycle is the standard
// pidbridge prune (same retention posture as hook-written rows), so no
// consumed-row bookkeeping survives here.
func (s *Store) ClaimLaunchSeed(ctx context.Context, pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM launch_seeds WHERE pid = ?`, pid)
	if err != nil {
		return false, fmt.Errorf("store.ClaimLaunchSeed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store.ClaimLaunchSeed: rows: %w", err)
	}
	return n > 0, nil
}

// ExpireStaleLaunchSeeds deletes unconsumed seeds older than olderThan —
// launches that died with their launcher (SIGKILL) or never produced a
// session inside the match window. Returns the number removed.
func (s *Store) ExpireStaleLaunchSeeds(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := timestamp(time.Now().UTC().Add(-olderThan))
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM launch_seeds WHERE started_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("store.ExpireStaleLaunchSeeds: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store.ExpireStaleLaunchSeeds: rows: %w", err)
	}
	return int(n), nil
}

// RecentSessionRefsForLaunchMatch loads the session candidates the launch-seed
// matcher may pair against: sessions started within windowMinutes, projected
// onto the same shape the cross-OS correlator uses.
func (s *Store) RecentSessionRefsForLaunchMatch(ctx context.Context, windowMinutes int) ([]processobs.CrossOSSessionRef, error) {
	if windowMinutes <= 0 {
		windowMinutes = 60
	}
	since := timestamp(time.Now().UTC().Add(-time.Duration(windowMinutes) * time.Minute))
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.tool, COALESCE(p.root_path, ''), s.started_at
		  FROM sessions s JOIN projects p ON s.project_id = p.id
		 WHERE s.started_at >= ?`,
		since)
	if err != nil {
		return nil, fmt.Errorf("store.RecentSessionRefsForLaunchMatch: %w", err)
	}
	defer rows.Close()
	var out []processobs.CrossOSSessionRef
	for rows.Next() {
		var ref processobs.CrossOSSessionRef
		var started string
		if err := rows.Scan(&ref.SessionID, &ref.Tool, &ref.ProjectRoot, &started); err != nil {
			return nil, fmt.Errorf("store.RecentSessionRefsForLaunchMatch: scan: %w", err)
		}
		ref.StartedAt = parseStamp(started)
		out = append(out, ref)
	}
	return out, rows.Err()
}
