package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Projects lists projects in scope with a team-overlap indicator (the set of
// teams whose members touched the project — a cross-team project is the
// "overlap"). Spend, developer counts, and the team set are all restricted to
// the caller's scope, so a lead never sees another team's slice of a shared
// project.
func Projects(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (ProjectsResult, error) {
	s := since(w, now)
	res := ProjectsResult{WindowDays: w.days(), Projects: []ProjectRollup{}}

	uScope, uArgs := userScopeSQL("user_id", scope)

	// 1. Per-project spend + active developers.
	spendRows, err := db.QueryContext(ctx, spendCTE+`
SELECT project_root, COALESCE(SUM(cost),0), COUNT(DISTINCT user_id)
  FROM spend
 WHERE project_root != '' AND `+uScope+`
 GROUP BY project_root
 ORDER BY 2 DESC, project_root`, append(spendArgs(s), uArgs...)...)
	if err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: spend: %w", err)
	}
	order := []string{}
	byRoot := map[string]*ProjectRollup{}
	for spendRows.Next() {
		var root string
		var cost float64
		var devs int64
		if err := spendRows.Scan(&root, &cost, &devs); err != nil {
			_ = spendRows.Close()
			return ProjectsResult{}, err
		}
		byRoot[root] = &ProjectRollup{
			ProjectID:        ProjectID(root),
			ProjectRoot:      root,
			CostUSD:          cost,
			ActiveDevelopers: devs,
			Teams:            []TeamRef{},
			Tools:            []string{},
		}
		order = append(order, root)
	}
	if err := spendRows.Err(); err != nil {
		_ = spendRows.Close()
		return ProjectsResult{}, err
	}
	_ = spendRows.Close()
	if len(order) == 0 {
		return res, nil
	}

	// 2. Session counts + tools per project (from sessions, scope-restricted).
	uScopeS, uArgsS := userScopeSQL("user_id", scope)
	//nolint:gosec // G202: SQL structure is code-constant and uScopeS is a parameterized scope fragment; all values are bound via ? args.
	srows, err := db.QueryContext(ctx, `
SELECT COALESCE(project_root,''), COALESCE(tool,''), COUNT(*)
  FROM sessions
 WHERE started_at >= ? AND COALESCE(project_root,'') != '' AND `+uScopeS+`
 GROUP BY project_root, tool`, append([]any{s}, uArgsS...)...)
	if err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: sessions: %w", err)
	}
	toolSet := map[string]map[string]struct{}{}
	for srows.Next() {
		var root, tool string
		var n int64
		if err := srows.Scan(&root, &tool, &n); err != nil {
			_ = srows.Close()
			return ProjectsResult{}, err
		}
		if p, ok := byRoot[root]; ok {
			p.SessionCount += n
			if tool != "" {
				if toolSet[root] == nil {
					toolSet[root] = map[string]struct{}{}
				}
				toolSet[root][tool] = struct{}{}
			}
		}
	}
	if err := srows.Err(); err != nil {
		_ = srows.Close()
		return ProjectsResult{}, err
	}
	_ = srows.Close()

	// 3. Team overlap per project (teams whose members touched it, scoped).
	teamSet, err := projectTeams(ctx, db, s, uScope, uArgs)
	if err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: teams: %w", err)
	}

	for _, root := range order {
		p := byRoot[root]
		p.Tools = sortedKeys(toolSet[root])
		p.Teams = teamSet[root]
		res.Projects = append(res.Projects, *p)
	}
	return res, nil
}

