package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// W2.2 session-scoped process rows (org-parity plan §4 W2.2,
// docs/plans/org-parity-full-depth-plan-2026-08-24.md). This file — NOT
// orgpush.go — owns the process_runs / process_events read (the privacy
// sentinel forbids the process_* table names from ever appearing in
// orgpush.go; the push path composes these rows via a function call, exactly
// like SelectProcessSummaries in processsummary.go).
//
// Unlike processsummary.go's Arc 4 aggregate, this is a RAW per-run read: it
// ships under admin_managed / full_content (ShareOptions.shipsRawContent())
// so the org session detail can render the same process tree the node's own
// System tab shows, scoped to one session. It still never reads
// process_network_bodies and never reads process_events.target*/details_json
// — only a per-run COUNT(*) of events crosses.

// sessionProcessWindowDays bounds the read to sessions active in the recent
// window; the server upserts by natural key (org_id, session_id, run_key), so
// re-pushing a window is idempotent.
const sessionProcessWindowDays = 7

// sessionProcessRunCap is the per-session cap on how many process runs this
// query returns. Sessions that spawn far more than this (a runaway shell
// loop, a noisy watch script) would otherwise dominate the push payload for
// one session; 200 comfortably covers ordinary interactive + sub-agent
// process fan-out while bounding worst case. The cap keeps the MOST RECENT
// runs (by started_at) per session, enforced IN SQL via ROW_NUMBER() OVER
// (PARTITION BY session_id ORDER BY started_at DESC) filtered rn <= this cap
// — exactly like sessionNetworkEventCap in networkorgrows.go — so the
// discarded runs never reach the per-run event count or the Go scan.
const sessionProcessRunCap = 200

// sessionProcessRowsQuery is the SelectSessionProcessRows read, held as a
// const so processorgrows_test.go can assert its EXPLAIN QUERY PLAN against
// the exact string production runs (a copy in the test would silently drift
// away from the query whose plan it claims to pin).
//
// The per-session cap is enforced IN SQL via ROW_NUMBER() rather than in Go,
// and the per-run event count is a CORRELATED subquery rather than a
// pre-aggregated LEFT JOIN. Both changes exist to stop work the cap was going
// to throw away:
//
//   - the count now fires only for runs that survive rn <= cap, as a covering-
//     index seek on idx_process_events_run (migration 090). The previous
//     LEFT JOIN (SELECT ... GROUP BY process_run_id) materialized an aggregate
//     over the ENTIRE process_events table on every push tick — no index led
//     with process_run_id, so it was a full scan plus a temp-b-tree GROUP BY
//     whose cost was independent of how few runs the window held.
//   - idx_process_runs_session_started_desc (migration 090) serves the
//     window's PARTITION BY session_id ORDER BY started_at DESC with no sort.
//     idx_process_runs_session is ASC on both columns and cannot: the required
//     order is MIXED, and reverse-scanning it yields session_id DESC. Without
//     the DESC index the plan externally sorted the whole window before the
//     cap could discard from it.
//
// A temp b-tree remains on the outer ORDER BY (SQLite cannot carry sort order
// out of a co-routine CTE), but it sorts only the post-cap survivors —
// bounded by cap x sessions, not by the window. Together these were the
// 11.9%-of-daemon-CPU finding of the 2026-08-26 audit.
//
// Selection is arithmetically identical to the Go-side cap it replaces: the
// sessionProcessRunCap most recent runs per session by started_at DESC,
// output ordered session_id then started_at DESC. Placeholders are (since,
// cap) in that order.
const sessionProcessRowsQuery = `
		WITH ranked AS (
			SELECT pr.id, pr.session_id, pr.process_key, pr.parent_process_key, pr.pid, pr.ppid,
			       pr.tool, pr.action_id, pr.turn_index,
			       pr.exe_path, pr.exe_basename, pr.exe_hash, pr.cwd, pr.argv_preview, pr.argv_argc,
			       pr.attribution_source, pr.attribution_confidence,
			       pr.started_at, pr.exited_at, pr.exit_code, pr.exit_signal, pr.duration_ms,
			       pr.cpu_user_ms, pr.cpu_system_ms, pr.max_rss_bytes, pr.read_bytes, pr.write_bytes,
			       ROW_NUMBER() OVER (PARTITION BY pr.session_id ORDER BY pr.started_at DESC) AS rn
			FROM process_runs pr
			WHERE pr.session_id IS NOT NULL AND pr.session_id != '' AND pr.started_at >= ?
		)
		SELECT r.session_id, r.process_key, r.parent_process_key, r.pid, r.ppid,
		       r.tool, r.action_id, r.turn_index,
		       r.exe_path, r.exe_basename, r.exe_hash, r.cwd, r.argv_preview, r.argv_argc,
		       r.attribution_source, r.attribution_confidence,
		       r.started_at, r.exited_at, r.exit_code, r.exit_signal, r.duration_ms,
		       r.cpu_user_ms, r.cpu_system_ms, r.max_rss_bytes, r.read_bytes, r.write_bytes,
		       (SELECT COUNT(*) FROM process_events pe WHERE pe.process_run_id = r.id)
		FROM ranked r
		WHERE r.rn <= ?
		ORDER BY r.session_id, r.started_at DESC`

