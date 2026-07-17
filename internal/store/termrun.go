package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TerminalRun is one row of the NODE-LOCAL terminal-run identity table
// (terminal-product-exploitation plan §2.1a / §7, migration 064). It is the
// durable identity of a dashboard terminal launch, distinct from both the PTY
// handle and any agent session. Content-free by construction: the project root
// and OOB correlation nonce are carried as domain-separated hashes only
// (produced by internal/termrun). This struct — like RemoteAuditEvent — uses
// plain fields so the store stays decoupled from the pure package's enums.
//
// This is the ONE store seam for terminal_run / terminal_run_session; both
// tables are sentinel-pinned out of the org-push wire
// (tests/invariant/privacy_test.go). No paired orgserver migration exists.
type TerminalRun struct {
	// RunID is the opaque run identity minted at launch (termrun.NewRunID).
	RunID string
	// Tool is the target tool (e.g. "claude-code").
	Tool string
	// Kind is "handoff" or "fresh".
	Kind string
	// SourceSessionID is the handoff source session (kind=handoff only); it is
	// NEVER a correlated target. "" for a fresh launch.
	SourceSessionID string
	// ProjectRootHash is termrun.HashProjectRoot(root); never a raw path.
	ProjectRootHash string
	// CorrelationTokenHash is termrun.HashCorrelationToken(nonce); never the raw
	// nonce.
	CorrelationTokenHash string
	// LaunchedAt defaults to now (UTC) when zero.
	LaunchedAt time.Time
	// EndedAt is when the run's process exited; nil while running.
	EndedAt *time.Time
	// ExitCode is the run's exit code; nil while running.
	ExitCode *int
}

// TerminalCorrelation is one scored link from a run to an observed agent
// session (terminal_run_session). A run has zero-or-more; downstream links
// attach only once confidence clears termrun.MinLinkConfidence.
type TerminalCorrelation struct {
	RunID      string
	SessionID  string
	Confidence float64
	// Source is "oob" | "marker" | "heuristic".
	Source string
	// ObservedAt defaults to now (UTC) when zero.
	ObservedAt time.Time
}

// InsertTerminalRun appends one terminal-run identity row. The sole writer of
// terminal_run (one-owner rule). A duplicate run id is an error (the id is
// crypto-random, so a collision signals a caller bug).
func (s *Store) InsertTerminalRun(ctx context.Context, run TerminalRun) error {
	launched := run.LaunchedAt
	if launched.IsZero() {
		launched = time.Now().UTC()
	}
	var endedArg any
	if run.EndedAt != nil {
		endedArg = run.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO terminal_run
		   (run_id, tool, kind, source_session_id, project_root_hash,
		    correlation_token_hash, launched_at, ended_at, exit_code)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.RunID, run.Tool, run.Kind, run.SourceSessionID, run.ProjectRootHash,
		run.CorrelationTokenHash, launched.UTC().Format(time.RFC3339Nano),
		endedArg, nullIntPtr(run.ExitCode))
	if err != nil {
		return fmt.Errorf("store.InsertTerminalRun: %w", err)
	}
	return nil
}

// EndTerminalRun records a run's exit (ended_at + exit_code). Idempotent-safe:
// it overwrites whatever was there, so a re-reported exit is harmless.
func (s *Store) EndTerminalRun(ctx context.Context, runID string, endedAt time.Time, exitCode int) error {
	if endedAt.IsZero() {
		endedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE terminal_run SET ended_at = ?, exit_code = ? WHERE run_id = ?`,
		endedAt.UTC().Format(time.RFC3339Nano), exitCode, runID)
	if err != nil {
		return fmt.Errorf("store.EndTerminalRun: %w", err)
	}
	return nil
}

// UpsertCorrelation records a scored run→session correlation, keeping the
// HIGHEST-confidence observation per (run_id, session_id): a later, weaker
// heuristic never downgrades an established OOB link. The scoring itself is
// owned by internal/termrun; this seam only persists the winner.
func (s *Store) UpsertCorrelation(ctx context.Context, c TerminalCorrelation) error {
	observed := c.ObservedAt
	if observed.IsZero() {
		observed = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO terminal_run_session (run_id, session_id, confidence, source, observed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(run_id, session_id) DO UPDATE SET
		   confidence  = excluded.confidence,
		   source      = excluded.source,
		   observed_at = excluded.observed_at
		 WHERE excluded.confidence > terminal_run_session.confidence`,
		c.RunID, c.SessionID, c.Confidence, c.Source,
		observed.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("store.UpsertCorrelation: %w", err)
	}
	return nil
}

