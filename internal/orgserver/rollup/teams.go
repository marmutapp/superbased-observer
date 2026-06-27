package rollup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Teams lists the teams in scope with aggregate (never per-developer) rollups
// for the window. Spend is developer-attributed: a team's spend is the sum of
// its members' spend.
func Teams(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (TeamsResult, error) {
	s := since(w, now)
	res := TeamsResult{WindowDays: w.days(), Teams: []TeamRollup{}}

	tScope, tArgs := teamScopeSQL("team_id", scope)

	// 1. Teams in scope (ordered for stable output).
	rows, err := db.QueryContext(ctx,
		`SELECT team_id, display_name FROM org_teams WHERE `+tScope+` ORDER BY display_name, team_id`, tArgs...) //nolint:gosec // G202: SQL structure is code-constant and tScope is a parameterized scope fragment; all values are bound via ? args.
	if err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: list: %w", err)
	}
	order := []string{}
	byID := map[string]*TeamRollup{}
	for rows.Next() {
		var t TeamRollup
		if err := rows.Scan(&t.TeamID, &t.DisplayName); err != nil {
			_ = rows.Close()
			return TeamsResult{}, fmt.Errorf("rollup.Teams: scan: %w", err)
		}
		cp := t
		byID[t.TeamID] = &cp
		order = append(order, t.TeamID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return TeamsResult{}, err
	}
	_ = rows.Close()
	if len(order) == 0 {
		return res, nil
	}

	// 2. Member counts (all members, activity-independent).
	members, err := groupCount(ctx, db,
		`SELECT team_id, COUNT(*) FROM org_team_members GROUP BY team_id`)
	if err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: members: %w", err)
	}

	// 3. Per-team spend + active developers (developer-attributed).
	spendRows, err := db.QueryContext(ctx, spendCTE+`
SELECT m.team_id, COALESCE(SUM(sp.cost),0), COUNT(DISTINCT sp.user_id)
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
 GROUP BY m.team_id`, spendArgs(s)...)
	if err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: spend: %w", err)
	}
	for spendRows.Next() {
		var id string
		var cost float64
		var devs int64
		if err := spendRows.Scan(&id, &cost, &devs); err != nil {
			_ = spendRows.Close()
			return TeamsResult{}, err
		}
		if t, ok := byID[id]; ok {
			t.CostUSD = cost
			t.ActiveDevelopers = devs
		}
	}
	if err := spendRows.Err(); err != nil {
		_ = spendRows.Close()
		return TeamsResult{}, err
	}
	_ = spendRows.Close()

	// 4. Session + action counts per team.
	sessions, err := groupCountJoin(ctx, db, "sessions", "started_at", s)
	if err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: sessions: %w", err)
	}
	actions, err := groupCountJoin(ctx, db, "actions", "timestamp", s)
	if err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: actions: %w", err)
	}

	// 5. Trailing 7-day spend sparkline + top tools per team (fill byID
	// pointers directly; both content-free).
	if err := teamSpark(ctx, db, now, byID); err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: spark: %w", err)
	}
	if err := teamTopTools(ctx, db, s, byID); err != nil {
		return TeamsResult{}, fmt.Errorf("rollup.Teams: top tools: %w", err)
	}

	for _, id := range order {
		t := byID[id]
		t.MemberCount = members[id]
		t.SessionCount = sessions[id]
		t.ActionCount = actions[id]
		res.Teams = append(res.Teams, *t)
	}
	return res, nil
}

// sparkDayIndex maps each of the trailing sparkDays YYYY-MM-DD strings (UTC,
// oldest→newest) to its slice index, shared by the team/project sparklines.
func sparkDayIndex(now time.Time) map[string]int {
	idx := make(map[string]int, sparkDays)
	for i := 0; i < sparkDays; i++ {
		day := now.UTC().AddDate(0, 0, -(sparkDays - 1 - i)).Format("2006-01-02")
		idx[day] = i
	}
	return idx
}

// teamSpark fills the trailing sparkDays-day daily-spend sparkline per team
// (sum of the team members' deduped spend, zero-filled), for teams in byID.
func teamSpark(ctx context.Context, db *sql.DB, now time.Time, byID map[string]*TeamRollup) error {
	s7 := since(Window{Days: sparkDays}, now)
	idx := sparkDayIndex(now)
	//nolint:gosec // G202: spendCTE is a code constant; the only joined identifiers are code-constant; values bind via ?.
	q := spendCTE + `
SELECT m.team_id, substr(sp.ts,1,10) AS d, COALESCE(SUM(sp.cost),0)
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
 GROUP BY m.team_id, d`
	return eachRow(ctx, db, q, spendArgs(s7), func(rows *sql.Rows) error {
		var id, day string
		var cost float64
		if err := rows.Scan(&id, &day, &cost); err != nil {
			return err
		}
		t, ok := byID[id]
		if !ok {
			return nil // team out of scope
		}
		if t.Spark == nil {
			t.Spark = make([]float64, sparkDays)
		}
		if i, ok := idx[day]; ok {
			t.Spark[i] += cost
		}
		return nil
	})
}

// teamTopToolsN bounds how many tools a team row surfaces.
const teamTopToolsN = 4

