package copilotanalytics

import (
	"context"
	"database/sql"
	"fmt"
)

// CostSummary is the SIBLING cost rollup (instance §5.2, the "merge inversion").
// Copilot cost is NOT per-turn-priced and does NOT enter rollup/cost.go::spendCTE
// — there is nothing to dedup against agent_push (a seat is paid regardless of
// enrollment). Instead this read converts Copilot's two cost feeds to USD,
// keeping them structurally distinct so neither is mis-summed:
//
//   - SeatSnapshot — the LATEST seat-breakdown (a MONTHLY subscription; seats ×
//     per-seat price). Point-in-time — NOT additive across days.
//   - OverageByDay — the per-day enhanced-billing metered $ (AI-Credits /
//     premium overage). Already USD; additive.
//
// Engagement count metrics are NEVER read here — they carry no cost and are
// surfaced separately as Copilot-exclusive metrics.
type CostSummary struct {
	OrgID           string
	PerSeatPriceUSD float64
	SeatSnapshot    SeatSnapshot
	OverageByDay    []DayUSD
	TotalOverageUSD float64
}

// SeatSnapshot is the latest seat-breakdown total as a monthly subscription cost.
type SeatSnapshot struct {
	Day        string  // the snapshot day (YYYY-MM-DD)
	Seats      float64 // seat_breakdown.total
	MonthlyUSD float64 // Seats × PerSeatPriceUSD (a monthly fee, not per-day)
}

// DayUSD is one day's metered overage in USD.
type DayUSD struct {
	Day string
	USD float64
}

// LoadCostSummary computes the sibling cost rollup for an org (or all orgs when
// orgID is ""). perSeatPriceUSD is the plan's per-seat monthly price ($19
// Business / $39 Enterprise) — a config, since the seats API returns counts only.
//
// Unit invariant (the cross-vendor unit trap): only `seats` and `usd` rows feed
// cost; `count` rows (engagement) are excluded by metric, and the two cost feeds
// are returned in distinct fields so a caller can never sum seats with usd.
func LoadCostSummary(ctx context.Context, db *sql.DB, orgID string, perSeatPriceUSD float64) (CostSummary, error) {
	out := CostSummary{OrgID: orgID, PerSeatPriceUSD: perSeatPriceUSD}

	orgFilter, args := orgPredicate(orgID)

	// Latest seat-breakdown total (point-in-time monthly subscription).
	seatRow := db.QueryRowContext(ctx,
		`SELECT day, value FROM copilot_analytics_daily
		   WHERE surface = ? AND metric = ? AND unit = ?`+orgFilter+`
		   ORDER BY day DESC LIMIT 1`,
		append([]any{string(SurfaceSeats), MetricSeatsTotal, string(UnitSeats)}, args...)...)
	var snapDay string
	var seats float64
	switch err := seatRow.Scan(&snapDay, &seats); err {
	case nil:
		out.SeatSnapshot = SeatSnapshot{Day: snapDay, Seats: seats, MonthlyUSD: seats * perSeatPriceUSD}
	case sql.ErrNoRows:
		// No seat data yet — leave the zero snapshot.
	default:
		return out, fmt.Errorf("copilotanalytics: load seat snapshot: %w", err)
	}

	// Per-day metered overage (additive USD).
	rows, err := db.QueryContext(ctx,
		`SELECT day, SUM(value) FROM copilot_analytics_daily
		   WHERE surface = ? AND metric = ? AND unit = ?`+orgFilter+`
		   GROUP BY day ORDER BY day`,
		append([]any{string(SurfaceBilling), MetricCost, string(UnitUSD)}, args...)...)
	if err != nil {
		return out, fmt.Errorf("copilotanalytics: load overage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d DayUSD
		if err := rows.Scan(&d.Day, &d.USD); err != nil {
			return out, fmt.Errorf("copilotanalytics: scan overage: %w", err)
		}
		out.OverageByDay = append(out.OverageByDay, d)
		out.TotalOverageUSD += d.USD
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("copilotanalytics: iterate overage: %w", err)
	}
	return out, nil
}

// orgPredicate returns an optional " AND org_id = ?" filter + its args.
func orgPredicate(orgID string) (string, []any) {
	if orgID == "" {
		return "", nil
	}
	return " AND org_id = ?", []any{orgID}
}
