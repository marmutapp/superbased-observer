package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// W2.1 session-scoped cache-event summary (see
// docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W2.1"). This file —
// NOT orgpush.go — owns the cache_events read (the privacy sentinel forbids
// the cache_* table names from ever appearing in orgpush.go; the push path
// composes this via a function call, the same discipline as
// cachesummary.go::SelectCacheSummaries). The output is content-free: counts +
// tokens by (session_id, model, tier, kind, cause, zero_usage) — no prompt
// prefix, no cache scope, no prefix hash, no diagnostic detail JSON.

// sessionCacheWindowDays bounds the recompute to sessions with at least one
// cache event in the trailing window; the server upserts by natural key, so
// re-pushing a window is idempotent (same posture as SelectCacheSummaries).
const sessionCacheWindowDays = 7

// SelectSessionCacheSummaries aggregates the cache_events log into the W2.1
// session-scoped wire rows, for any session that had a cache event in the
// trailing sessionCacheWindowDays window.
func (s *Store) SelectSessionCacheSummaries(ctx context.Context) ([]orgcontract.SessionCacheRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -sessionCacheWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, model, tier, kind, COALESCE(cause, ''),
		       (tokens_read = 0 AND tokens_written = 0) AS zero_usage,
		       COUNT(*),
		       COALESCE(SUM(tokens_read), 0),
		       COALESCE(SUM(tokens_written), 0)
		FROM cache_events
		WHERE timestamp >= ?
		GROUP BY session_id, model, tier, kind, cause, zero_usage
		ORDER BY session_id, model, kind, cause`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectSessionCacheSummaries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.SessionCacheRow{}
	for rows.Next() {
		var r orgcontract.SessionCacheRow
		var zeroUsage int
		if err := rows.Scan(&r.SessionID, &r.Model, &r.Tier, &r.Kind, &r.Cause,
			&zeroUsage, &r.Events, &r.TokensRead, &r.TokensWritten); err != nil {
			return nil, fmt.Errorf("store.SelectSessionCacheSummaries: scan: %w", err)
		}
		r.ZeroUsage = zeroUsage != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectSessionCacheSummaries: %w", err)
	}
	return out, nil
}
