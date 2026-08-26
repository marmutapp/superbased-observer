package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// Arc 4 P5d routing-detail aggregation. This file — NOT orgpush.go — owns the
// router_decisions read (the privacy sentinel forbids that table name from ever
// appearing in orgpush.go; the push path composes this aggregate via a function
// call, exactly like SelectRoutingSummaries). The output is AGGREGATE ONLY, but
// unlike the tier-only routing summary it keeps the ACTUAL model ids — the
// model-id-bearing per-decision detail the managed extraction tier wants. No
// content, no session id, no timestamp beyond the day bucket.

// routingDetailWindowDays bounds the aggregate to the recent window; the server
// upserts by natural key, so re-pushing a window is idempotent.
const routingDetailWindowDays = 7

// SelectRoutingDetail aggregates the decision log into the P5d wire rows.
func (s *Store) SelectRoutingDetail(ctx context.Context) ([]orgcontract.RoutingDetailRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -routingDetailWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(ts, 1, 10) AS day, original_model, selected_model, turn_kind, mode,
		       COUNT(*), COALESCE(SUM(applied), 0),
		       COALESCE(SUM(est_savings_usd), 0), COALESCE(SUM(cache_forfeit_usd), 0)
		FROM router_decisions
		WHERE ts >= ?
		GROUP BY day, original_model, selected_model, turn_kind, mode
		ORDER BY day, original_model, selected_model, turn_kind, mode`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectRoutingDetail: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.RoutingDetailRow{}
	for rows.Next() {
		var r orgcontract.RoutingDetailRow
		if err := rows.Scan(&r.Day, &r.OriginalModel, &r.SelectedModel, &r.TurnKind, &r.Mode,
			&r.Decisions, &r.Applied, &r.EstSavingsUSD, &r.CacheForfeitUSD); err != nil {
			return nil, fmt.Errorf("store.SelectRoutingDetail: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectRoutingDetail: %w", err)
	}
	return out, nil
}
