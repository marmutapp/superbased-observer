package store

import (
	"context"
	"fmt"
	"time"
)

// UserSpend returns the summed obs_spans.cost_usd for one end-user since the
// given instant, joining spans to their trace's app-shared user identity
// (obs_traces.user — populated from the OTel enduser.id / the admission `user`
// field at ingest). It is the per-end-user spend basis for the admission budget
// gate (docs/guardrails.md, org-budget plan §1.2): in the org-hosted-app model
// the budgeted subject is an end-user of the hosted app, not an enrolled
// developer — so spend is attributed through obs_traces.user, NOT the
// coding-assistant sessions/api_turns/token_usage tables. An empty user returns
// 0 (an anonymous request has no per-user budget). Node-local, read-only; the
// obs_* tables never enter the agent→org push wire.
func (s *Store) UserSpend(ctx context.Context, user string, since time.Time) (float64, error) {
	if user == "" {
		return 0, nil
	}
	var total float64
	err := s.db.QueryRowContext(ctx, `
SELECT COALESCE(SUM(sp.cost_usd),0)
  FROM obs_spans sp
  JOIN obs_traces t ON t.trace_id = sp.trace_id
 WHERE t.user = ? AND sp.started_at >= ?`, user, ts(since)).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("obs/store.UserSpend: %w", err)
	}
	return total, nil
}

// UserSpendRow is one end-user's spend across the three budget windows.
type UserSpendRow struct {
	User     string  `json:"user"`
	FiveHour float64 `json:"five_hour"`
	Weekly   float64 `json:"weekly"`
	Monthly  float64 `json:"monthly"`
}

// TopUserSpend returns the top-`limit` end-users by calendar-month spend, each
// with their rolling-5-hour and rolling-7-day totals — the data behind
// `observer obs admission budget status` and the dashboard budget card. Windows
// are computed relative to `now` (UTC). Unattributed spend (empty user) is
// excluded. Node-local, read-only.
func (s *Store) TopUserSpend(ctx context.Context, now time.Time, limit int) ([]UserSpendRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	fiveHour := ts(now.Add(-5 * time.Hour))
	weekly := ts(now.AddDate(0, 0, -7))
	monthly := ts(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	rows, err := s.db.QueryContext(ctx, `
SELECT t.user,
       COALESCE(SUM(CASE WHEN sp.started_at >= ? THEN sp.cost_usd END),0),
       COALESCE(SUM(CASE WHEN sp.started_at >= ? THEN sp.cost_usd END),0),
       COALESCE(SUM(sp.cost_usd),0)
  FROM obs_spans sp
  JOIN obs_traces t ON t.trace_id = sp.trace_id
 WHERE t.user IS NOT NULL AND t.user != '' AND sp.started_at >= ?
 GROUP BY t.user
 ORDER BY 4 DESC
 LIMIT ?`, fiveHour, weekly, monthly, limit)
	if err != nil {
		return nil, fmt.Errorf("obs/store.TopUserSpend: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UserSpendRow
	for rows.Next() {
		var r UserSpendRow
		if err := rows.Scan(&r.User, &r.FiveHour, &r.Weekly, &r.Monthly); err != nil {
			return nil, fmt.Errorf("obs/store.TopUserSpend: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountBudgetBreaches returns how many admission verdicts fired a per-end-user
// budget criterion (budget.user_*) since `since` — the would-block tally for
// the status surface (in observe mode these are shadow verdicts).
func (s *Store) CountBudgetBreaches(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM obs_admission_events WHERE criterion_id LIKE 'budget.user_%' AND ts >= ?`,
		ts(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("obs/store.CountBudgetBreaches: %w", err)
	}
	return n, nil
}
