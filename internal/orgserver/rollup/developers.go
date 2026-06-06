package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Developers computes the per-developer breakdown for one team's members over
// the window. This is the privacy-sensitive drill-down: the HANDLER must write
// the audit_log row before calling this. found is false for an unknown team.
//
// The full roster is returned (members with zero activity included) so the view
// reflects team membership, not just who happened to be active.
func Developers(ctx context.Context, db *sql.DB, w Window, teamID string, now time.Time) (res DevelopersResult, found bool, err error) {
	s := since(w, now)
	res = DevelopersResult{TeamID: teamID, WindowDays: w.days(), Developers: []DeveloperRollup{}}

	err = db.QueryRowContext(ctx, `SELECT display_name FROM org_teams WHERE team_id = ?`, teamID).
		Scan(&res.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return DevelopersResult{}, false, nil
	}
	if err != nil {
		return DevelopersResult{}, false, fmt.Errorf("rollup.Developers: team: %w", err)
	}

	// Roster (ordered for stable output).
	rows, err := db.QueryContext(ctx, `
SELECT m.user_id, m.role, om.email, COALESCE(om.display_name,'')
  FROM org_team_members m
  JOIN org_members om ON om.user_id = m.user_id
 WHERE m.team_id = ?
 ORDER BY om.email, m.user_id`, teamID)
	if err != nil {
		return DevelopersResult{}, false, fmt.Errorf("rollup.Developers: roster: %w", err)
	}
	order := []string{}
	byID := map[string]*DeveloperRollup{}
	for rows.Next() {
		var d DeveloperRollup
		if err := rows.Scan(&d.UserID, &d.Role, &d.Email, &d.DisplayName); err != nil {
			_ = rows.Close()
			return DevelopersResult{}, false, fmt.Errorf("rollup.Developers: scan: %w", err)
		}
		cp := d
		byID[d.UserID] = &cp
		order = append(order, d.UserID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return DevelopersResult{}, false, err
	}
	_ = rows.Close()
	if len(order) == 0 {
		return res, true, nil
	}

	// Per-developer spend + last active.
	spendRows, err := db.QueryContext(ctx, spendCTE+`
SELECT sp.user_id, COALESCE(SUM(sp.cost),0), MAX(sp.ts)
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
 WHERE m.team_id = ?
 GROUP BY sp.user_id`, append(spendArgs(s), teamID)...)
	if err != nil {
		return DevelopersResult{}, false, fmt.Errorf("rollup.Developers: spend: %w", err)
	}
	for spendRows.Next() {
		var id, last string
		var cost float64
		if err := spendRows.Scan(&id, &cost, &last); err != nil {
			_ = spendRows.Close()
			return DevelopersResult{}, false, err
		}
		if d, ok := byID[id]; ok {
			d.CostUSD = cost
			d.LastActive = last
		}
	}
	if err := spendRows.Err(); err != nil {
		_ = spendRows.Close()
		return DevelopersResult{}, false, err
	}
	_ = spendRows.Close()

	// Per-developer session + action counts (restricted to this team's roster).
	memberScope := "user_id IN (SELECT user_id FROM org_team_members WHERE team_id = ?)"
	sessions, err := perUserCount(ctx, db, "sessions", "started_at", s, memberScope, teamID)
	if err != nil {
		return DevelopersResult{}, false, fmt.Errorf("rollup.Developers: sessions: %w", err)
	}
	actions, err := perUserCount(ctx, db, "actions", "timestamp", s, memberScope, teamID)
	if err != nil {
		return DevelopersResult{}, false, fmt.Errorf("rollup.Developers: actions: %w", err)
	}

	for _, id := range order {
		d := byID[id]
		d.SessionCount = sessions[id]
		d.ActionCount = actions[id]
		res.Developers = append(res.Developers, *d)
	}
	return res, true, nil
}

// perUserCount counts rows of <table> in the window per user_id, restricted by
// the given scope clause (which references a trailing teamID arg).
func perUserCount(ctx context.Context, db *sql.DB, table, tsCol, s, scope, teamID string) (map[string]int64, error) {
	q := fmt.Sprintf(`SELECT user_id, COUNT(*) FROM %s WHERE %s >= ? AND %s GROUP BY user_id`, table, tsCol, scope)
	return groupCount(ctx, db, q, s, teamID)
}