// SelectSessionProcessRows reads process_runs (+ a process_events count per
// run) for every session with process activity in the trailing window,
// capped to the sessionProcessRunCap most recent runs per session.
func (s *Store) SelectSessionProcessRows(ctx context.Context) ([]orgcontract.SessionProcessRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -sessionProcessWindowDays)
	rows, err := s.db.QueryContext(ctx, sessionProcessRowsQuery, timestamp(since), sessionProcessRunCap)
	if err != nil {
		return nil, fmt.Errorf("store.SelectSessionProcessRows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.SessionProcessRow{}
	for rows.Next() {
		var (
			r                      orgcontract.SessionProcessRow
			parentKey              sql.NullString
			ppid                   sql.NullInt64
			tool                   sql.NullString
			actionID, turnIndex    sql.NullInt64
			exePath, exeBasename   sql.NullString
			exeHash, cwd           sql.NullString
			argvPreview            sql.NullString
			argvArgc               sql.NullInt64
			exitedAt               sql.NullString
			exitCode, exitSignal   sql.NullInt64
			durationMs             sql.NullInt64
			cpuUserMs, cpuSystemMs sql.NullInt64
			maxRSSBytes            sql.NullInt64
			readBytes, writeBytes  sql.NullInt64
		)
		if err := rows.Scan(
			&r.SessionID, &r.RunKey, &parentKey, &r.PID, &ppid,
			&tool, &actionID, &turnIndex,
			&exePath, &exeBasename, &exeHash, &cwd, &argvPreview, &argvArgc,
			&r.AttributionSource, &r.AttributionConfidence,
			&r.StartedAt, &exitedAt, &exitCode, &exitSignal, &durationMs,
			&cpuUserMs, &cpuSystemMs, &maxRSSBytes, &readBytes, &writeBytes,
			&r.EventCount,
		); err != nil {
			return nil, fmt.Errorf("store.SelectSessionProcessRows: scan: %w", err)
		}

		r.ParentRunKey = parentKey.String
		r.PPID = ppid.Int64
		r.Tool = tool.String
		r.ActionID = actionID.Int64
		r.TurnIndex = turnIndex.Int64
		r.ExePath = exePath.String
		r.ExeBasename = exeBasename.String
		r.ExeHash = exeHash.String
		r.CWD = cwd.String
		r.ArgvPreview = argvPreview.String
		r.ArgvArgc = argvArgc.Int64
		r.EndedAt = exitedAt.String
		r.Exited = exitedAt.Valid
		r.ExitCode = exitCode.Int64
		r.ExitSignal = exitSignal.Int64
		r.DurationMs = durationMs.Int64
		r.CPUUserMs = cpuUserMs.Int64
		r.CPUSystemMs = cpuSystemMs.Int64
		r.MaxRSSBytes = maxRSSBytes.Int64
		r.ReadBytes = readBytes.Int64
		r.WriteBytes = writeBytes.Int64

		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectSessionProcessRows: %w", err)
	}
	return out, nil
}