// LoadTerminalRun returns a run by id, or ok=false when absent.
func (s *Store) LoadTerminalRun(ctx context.Context, runID string) (TerminalRun, bool, error) {
	var run TerminalRun
	var launched string
	var ended sql.NullString
	var exit sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, tool, kind, source_session_id, project_root_hash,
		        correlation_token_hash, launched_at, ended_at, exit_code
		   FROM terminal_run WHERE run_id = ?`, runID).
		Scan(&run.RunID, &run.Tool, &run.Kind, &run.SourceSessionID, &run.ProjectRootHash,
			&run.CorrelationTokenHash, &launched, &ended, &exit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TerminalRun{}, false, nil
		}
		return TerminalRun{}, false, fmt.Errorf("store.LoadTerminalRun: %w", err)
	}
	run.LaunchedAt, _ = time.Parse(time.RFC3339Nano, launched)
	if ended.Valid {
		if t, perr := time.Parse(time.RFC3339Nano, ended.String); perr == nil {
			run.EndedAt = &t
		}
	}
	if exit.Valid {
		v := int(exit.Int64)
		run.ExitCode = &v
	}
	return run, true, nil
}

// TerminalRunSummary is one row of the read-only terminal-run history view
// (dashboard-management-surface plan §E). Metadata only, like TerminalRun: no
// raw path (the project root is hashed on the base row and not surfaced here)
// and no command text. It folds in the strongest correlated agent session (if
// any) and the observed command-boundary count, so the history view can link a
// run to the session it produced without an N+1 fan-out.
type TerminalRunSummary struct {
	RunID          string
	Tool           string
	Kind           string
	LaunchedAt     time.Time
	EndedAt        *time.Time
	ExitCode       *int
	BestSessionID  string  // strongest correlation; "" when none observed yet
	BestConfidence float64 // 0 when BestSessionID is ""
	CommandCount   int     // rows in terminal_commands for this run
}

// ListTerminalRuns returns the most-recent terminal runs (newest first), each
// folded with its strongest correlated session + observed command-boundary
// count. Read-only; the sole reader seam for the history view. limit is clamped
// by the caller. Both tables are NODE-LOCAL (never on the org-push wire).
func (s *Store) ListTerminalRuns(ctx context.Context, limit int) ([]TerminalRunSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.run_id, r.tool, r.kind, r.launched_at, r.ended_at, r.exit_code,
		        (SELECT trs.session_id FROM terminal_run_session trs
		           WHERE trs.run_id = r.run_id
		           ORDER BY trs.confidence DESC, trs.id ASC LIMIT 1),
		        (SELECT trs.confidence FROM terminal_run_session trs
		           WHERE trs.run_id = r.run_id
		           ORDER BY trs.confidence DESC, trs.id ASC LIMIT 1),
		        (SELECT COUNT(*) FROM terminal_commands tc WHERE tc.run_id = r.run_id)
		   FROM terminal_run r
		  ORDER BY r.launched_at DESC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store.ListTerminalRuns: %w", err)
	}
	defer rows.Close()
	var out []TerminalRunSummary
	for rows.Next() {
		var (
			sum      TerminalRunSummary
			launched string
			ended    sql.NullString
			exit     sql.NullInt64
			bestSess sql.NullString
			bestConf sql.NullFloat64
		)
		if scanErr := rows.Scan(&sum.RunID, &sum.Tool, &sum.Kind, &launched, &ended, &exit,
			&bestSess, &bestConf, &sum.CommandCount); scanErr != nil {
			return nil, fmt.Errorf("store.ListTerminalRuns scan: %w", scanErr)
		}
		sum.LaunchedAt, _ = time.Parse(time.RFC3339Nano, launched)
		if ended.Valid {
			if t, perr := time.Parse(time.RFC3339Nano, ended.String); perr == nil {
				sum.EndedAt = &t
			}
		}
		if exit.Valid {
			v := int(exit.Int64)
			sum.ExitCode = &v
		}
		if bestSess.Valid {
			sum.BestSessionID = bestSess.String
			if bestConf.Valid {
				sum.BestConfidence = bestConf.Float64
			}
		}
		out = append(out, sum)
	}
	return out, rows.Err()
}

// LoadCorrelations returns a run's correlations, strongest first.
func (s *Store) LoadCorrelations(ctx context.Context, runID string) ([]TerminalCorrelation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, session_id, confidence, source, observed_at
		   FROM terminal_run_session WHERE run_id = ?
		  ORDER BY confidence DESC, id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("store.LoadCorrelations: %w", err)
	}
	defer rows.Close()
	var out []TerminalCorrelation
	for rows.Next() {
		var c TerminalCorrelation
		var observed string
		if err := rows.Scan(&c.RunID, &c.SessionID, &c.Confidence, &c.Source, &observed); err != nil {
			return nil, fmt.Errorf("store.LoadCorrelations scan: %w", err)
		}
		c.ObservedAt, _ = time.Parse(time.RFC3339Nano, observed)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveRunForSession returns the run most confidently correlated to a
// session, for the reverse lookup cost/status linking needs (plan §2.1a). It
// returns ok=false when no correlation exists; the caller applies the
// termrun.MinLinkConfidence gate on the returned confidence before attaching
// links.
func (s *Store) ResolveRunForSession(ctx context.Context, sessionID string) (runID string, confidence float64, ok bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, confidence FROM terminal_run_session
		  WHERE session_id = ?
		  ORDER BY confidence DESC, id ASC LIMIT 1`, sessionID)
	if serr := row.Scan(&runID, &confidence); serr != nil {
		if errors.Is(serr, sql.ErrNoRows) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("store.ResolveRunForSession: %w", serr)
	}
	return runID, confidence, true, nil
}
