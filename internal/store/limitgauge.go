package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Arc 4 P5e predictions / limit-gauge aggregation. This file — NOT orgpush.go —
// owns the limit_snapshots read (the privacy sentinel forbids that table name
// from ever appearing in orgpush.go; the push path composes this aggregate via
// a function call, exactly like SelectRoutingSummaries). The output is AGGREGATE
// ONLY: per (day, provider) rate-limit utilization stats — no scope_hash, no
// session id, no raw headers.

// limitGaugeWindowDays bounds the aggregate to the recent window; the server
// upserts by natural key, so re-pushing a window is idempotent.
const limitGaugeWindowDays = 7

// SelectLimitGauges aggregates the limit_snapshots log into the P5e wire rows.
// observed_at is unix seconds, so the day bucket is computed with the SQLite
// unixepoch modifier.
func (s *Store) SelectLimitGauges(ctx context.Context) ([]orgcontract.LimitGaugeRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -limitGaugeWindowDays).Unix()
	rows, err := s.db.QueryContext(ctx, `
		SELECT strftime('%Y-%m-%d', observed_at, 'unixepoch') AS day, provider,
		       COUNT(*),
		       COALESCE(MAX(window_5h_util), 0), COALESCE(AVG(window_5h_util), 0),
		       COALESCE(MAX(window_7d_util), 0), COALESCE(AVG(window_7d_util), 0)
		FROM limit_snapshots
		WHERE observed_at >= ?
		GROUP BY day, provider
		ORDER BY day, provider`, since)
	if err != nil {
		return nil, fmt.Errorf("store.SelectLimitGauges: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.LimitGaugeRow{}
	for rows.Next() {
		var r orgcontract.LimitGaugeRow
		if err := rows.Scan(&r.Day, &r.Provider, &r.Snapshots,
			&r.Max5hUtil, &r.Avg5hUtil, &r.Max7dUtil, &r.Avg7dUtil); err != nil {
			return nil, fmt.Errorf("store.SelectLimitGauges: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectLimitGauges: %w", err)
	}
	return out, nil
}
