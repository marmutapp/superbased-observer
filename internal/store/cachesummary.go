package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Arc 4 P5c cache-detail aggregation. This file — NOT orgpush.go — owns the
// cache_events read (the privacy sentinel forbids the cache_* table names from
// ever appearing in orgpush.go; the push path composes this aggregate via a
// function call). The output is AGGREGATE ONLY: counts + tokens + cost delta by
// (day, model, kind). No prompt prefix, no raw cache scope (it is a hash), no
// path — the cache_* tables stay node-local except for this content-free
// aggregate, which ships only under the cache_detail tier.

// cacheSummaryWindowDays bounds the aggregate to the recent window; the server
// upserts by natural key, so re-pushing a window is idempotent.
const cacheSummaryWindowDays = 7

// SelectCacheSummaries aggregates the cache_events log into the P5c wire rows.
func (s *Store) SelectCacheSummaries(ctx context.Context) ([]orgcontract.CacheSummaryRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -cacheSummaryWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(timestamp, 1, 10) AS day, model, kind,
		       COUNT(*),
		       COALESCE(SUM(tokens_read), 0),
		       COALESCE(SUM(tokens_written), 0),
		       COALESCE(SUM(cost_delta_usd), 0)
		FROM cache_events
		WHERE timestamp >= ?
		GROUP BY day, model, kind
		ORDER BY day, model, kind`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectCacheSummaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.CacheSummaryRow{}
	for rows.Next() {
		var r orgcontract.CacheSummaryRow
		if err := rows.Scan(&r.Day, &r.Model, &r.Kind, &r.Events, &r.TokensRead, &r.TokensWritten, &r.CostDeltaUSD); err != nil {
			return nil, fmt.Errorf("store.SelectCacheSummaries: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectCacheSummaries: %w", err)
	}
	return out, nil
}
