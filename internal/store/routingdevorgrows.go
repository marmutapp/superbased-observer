package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// routingdevorgrows.go is the W2.3 org-wire seam for the per-developer
// routing detail feature. It OWNS the router_decisions read for the
// enterprise per-dev path — routingdetail.go already owns this table for
// the teams-tier RoutingDetailRow aggregate, and both files are allowed to
// read the same table (module-boundary discipline is about ONE OWNER PER
// WRITE SEAM / one function-call composition point into orgpush.go, not
// "only one reader in the whole binary"). orgpush.go never touches
// router_decisions directly either way.
//
// The aggregation is structurally identical to SelectRoutingDetail (same
// GROUP BY dimensions, same COALESCE/SUM math) — this file exists as a
// separate, deliberately non-shared query because the two wire rows ship
// under different gates (share.RoutingDetail vs shipsRawContent()) and the
// task's design decision was to keep the existing teams surface completely
// untouched rather than parameterize it.

// routingDevWindowDays mirrors routingDetailWindowDays: recompute + upsert
// over a short trailing window, idempotent on the server's natural key.
const routingDevWindowDays = 7

// SelectRoutingDevRows aggregates router_decisions into the W2.3
// per-developer wire rows for the trailing window.
func (s *Store) SelectRoutingDevRows(ctx context.Context) ([]orgcontract.RoutingDevRow, error) {
	since := time.Now().UTC().AddDate(0, 0, -routingDevWindowDays)
	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(ts, 1, 10) AS day, original_model, selected_model, turn_kind, mode,
		       COUNT(*), COALESCE(SUM(applied), 0),
		       COALESCE(SUM(est_savings_usd), 0), COALESCE(SUM(cache_forfeit_usd), 0)
		FROM router_decisions
		WHERE ts >= ?
		GROUP BY day, original_model, selected_model, turn_kind, mode
		ORDER BY day, original_model, selected_model, turn_kind, mode`, timestamp(since))
	if err != nil {
		return nil, fmt.Errorf("store.SelectRoutingDevRows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []orgcontract.RoutingDevRow{}
	for rows.Next() {
		var r orgcontract.RoutingDevRow
		if err := rows.Scan(&r.Day, &r.OriginalModel, &r.SelectedModel, &r.TurnKind, &r.Mode,
			&r.Decisions, &r.Switched, &r.EstSavingsUSD, &r.CacheForfeitUSD); err != nil {
			return nil, fmt.Errorf("store.SelectRoutingDevRows: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.SelectRoutingDevRows: %w", err)
	}
	return out, nil
}
