package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Arc 4 P5h terminal-detail aggregation. This file — NOT orgpush.go — owns the
// terminal_run / terminal_commands / remote_audit reads (the privacy sentinel
// forbids those table names from ever appearing in orgpush.go, AND they carry
// dedicated end-to-end never-ships tests; the push path composes these
// aggregates via function calls). The output is AGGREGATE ONLY and content-free:
//
//   - SelectTerminalSummaries: per (day × tool × kind) counts of terminal runs,
//     the commands issued in them, how many runs ended, and how many exited
//     non-zero. NEVER a command, a project_root_hash, a correlation_token_hash,
//     a cmd_hash, or a source_session_id.
//   - SelectRemoteAuditSummaries: per (day × kind × decision × principal) event
//     counts. NEVER a session_id, a remote_addr, a route, or a detail string.
//
// Both ship ONLY under the terminal_detail tier (node opt-in on an individual
// node, admin-raised on a managed one via the DISTINCT extract.terminal
// authority). The terminal_* / remote_audit tables stay pinned entirely out of
// the wire otherwise — this aggregate under this explicit tier is the
// deliberate, reviewed reversal of that pin.

// terminalSummaryWindowDays bounds both aggregates to the recent window; the
// server upserts by natural key, so re-pushing a window is idempotent.
const terminalSummaryWindowDays = 7

// SelectTerminalSummaries aggregates terminal_run (joined to terminal_commands
// for the command count) into the P5h per (day, tool, kind) wire rows.
func (s *Store) SelectTerminalSummaries(ctx context.Context) ([]orgcontract.TerminalSummaryRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -terminalSummaryWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(r.launched_at, 1, 10) AS day, r.tool, r.kind,
		       COUNT(DISTINCT r.run_id),
		       COUNT(DISTINCT CASE WHEN r.ended_at IS NOT NULL THEN r.run_id END),
		       COUNT(DISTINCT CASE WHEN r.exit_code IS NOT NULL AND r.exit_code != 0 THEN r.run_id END),
		       COUNT(c.id)
		FROM terminal_run r
		LEFT JOIN terminal_commands c ON c.run_id = r.run_id
		WHERE r.launched_at >= ?
		GROUP BY day, r.tool, r.kind
		ORDER BY day, r.tool, r.kind`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectTerminalSummaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.TerminalSummaryRow{}
	for rows.Next() {
		var r orgcontract.TerminalSummaryRow
		if err := rows.Scan(&r.Day, &r.Tool, &r.Kind, &r.Runs, &r.Ended, &r.NonzeroExits, &r.Commands); err != nil {
			return nil, fmt.Errorf("store.SelectTerminalSummaries: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectTerminalSummaries: %w", err)
	}
	return out, nil
}

// SelectRemoteAuditSummaries aggregates remote_audit into the P5h per (day,
// kind, decision, principal) event-count wire rows.
func (s *Store) SelectRemoteAuditSummaries(ctx context.Context) ([]orgcontract.RemoteAuditSummaryRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -terminalSummaryWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(ts, 1, 10) AS day, kind, decision, principal, COUNT(*)
		FROM remote_audit
		WHERE ts >= ?
		GROUP BY day, kind, decision, principal
		ORDER BY day, kind, decision, principal`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectRemoteAuditSummaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.RemoteAuditSummaryRow{}
	for rows.Next() {
		var r orgcontract.RemoteAuditSummaryRow
		if err := rows.Scan(&r.Day, &r.Kind, &r.Decision, &r.Principal, &r.Events); err != nil {
			return nil, fmt.Errorf("store.SelectRemoteAuditSummaries: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectRemoteAuditSummaries: %w", err)
	}
	return out, nil
}
