package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// LiveWindowMinutes is the "active now" window for the Live presence surface.
const LiveWindowMinutes = 15

// LiveSession is one currently-active session (any action / api_turn /
// token_usage row in the last LiveWindowMinutes). Content-free.
type LiveSession struct {
	SessionID   string  `json:"session_id"`
	UserID      string  `json:"user_id"`
	Email       string  `json:"email,omitempty"`
	DisplayName string  `json:"display_name,omitempty"`
	Tool        string  `json:"tool,omitempty"`
	Model       string  `json:"model,omitempty"`
	LastActive  string  `json:"last_active"`
	CostUSD     float64 `json:"cost_usd"`
	ActionCount int64   `json:"action_count"`
}

// LiveResult powers GET /api/org/live — the "who's working now" surface.
type LiveResult struct {
	WindowMinutes int           `json:"window_minutes"`
	ActiveDevs    int           `json:"active_devs"`
	Sessions      []LiveSession `json:"sessions"`
}

// Live returns the sessions active within the last LiveWindowMinutes, scoped
// (admin → all; lead → their teams; member → self). It is the same per-developer
// disclosure class as the session list, so the HANDLER writes a view_org_sessions
// audit row before calling. Content-free: identity + tool/model + counts + cost.
func Live(ctx context.Context, db *sql.DB, scope Scope, selfUserID string, now time.Time) (LiveResult, error) {
	res := LiveResult{WindowMinutes: LiveWindowMinutes, Sessions: []LiveSession{}}
	uScope, uArgs := peopleScopeSQL("user_id", scope, selfUserID)
	if uScope == falseScope {
		return res, nil
	}
	cut := now.UTC().Add(-LiveWindowMinutes * time.Minute).Format(time.RFC3339)

	// Distinct active session ids + their most-recent activity timestamp, from
	// the union of the three activity substrates.
	//nolint:gosec // G201: uScope is a parameterized fragment; cut binds via ?.
	recentQ := `
WITH act AS (
    SELECT user_id, COALESCE(session_id,'') AS sid, timestamp AS ts FROM actions     WHERE timestamp >= ?
    UNION ALL
    SELECT user_id, COALESCE(session_id,'') AS sid, timestamp AS ts FROM api_turns   WHERE timestamp >= ?
    UNION ALL
    SELECT user_id, COALESCE(session_id,'') AS sid, timestamp AS ts FROM token_usage WHERE timestamp >= ?
)
SELECT sid, MAX(ts) FROM act WHERE sid != '' AND ` + uScope + ` GROUP BY sid ORDER BY MAX(ts) DESC`

	byID := map[string]*LiveSession{}
	order := []string{}
	if err := eachRow(ctx, db, recentQ, append([]any{cut, cut, cut}, uArgs...), func(rows *sql.Rows) error {
		var sid, last string
		if err := rows.Scan(&sid, &last); err != nil {
			return err
		}
		byID[sid] = &LiveSession{SessionID: sid, LastActive: last}
		order = append(order, sid)
		return nil
	}); err != nil {
		return LiveResult{}, fmt.Errorf("rollup.Live: recent: %w", err)
	}
	if len(order) == 0 {
		return res, nil
	}

	// Session meta (tool/model/user) for the active set.
	if err := liveMeta(ctx, db, order, byID); err != nil {
		return LiveResult{}, fmt.Errorf("rollup.Live: meta: %w", err)
	}
	// Cost + action count per session over the live window (cheap-small set).
	if err := liveSpend(ctx, db, cut, uScope, uArgs, byID); err != nil {
		return LiveResult{}, fmt.Errorf("rollup.Live: spend: %w", err)
	}

	devs := map[string]bool{}
	out := make([]LiveSession, 0, len(order))
	for _, id := range order {
		s := byID[id]
		out = append(out, *s)
		if s.UserID != "" {
			devs[s.UserID] = true
		}
	}
	res.Sessions = out
	res.ActiveDevs = len(devs)
	return res, nil
}

// liveMeta fills tool/model/user/email/display for the active session set.
func liveMeta(ctx context.Context, db *sql.DB, ids []string, byID map[string]*LiveSession) error {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	//nolint:gosec // G201: only a ?-placeholder list is interpolated; ids bind via ?.
	q := `SELECT s.id, s.user_id, COALESCE(s.tool,''), COALESCE(s.model,''),
                 COALESCE(m.email,''), COALESCE(m.display_name,'')
            FROM sessions s
            LEFT JOIN org_members m ON m.user_id = s.user_id
           WHERE s.id IN (` + placeholders(len(ids)) + `)`
	return eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var id, user, tool, model, email, disp string
		if err := rows.Scan(&id, &user, &tool, &model, &email, &disp); err != nil {
			return err
		}
		if s, ok := byID[id]; ok {
			s.UserID, s.Tool, s.Model, s.Email, s.DisplayName = user, tool, model, email, disp
		}
		return nil
	})
}

// liveSpend fills CostUSD + ActionCount per active session over the window.
func liveSpend(ctx context.Context, db *sql.DB, cut, uScope string, uArgs []any, byID map[string]*LiveSession) error {
	//nolint:gosec // G202: sessionSpendCTE is a code constant; uScope parameterized; values bind via ?.
	q := sessionSpendCTE + `
SELECT sid, COALESCE(SUM(cost),0) FROM sspend WHERE sid != '' AND ` + uScope + ` GROUP BY sid`
	if err := eachRow(ctx, db, q, append(spendArgs(cut), uArgs...), func(rows *sql.Rows) error {
		var sid string
		var cost float64
		if err := rows.Scan(&sid, &cost); err != nil {
			return err
		}
		if s, ok := byID[sid]; ok {
			s.CostUSD = cost
		}
		return nil
	}); err != nil {
		return err
	}
	// Action counts in the window.
	aq := `SELECT COALESCE(session_id,''), COUNT(*) FROM actions WHERE timestamp >= ? AND ` + uScope + ` GROUP BY session_id`
	//nolint:gosec // G201: uScope parameterized; cut binds via ?.
	return eachRow(ctx, db, aq, append([]any{cut}, uArgs...), func(rows *sql.Rows) error {
		var sid string
		var n int64
		if err := rows.Scan(&sid, &n); err != nil {
			return err
		}
		if s, ok := byID[sid]; ok {
			s.ActionCount = n
		}
		return nil
	})
}
