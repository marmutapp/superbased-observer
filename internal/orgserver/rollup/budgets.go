package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// BudgetWindowDays is the rolling window budgets are evaluated over ("monthly"
// cap == trailing 30-day spend), per the build prompt.
const BudgetWindowDays = 30

// Budgets lists all budgets joined with their current rolling-30-day spend and
// ratio. Budgets are an admin-only surface, so no Scope filter is applied.
func Budgets(ctx context.Context, db *sql.DB, now time.Time) (BudgetsResult, error) {
	res := BudgetsResult{Budgets: []BudgetStatus{}}
	rows, err := db.QueryContext(ctx, `
SELECT id, scope, scope_id, monthly_usd_cap, COALESCE(alert_webhook_url,''),
       alert_thresholds, last_fired_threshold, created_at, updated_at
  FROM budgets
 ORDER BY scope, scope_id`)
	if err != nil {
		return BudgetsResult{}, fmt.Errorf("rollup.Budgets: list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b BudgetStatus
		var thresholds string
		if err := rows.Scan(&b.ID, &b.Scope, &b.ScopeID, &b.MonthlyUSDCap, &b.AlertWebhookURL,
			&thresholds, &b.LastFiredThreshold, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return BudgetsResult{}, fmt.Errorf("rollup.Budgets: scan: %w", err)
		}
		b.AlertThresholds = ParseThresholds(thresholds)
		res.Budgets = append(res.Budgets, b)
	}
	if err := rows.Err(); err != nil {
		return BudgetsResult{}, err
	}

	// Enrich each with current spend + label.
	for i := range res.Budgets {
		b := &res.Budgets[i]
		spend, err := ScopeSpend(ctx, db, b.Scope, b.ScopeID, BudgetWindowDays, now)
		if err != nil {
			return BudgetsResult{}, fmt.Errorf("rollup.Budgets: spend %s/%s: %w", b.Scope, b.ScopeID, err)
		}
		b.CurrentSpendUSD = spend
		if b.MonthlyUSDCap > 0 {
			b.CurrentRatio = spend / b.MonthlyUSDCap
		}
		b.ScopeLabel = ScopeLabel(ctx, db, b.Scope, b.ScopeID)
	}
	return res, nil
}

// ScopeSpend computes the trailing-`days` spend for a budget scope. For a team
// the scope_id is a team_id and spend is its members' (developer-attributed);
// for a project the scope_id is the project_root_hash (the privacy-stripped
// project dimension — see spendCTE) and spend is that project's.
// It is the single spend definition shared by the budget list and the budget
// evaluator goroutine, so an alert and the dashboard never disagree.
func ScopeSpend(ctx context.Context, db *sql.DB, scope, scopeID string, days int, now time.Time) (float64, error) {
	s := since(Window{Days: days}, now)
	var q string
	var args []any
	switch scope {
	case "team":
		q = spendCTE + `
SELECT COALESCE(SUM(sp.cost),0)
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
 WHERE m.team_id = ?`
		args = append(spendArgs(s), scopeID)
	case "project":
		q = spendCTE + `SELECT COALESCE(SUM(cost),0) FROM spend WHERE project_root_hash = ?`
		args = append(spendArgs(s), scopeID)
	default:
		return 0, fmt.Errorf("rollup.ScopeSpend: unknown scope %q", scope)
	}
	var spend float64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&spend); err != nil {
		return 0, fmt.Errorf("rollup.ScopeSpend: %w", err)
	}
	return spend, nil
}

// ScopeLabel resolves a human-readable label for a budget scope (the team
// display name, or the project_root itself). Falls back to the scope_id.
func ScopeLabel(ctx context.Context, db *sql.DB, scope, scopeID string) string {
	if scope == "team" {
		var name string
		if err := db.QueryRowContext(ctx,
			`SELECT display_name FROM org_teams WHERE team_id = ?`, scopeID).Scan(&name); err == nil && name != "" {
			return name
		}
	}
	return scopeID
}

// ParseThresholds parses the JSON alert_thresholds column into a slice, falling
// back to the default 75/90/100% ladder on empty/invalid input.
func ParseThresholds(raw string) []float64 {
	var out []float64
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err == nil && len(out) > 0 {
			return out
		}
	}
	return []float64{0.75, 0.9, 1.0}
}
