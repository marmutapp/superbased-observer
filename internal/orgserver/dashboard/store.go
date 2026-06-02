package dashboard

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	gen "github.com/marmutapp/superbased-observer/internal/orgserver/dashboard/gen"
	"github.com/marmutapp/superbased-observer/internal/orgserver/rollup"
)

// ErrBudgetExists is returned when a budget already exists for a scope target
// (one budget per (scope, scope_id)).
var ErrBudgetExists = errors.New("dashboard: budget already exists for scope")

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// createBudget inserts a budget row. Thresholds default to 75/90/100% when
// absent. A duplicate (scope, scope_id) maps to ErrBudgetExists.
func (a *API) createBudget(ctx context.Context, in gen.BudgetInput) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	now := a.now().Format(time.RFC3339)
	thresholds := marshalThresholds(in.AlertThresholds)
	_, err = a.db.ExecContext(ctx, `
INSERT INTO budgets (id, scope, scope_id, monthly_usd_cap, alert_webhook_url, alert_thresholds, last_fired_threshold, created_at, updated_at)
VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, 0, ?, ?)`,
		id, string(in.Scope), in.ScopeId, in.MonthlyUsdCap, deref(in.AlertWebhookUrl), thresholds, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrBudgetExists
		}
		return "", fmt.Errorf("dashboard.createBudget: %w", err)
	}
	return id, nil
}

// updateBudget replaces a budget's mutable fields. found is false for an
// unknown id. last_fired_threshold is reset to 0 so a new cap/threshold ladder
// re-alerts cleanly.
func (a *API) updateBudget(ctx context.Context, id string, in gen.BudgetInput) (bool, error) {
	res, err := a.db.ExecContext(ctx, `
UPDATE budgets
   SET scope = ?, scope_id = ?, monthly_usd_cap = ?, alert_webhook_url = NULLIF(?, ''),
       alert_thresholds = ?, last_fired_threshold = 0, updated_at = ?
 WHERE id = ?`,
		string(in.Scope), in.ScopeId, in.MonthlyUsdCap, deref(in.AlertWebhookUrl),
		marshalThresholds(in.AlertThresholds), a.now().Format(time.RFC3339), id)
	if err != nil {
		return false, fmt.Errorf("dashboard.updateBudget: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (a *API) deleteBudget(ctx context.Context, id string) (bool, error) {
	res, err := a.db.ExecContext(ctx, `DELETE FROM budgets WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("dashboard.deleteBudget: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// budgetStatusByID loads one budget joined with its current rolling-30-day
// spend (the same shape the list returns).
func (a *API) budgetStatusByID(ctx context.Context, id string) (rollup.BudgetStatus, bool, error) {
	var b rollup.BudgetStatus
	var thresholds string
	var webhook sql.NullString
	err := a.db.QueryRowContext(ctx, `
SELECT id, scope, scope_id, monthly_usd_cap, alert_webhook_url, alert_thresholds,
       last_fired_threshold, created_at, updated_at
  FROM budgets WHERE id = ?`, id).
		Scan(&b.ID, &b.Scope, &b.ScopeID, &b.MonthlyUSDCap, &webhook, &thresholds,
			&b.LastFiredThreshold, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rollup.BudgetStatus{}, false, nil
	}
	if err != nil {
		return rollup.BudgetStatus{}, false, fmt.Errorf("dashboard.budgetStatusByID: %w", err)
	}
	b.AlertWebhookURL = webhook.String
	b.AlertThresholds = rollup.ParseThresholds(thresholds)
	spend, err := rollup.ScopeSpend(ctx, a.db, b.Scope, b.ScopeID, rollup.BudgetWindowDays, a.now())
	if err != nil {
		return rollup.BudgetStatus{}, false, err
	}
	b.CurrentSpendUSD = spend
	if b.MonthlyUSDCap > 0 {
		b.CurrentRatio = spend / b.MonthlyUSDCap
	}
	b.ScopeLabel = rollup.ScopeLabel(ctx, a.db, b.Scope, b.ScopeID)
	return b, true, nil
}

// revokeBearer adds a jti to the revocation list (idempotent). VerifyBearer
// consults this list, so the bearer is rejected on its next use.
func (a *API) revokeBearer(ctx context.Context, jti string) error {
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO revoked_bearers (jti, revoked_at) VALUES (?, ?)
		 ON CONFLICT(jti) DO NOTHING`, jti, a.now().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("dashboard.revokeBearer: %w", err)
	}
	return nil
}

// listBearers returns a developer's issued bearers with their revoked status.
func (a *API) listBearers(ctx context.Context, userID string) (rollup.BearersResult, error) {
	res := rollup.BearersResult{UserID: userID, Bearers: []rollup.BearerInfo{}}
	rows, err := a.db.QueryContext(ctx, `
SELECT b.jti, b.issued_at, b.expires_at, (r.jti IS NOT NULL)
  FROM issued_bearers b
  LEFT JOIN revoked_bearers r ON r.jti = b.jti
 WHERE b.user_id = ?
 ORDER BY b.issued_at DESC`, userID)
	if err != nil {
		return rollup.BearersResult{}, fmt.Errorf("dashboard.listBearers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bi rollup.BearerInfo
		if err := rows.Scan(&bi.Jti, &bi.IssuedAt, &bi.ExpiresAt, &bi.Revoked); err != nil {
			return rollup.BearersResult{}, err
		}
		res.Bearers = append(res.Bearers, bi)
	}
	return res, rows.Err()
}

// setTeamRole sets a team member's role. found is false when the membership
// does not exist (so promoting a non-member is a 404, not a silent no-op).
func (a *API) setTeamRole(ctx context.Context, teamID, userID, role string) (bool, error) {
	res, err := a.db.ExecContext(ctx,
		`UPDATE org_team_members SET role = ? WHERE team_id = ? AND user_id = ?`, role, teamID, userID)
	if err != nil {
		return false, fmt.Errorf("dashboard.setTeamRole: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func marshalThresholds(in *[]float64) string {
	vals := rollup.ParseThresholds("") // default ladder
	if in != nil && len(*in) > 0 {
		vals = *in
	}
	b, _ := json.Marshal(vals)
	return string(b)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure
// (modernc surfaces it in the error text; there is no typed sentinel).
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
