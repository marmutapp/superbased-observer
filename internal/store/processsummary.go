package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Arc 4 P5g process-detail aggregation. This file — NOT orgpush.go — owns the
// process_runs read (the privacy sentinel forbids the process_* table names
// from ever appearing in orgpush.go; the push path composes this aggregate via
// a function call). The output is AGGREGATE ONLY: per (day × tool) counts of
// process runs, how many exited, how many exited non-zero, and the summed
// duration. It NEVER carries an executable path, argv, cwd, network body, or
// any of the process/network domain-separated hashes — the process_* tables
// stay node-local except for this content-free aggregate, which ships only
// under the process_detail tier (node opt-in / admin-raised via the DISTINCT
// extract.process authority). Per the process/network arc, plaintext bodies are
// node-local only; this aggregate ships no body at all.

// processSummaryWindowDays bounds the aggregate to the recent window; the server
// upserts by natural key, so re-pushing a window is idempotent.
const processSummaryWindowDays = 7

// SelectProcessSummaries aggregates the process_runs log into the P5g wire rows.
func (s *Store) SelectProcessSummaries(ctx context.Context) ([]orgcontract.ProcessSummaryRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -processSummaryWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(started_at, 1, 10) AS day, COALESCE(tool, ''),
		       COUNT(*),
		       SUM(CASE WHEN exited_at IS NOT NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN exit_code IS NOT NULL AND exit_code != 0 THEN 1 ELSE 0 END),
		       COALESCE(SUM(duration_ms), 0)
		FROM process_runs
		WHERE started_at >= ?
		GROUP BY day, tool
		ORDER BY day, tool`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectProcessSummaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.ProcessSummaryRow{}
	for rows.Next() {
		var r orgcontract.ProcessSummaryRow
		if err := rows.Scan(&r.Day, &r.Tool, &r.Runs, &r.Exited, &r.NonzeroExits, &r.DurationMsSum); err != nil {
			return nil, fmt.Errorf("store.SelectProcessSummaries: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectProcessSummaries: %w", err)
	}
	return out, nil
}
