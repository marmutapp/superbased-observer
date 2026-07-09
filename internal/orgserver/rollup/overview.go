package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Overview computes the org-wide (admin) or led-teams (lead) overview rollup
// for the trailing window. Spend is attributed to developers (a team's spend is
// the sum of its members' spend), so the totals reconcile with the per-team and
// per-developer rollups and with each developer's local dashboard.
func Overview(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (OverviewResult, error) {
	s := since(w, now)
	res := OverviewResult{
		WindowDays:  w.days(),
		CostByDay:   []CostPoint{},
		TopTeams:    []TeamSpend{},
		TopProjects: []ProjectSpend{},
	}

	uScope, uArgs := userScopeSQL("user_id", scope)

	// Spend totals + active developers, from the deduped spend CTE.
	totalsQ := spendCTE + `
SELECT COALESCE(SUM(cost),0), COUNT(DISTINCT user_id)
  FROM spend
 WHERE ` + uScope
	totalsArgs := append(spendArgs(s), uArgs...)
	if err := db.QueryRowContext(ctx, totalsQ, totalsArgs...).
		Scan(&res.TotalCostUSD, &res.ActiveDevelopers); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: totals: %w", err)
	}

	// Activity counts (sessions/actions/api_turns) in window, user-scoped.
	var err error
	if res.TotalSessions, err = scopedCount(ctx, db, "sessions", "started_at", s, uScope, uArgs); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: sessions: %w", err)
	}
	if res.TotalActions, err = scopedCount(ctx, db, "actions", "timestamp", s, uScope, uArgs); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: actions: %w", err)
	}
	if res.TotalAPITurns, err = scopedCount(ctx, db, "api_turns", "timestamp", s, uScope, uArgs); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: api_turns: %w", err)
	}

	// Team + project counts.
	tScope, tArgs := teamScopeSQL("team_id", scope)
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_teams WHERE `+tScope, tArgs...).Scan(&res.TeamCount); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: team count: %w", err)
	}
	projCountQ := spendCTE + `
SELECT COUNT(DISTINCT project_root_hash) FROM spend WHERE project_root_hash != '' AND ` + uScope
	if err := db.QueryRowContext(ctx, projCountQ, append(spendArgs(s), uArgs...)...).
		Scan(&res.ProjectCount); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: project count: %w", err)
	}

	// Cost by day.
	if res.CostByDay, err = costByDay(ctx, db, s, uScope, uArgs); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: cost by day: %w", err)
	}

	// Top teams by spend (developer-attributed). Same scope args, column
	// qualified for the join alias.
	tScopeJoin, _ := teamScopeSQL("t.team_id", scope)
	topTeamsQ := spendCTE + `
SELECT t.team_id, t.display_name, COALESCE(SUM(sp.cost),0) AS cost
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
  JOIN org_teams t        ON t.team_id = m.team_id
 WHERE ` + tScopeJoin + `
 GROUP BY t.team_id, t.display_name
 ORDER BY cost DESC, t.display_name
 LIMIT ?`
	topTeamsArgs := append(spendArgs(s), tArgs...)
	topTeamsArgs = append(topTeamsArgs, topN)
	if res.TopTeams, err = queryTeamSpend(ctx, db, topTeamsQ, topTeamsArgs); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: top teams: %w", err)
	}

	// Top projects by spend. The `project_root` JSON field carries the
	// hash hex in metadata-only mode (raw path is stripped server-side);
	// the dashboard renders it as the project identifier. Full-content
	// mode populates the hash via the agent's hashOrComputed path, so
	// aggregation works there too.
	topProjQ := spendCTE + `
SELECT project_root_hash, COALESCE(SUM(cost),0) AS cost
  FROM spend
 WHERE project_root_hash != '' AND ` + uScope + `
 GROUP BY project_root_hash
 ORDER BY cost DESC, project_root_hash
 LIMIT ?`
	topProjArgs := append(spendArgs(s), uArgs...)
	topProjArgs = append(topProjArgs, topN)
	if res.TopProjects, err = queryProjectSpend(ctx, db, topProjQ, topProjArgs); err != nil {
		return OverviewResult{}, fmt.Errorf("rollup.Overview: top projects: %w", err)
	}

	// Phase 1 additive enrichment — token buckets, cache efficiency,
	// reliability split, error rate, proxy-only latency, tool/model mix,
	// activity grids, prior-period deltas. Content-free; degrades gracefully.
	if err := enrichOverview(ctx, db, w, scope, now, &res); err != nil {
		return OverviewResult{}, err
	}

	return res, nil
}

// scopedCount counts rows in table whose tsCol is within the window and whose
// user_id is in scope.
func scopedCount(ctx context.Context, db *sql.DB, table, tsCol, s, uScope string, uArgs []any) (int64, error) {
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s >= ? AND %s`, table, tsCol, uScope) //nolint:gosec // G201: table/tsCol are code-constant identifiers and uScope is a parameterized scope fragment; the window value is bound via ?.
	args := append([]any{s}, uArgs...)
	var n int64
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// costByDay returns the per-day spend series (UTC, ascending) for the window.
func costByDay(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any) ([]CostPoint, error) {
	//nolint:gosec // G202: spendCTE is a code constant and uScope is a parameterized scope fragment; all values are bound via ? args.
	q := spendCTE + `
SELECT substr(ts,1,10) AS day, COALESCE(SUM(cost),0) AS cost
  FROM spend
 WHERE ` + uScope + `
 GROUP BY day
 ORDER BY day`
	rows, err := db.QueryContext(ctx, q, append(spendArgs(s), uArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CostPoint{}
	for rows.Next() {
		var p CostPoint
		if err := rows.Scan(&p.Date, &p.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func queryTeamSpend(ctx context.Context, db *sql.DB, q string, args []any) ([]TeamSpend, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamSpend{}
	for rows.Next() {
		var t TeamSpend
		if err := rows.Scan(&t.TeamID, &t.DisplayName, &t.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func queryProjectSpend(ctx context.Context, db *sql.DB, q string, args []any) ([]ProjectSpend, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectSpend{}
	for rows.Next() {
		var p ProjectSpend
		var hash string
		if err := rows.Scan(&hash, &p.CostUSD); err != nil {
			return nil, err
		}
		// The CTE column is project_root_hash; populate the wire's
		// ProjectRoot field with the hash so the dashboard has a
		// stable identifier to display. ProjectIDFromHash slices the
		// first 16 chars (the hash already IS sha256(rawRoot)).
		p.ProjectRoot = hash
		p.ProjectID = ProjectIDFromHash(hash)
		out = append(out, p)
	}
	return out, rows.Err()
}