// ProjectDetail computes the detail for one project (identified by its id hash),
// scope-restricted. found is false when no in-scope project hashes to id.
func ProjectDetail(ctx context.Context, db *sql.DB, w Window, projectID string, scope Scope, now time.Time) (res ProjectDetailResult, found bool, err error) {
	s := since(w, now)
	uScope, uArgs := userScopeSQL("user_id", scope)

	root, ok, err := projectRootByID(ctx, db, s, projectID, uScope, uArgs)
	if err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: resolve: %w", err)
	}
	if !ok {
		return ProjectDetailResult{}, false, nil
	}

	res = ProjectDetailResult{
		ProjectID:   projectID,
		ProjectRoot: root,
		WindowDays:  w.days(),
		Teams:       []TeamRef{},
		Tools:       []string{},
		CostByDay:   []CostPoint{},
		TopModels:   []ModelSpend{},
	}

	// Cost + active developers for this project, scoped.
	projScope := "project_root = ? AND " + uScope
	projArgs := append([]any{root}, uArgs...)
	if err := db.QueryRowContext(ctx, spendCTE+`
SELECT COALESCE(SUM(cost),0), COUNT(DISTINCT user_id) FROM spend WHERE `+projScope,
		appendArgs(spendArgs(s), projArgs...)...).Scan(&res.CostUSD, &res.ActiveDevelopers); err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: spend: %w", err)
	}

	if res.CostByDay, err = costByDay(ctx, db, s, projScope, projArgs); err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: cost by day: %w", err)
	}

	topModelsQ := spendCTE + `
SELECT model, COALESCE(SUM(cost),0), COALESCE(SUM(tokens),0)
  FROM spend WHERE model != '' AND ` + projScope + `
 GROUP BY model ORDER BY 2 DESC, model LIMIT ?`
	if res.TopModels, err = queryModelSpend(ctx, db, topModelsQ, appendArgs(spendArgs(s), append(projArgs, topN)...)); err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: top models: %w", err)
	}

	// Session count + tools for this project, scoped.
	//nolint:gosec // G202: SQL structure is code-constant and uScope is a parameterized scope fragment; all values are bound via ? args.
	srows, err := db.QueryContext(ctx, `
SELECT COALESCE(tool,''), COUNT(*) FROM sessions
 WHERE started_at >= ? AND project_root = ? AND `+uScope+`
 GROUP BY tool`, appendArgs([]any{s, root}, uArgs...)...)
	if err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: sessions: %w", err)
	}
	tools := map[string]struct{}{}
	for srows.Next() {
		var tool string
		var n int64
		if err := srows.Scan(&tool, &n); err != nil {
			_ = srows.Close()
			return ProjectDetailResult{}, false, err
		}
		res.SessionCount += n
		if tool != "" {
			tools[tool] = struct{}{}
		}
	}
	if err := srows.Err(); err != nil {
		_ = srows.Close()
		return ProjectDetailResult{}, false, err
	}
	_ = srows.Close()
	res.Tools = sortedKeys(tools)

	// Teams touching this project, scoped.
	teamSet, err := projectTeams(ctx, db, s, projScope, projArgs)
	if err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: teams: %w", err)
	}
	res.Teams = teamSet[root]
	if res.Teams == nil {
		res.Teams = []TeamRef{}
	}

	return res, true, nil
}

// projectTeams returns, per project_root, the set of teams whose members
// touched it within the window, applying the given spend scope clause.
func projectTeams(ctx context.Context, db *sql.DB, s, scopeSQL string, scopeArgs []any) (map[string][]TeamRef, error) {
	//nolint:gosec // G202: spendCTE and JOIN/WHERE fragments are code constants and scopeSQL is a parameterized scope fragment; all values are bound via ? args.
	q := spendCTE + `
SELECT DISTINCT sp.project_root, t.team_id, t.display_name
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
  JOIN org_teams t        ON t.team_id = m.team_id
 WHERE sp.project_root != '' AND ` + scopeSQL + `
 ORDER BY sp.project_root, t.display_name`
	rows, err := db.QueryContext(ctx, q, appendArgs(spendArgs(s), scopeArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]TeamRef{}
	for rows.Next() {
		var root string
		var ref TeamRef
		if err := rows.Scan(&root, &ref.TeamID, &ref.DisplayName); err != nil {
			return nil, err
		}
		out[root] = append(out[root], ref)
	}
	return out, rows.Err()
}

// projectRootByID scans the distinct in-scope project_roots in the window and
// returns the one whose ProjectID equals id (a project_root is not recoverable
// from its hash).
func projectRootByID(ctx context.Context, db *sql.DB, s, id, uScope string, uArgs []any) (string, bool, error) {
	rows, err := db.QueryContext(ctx, spendCTE+`
SELECT DISTINCT project_root FROM spend WHERE project_root != '' AND `+uScope,
		append(spendArgs(s), uArgs...)...)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return "", false, err
		}
		if ProjectID(root) == id {
			return root, true, nil
		}
	}
	return "", false, rows.Err()
}

// sortedKeys returns the sorted keys of a string set.
func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}