// teamTopTools fills the top tools (by cost, descending) per team for teams in
// byID, from the deduped enriched substrate joined through team membership.
func teamTopTools(ctx context.Context, db *sql.DB, s string, byID map[string]*TeamRollup) error {
	//nolint:gosec // G202: enrichedCTE is a code constant; the only joined identifiers are code-constant; values bind via ?.
	q := enrichedCTE + `
SELECT m.team_id, ev.tool, COALESCE(SUM(ev.cost),0) c
  FROM ev
  JOIN org_team_members m ON m.user_id = ev.user_id
 WHERE ev.tool != ''
 GROUP BY m.team_id, ev.tool`
	byTeam := map[string][]toolCost{}
	if err := eachRow(ctx, db, q, spendArgs(s), func(rows *sql.Rows) error {
		var id, tool string
		var c float64
		if err := rows.Scan(&id, &tool, &c); err != nil {
			return err
		}
		if _, ok := byID[id]; ok {
			byTeam[id] = append(byTeam[id], toolCost{tool, c})
		}
		return nil
	}); err != nil {
		return err
	}
	for id, tcs := range byTeam {
		sort.SliceStable(tcs, func(i, j int) bool {
			if tcs[i].cost != tcs[j].cost {
				return tcs[i].cost > tcs[j].cost
			}
			return tcs[i].tool < tcs[j].tool
		})
		n := len(tcs)
		if n > teamTopToolsN {
			n = teamTopToolsN
		}
		tools := make([]string, n)
		for i := 0; i < n; i++ {
			tools[i] = tcs[i].tool
		}
		byID[id].TopTools = tools
	}
	return nil
}

// toolCost pairs a tool label with its cost for the top-tools ranking.
type toolCost struct {
	tool string
	cost float64
}

// TeamDetail computes the aggregate detail for one team (no per-developer
// split — that is Developers). The handler enforces that the caller may see
// this team; this query trusts it. found is false when the team id is unknown.
func TeamDetail(ctx context.Context, db *sql.DB, w Window, teamID string, now time.Time) (res TeamDetailResult, found bool, err error) {
	s := since(w, now)
	res = TeamDetailResult{
		TeamID:      teamID,
		WindowDays:  w.days(),
		CostByDay:   []CostPoint{},
		TopProjects: []ProjectSpend{},
		TopModels:   []ModelSpend{},
	}

	err = db.QueryRowContext(ctx, `SELECT display_name FROM org_teams WHERE team_id = ?`, teamID).
		Scan(&res.DisplayName)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamDetailResult{}, false, nil
	}
	if err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: team: %w", err)
	}

	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_team_members WHERE team_id = ?`, teamID).
		Scan(&res.MemberCount); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: members: %w", err)
	}

	// Spend + active developers for the team's members.
	totQ := spendCTE + `
SELECT COALESCE(SUM(sp.cost),0), COUNT(DISTINCT sp.user_id)
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
 WHERE m.team_id = ?`
	if err := db.QueryRowContext(ctx, totQ, append(spendArgs(s), teamID)...).
		Scan(&res.CostUSD, &res.ActiveDevelopers); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: spend: %w", err)
	}

	memberScope := "user_id IN (SELECT user_id FROM org_team_members WHERE team_id = ?)"
	if res.SessionCount, err = scopedCount(ctx, db, "sessions", "started_at", s, memberScope, []any{teamID}); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: sessions: %w", err)
	}
	if res.ActionCount, err = scopedCount(ctx, db, "actions", "timestamp", s, memberScope, []any{teamID}); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: actions: %w", err)
	}
	if res.APITurnCount, err = scopedCount(ctx, db, "api_turns", "timestamp", s, memberScope, []any{teamID}); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: api_turns: %w", err)
	}

	// Cost by day, top projects, top models — all over the team's members.
	memberSpendScope := "user_id IN (SELECT user_id FROM org_team_members WHERE team_id = ?)"
	if res.CostByDay, err = costByDay(ctx, db, s, memberSpendScope, []any{teamID}); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: cost by day: %w", err)
	}

	topProjQ := spendCTE + `
SELECT project_root_hash, COALESCE(SUM(cost),0) AS cost
  FROM spend
 WHERE project_root_hash != '' AND ` + memberSpendScope + `
 GROUP BY project_root_hash ORDER BY cost DESC, project_root_hash LIMIT ?`
	if res.TopProjects, err = queryProjectSpend(ctx, db, topProjQ, appendArgs(spendArgs(s), teamID, topN)); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: top projects: %w", err)
	}

	topModelsQ := spendCTE + `
SELECT model, COALESCE(SUM(cost),0) AS cost, COALESCE(SUM(tokens),0) AS toks
  FROM spend
 WHERE model != '' AND ` + memberSpendScope + `
 GROUP BY model ORDER BY cost DESC, model LIMIT ?`
	if res.TopModels, err = queryModelSpend(ctx, db, topModelsQ, appendArgs(spendArgs(s), teamID, topN)); err != nil {
		return TeamDetailResult{}, false, fmt.Errorf("rollup.TeamDetail: top models: %w", err)
	}

	return res, true, nil
}

// groupCount runs a `SELECT key, COUNT(...)` query and returns a key→count map.
func groupCount(ctx context.Context, db *sql.DB, q string, args ...any) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// groupCountJoin counts rows of <table> within the window per team, joining the
// row's user_id to org_team_members.
func groupCountJoin(ctx context.Context, db *sql.DB, table, tsCol, s string) (map[string]int64, error) {
	q := fmt.Sprintf(`
SELECT m.team_id, COUNT(*)
  FROM %s x
  JOIN org_team_members m ON m.user_id = x.user_id
 WHERE x.%s >= ?
 GROUP BY m.team_id`, table, tsCol)
	return groupCount(ctx, db, q, s)
}

func queryModelSpend(ctx context.Context, db *sql.DB, q string, args []any) ([]ModelSpend, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelSpend{}
	for rows.Next() {
		var m ModelSpend
		if err := rows.Scan(&m.Model, &m.CostUSD, &m.Tokens); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// appendArgs appends trailing scalar args to a copy of base.
func appendArgs(base []any, extra ...any) []any {
	out := make([]any, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}
