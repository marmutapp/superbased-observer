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

	// 1. Per-project spend + active developers. Aggregation key is the
	// project_root_hash (the privacy-stripped column is the raw root —
	// not what we group by in metadata-only mode). See spendCTE for
	// the JOIN-fallback that handles proxy-fed api_turns whose own
	// hash is empty.
	spendRows, err := db.QueryContext(ctx, spendCTE+`
SELECT project_root_hash, COALESCE(SUM(cost),0), COUNT(DISTINCT user_id)
  FROM spend
 WHERE project_root_hash != '' AND `+uScope+`
 GROUP BY project_root_hash
 ORDER BY 2 DESC, project_root_hash`, append(spendArgs(s), uArgs...)...)
	if err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: spend: %w", err)
	}
	order := []string{}
	byRoot := map[string]*ProjectRollup{}
	for spendRows.Next() {
		var hash string
		var cost float64
		var devs int64
		if err := spendRows.Scan(&hash, &cost, &devs); err != nil {
			_ = spendRows.Close()
			return ProjectsResult{}, err
		}
		byRoot[hash] = &ProjectRollup{
			ProjectID:        ProjectIDFromHash(hash),
			ProjectRoot:      hash,
			CostUSD:          cost,
			ActiveDevelopers: devs,
			Teams:            []TeamRef{},
			Tools:            []string{},
		}
		order = append(order, hash)
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
	// sessions.project_root_hash is always populated in v1.8.0+ data
	// (the ingest layer's hashOrComputed seeds it from raw if needed),
	// so it's a stable join key even in metadata-only mode.
	uScopeS, uArgsS := userScopeSQL("user_id", scope)
	//nolint:gosec // G202: SQL structure is code-constant and uScopeS is a parameterized scope fragment; all values are bound via ? args.
	srows, err := db.QueryContext(ctx, `
SELECT COALESCE(project_root_hash,''), COALESCE(tool,''), COUNT(*)
  FROM sessions
 WHERE started_at >= ? AND COALESCE(project_root_hash,'') != '' AND `+uScopeS+`
 GROUP BY project_root_hash, tool`, append([]any{s}, uArgsS...)...)
	if err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: sessions: %w", err)
	}
	toolSet := map[string]map[string]struct{}{}
	for srows.Next() {
		var hash, tool string
		var n int64
		if err := srows.Scan(&hash, &tool, &n); err != nil {
			_ = srows.Close()
			return ProjectsResult{}, err
		}
		if p, ok := byRoot[hash]; ok {
			p.SessionCount += n
			if tool != "" {
				if toolSet[hash] == nil {
					toolSet[hash] = map[string]struct{}{}
				}
				toolSet[hash][tool] = struct{}{}
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

	// 4. Trailing 7-day spend sparkline + token-bucket split per project.
	if err := projectSpark(ctx, db, now, uScope, uArgs, byRoot); err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: spark: %w", err)
	}
	if err := projectBuckets(ctx, db, s, uScope, uArgs, byRoot); err != nil {
		return ProjectsResult{}, fmt.Errorf("rollup.Projects: buckets: %w", err)
	}

	for _, hash := range order {
		p := byRoot[hash]
		p.Tools = sortedKeys(toolSet[hash])
		// teamSet[hash] is nil for a project with no team overlap; a nil
		// slice marshals to JSON null and crashes the dashboard's
		// p.teams.length read (the ProjectDetail path already guards this).
		p.Teams = teamSet[hash]
		if p.Teams == nil {
			p.Teams = []TeamRef{}
		}
		res.Projects = append(res.Projects, *p)
	}
	return res, nil
}

// projectSpark fills the trailing sparkDays-day daily-spend sparkline per
// project (by project_root_hash, zero-filled), scoped, for projects in byRoot.
func projectSpark(ctx context.Context, db *sql.DB, now time.Time, uScope string, uArgs []any, byRoot map[string]*ProjectRollup) error {
	s7 := since(Window{Days: sparkDays}, now)
	idx := sparkDayIndex(now)
	//nolint:gosec // G202: spendCTE is a code constant and uScope is a parameterized scope fragment; values bind via ?.
	q := spendCTE + `
SELECT project_root_hash, substr(ts,1,10) AS d, COALESCE(SUM(cost),0)
  FROM spend WHERE project_root_hash != '' AND ` + uScope + `
 GROUP BY project_root_hash, d`
	return eachRow(ctx, db, q, append(spendArgs(s7), uArgs...), func(rows *sql.Rows) error {
		var hash, day string
		var cost float64
		if err := rows.Scan(&hash, &day, &cost); err != nil {
			return err
		}
		p, ok := byRoot[hash]
		if !ok {
			return nil
		}
		if p.Spark == nil {
			p.Spark = make([]float64, sparkDays)
		}
		if i, ok := idx[day]; ok {
			p.Spark[i] += cost
		}
		return nil
	})
}

// projectBucketsCTE resolves the net-input / output token split per
// project_root_hash, deduped exactly like spendCTE (proxy api_turns win;
// each branch LEFT JOINs sessions so a proxy row whose own hash is empty
// still inherits the session's hash). Binds spendArgs(s).
const projectBucketsCTE = `
WITH proxy_ids AS (
    SELECT request_id FROM api_turns
     WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ?
),
pb AS (
    SELECT COALESCE(NULLIF(t.project_root_hash,''), NULLIF(s.project_root_hash,''), '') AS prh,
           t.user_id                       AS user_id,
           COALESCE(t.input_tokens,0)      AS net_input,
           COALESCE(t.output_tokens,0)     AS output
      FROM api_turns t
      LEFT JOIN sessions s ON s.id = t.session_id AND s.user_id = t.user_id
     WHERE t.timestamp >= ?
    UNION ALL
    SELECT COALESCE(NULLIF(tu.project_root_hash,''), NULLIF(s.project_root_hash,''), '') AS prh,
           tu.user_id                      AS user_id,
           COALESCE(tu.input_tokens,0)     AS net_input,
           COALESCE(tu.output_tokens,0)    AS output
      FROM token_usage tu
      LEFT JOIN sessions s ON s.id = tu.session_id AND s.user_id = tu.user_id
     WHERE tu.timestamp >= ?
       AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
            OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_ids))
)`

// projectBuckets fills the net-input / output token split per project for the
// per-row mini-bar, scoped, for projects in byRoot. Leaves Buckets nil when a
// project produced no tokens (honest empty).
func projectBuckets(ctx context.Context, db *sql.DB, s string, uScope string, uArgs []any, byRoot map[string]*ProjectRollup) error {
	//nolint:gosec // G202: projectBucketsCTE is a code constant and uScope is a parameterized scope fragment; values bind via ?.
	q := projectBucketsCTE + `
SELECT prh, COALESCE(SUM(net_input),0), COALESCE(SUM(output),0)
  FROM pb WHERE prh != '' AND ` + uScope + `
 GROUP BY prh`
	return eachRow(ctx, db, q, append(spendArgs(s), uArgs...), func(rows *sql.Rows) error {
		var hash string
		var netInput, output int64
		if err := rows.Scan(&hash, &netInput, &output); err != nil {
			return err
		}
		p, ok := byRoot[hash]
		if !ok {
			return nil
		}
		if netInput != 0 || output != 0 {
			p.Buckets = &TokenBuckets{NetInput: netInput, Output: output}
		}
		return nil
	})
}

// ProjectDetail computes the detail for one project (identified by its id hash),
// scope-restricted. found is false when no in-scope project hashes to id.
func ProjectDetail(ctx context.Context, db *sql.DB, w Window, projectID string, scope Scope, now time.Time) (res ProjectDetailResult, found bool, err error) {
	s := since(w, now)
	uScope, uArgs := userScopeSQL("user_id", scope)

	hash, ok, err := projectRootByID(ctx, db, s, projectID, uScope, uArgs)
	if err != nil {
		return ProjectDetailResult{}, false, fmt.Errorf("rollup.ProjectDetail: resolve: %w", err)
	}
	if !ok {
		return ProjectDetailResult{}, false, nil
	}

	res = ProjectDetailResult{
		ProjectID:   projectID,
		ProjectRoot: hash,
		WindowDays:  w.days(),
		Teams:       []TeamRef{},
		Tools:       []string{},
		CostByDay:   []CostPoint{},
		TopModels:   []ModelSpend{},
	}

	// Cost + active developers for this project, scoped. The project
	// dimension is the project_root_hash (see spendCTE for the JOIN-
	// fallback that resolves it for proxy-fed api_turns).
	projScope := "project_root_hash = ? AND " + uScope
	projArgs := append([]any{hash}, uArgs...)
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
 WHERE started_at >= ? AND project_root_hash = ? AND `+uScope+`
 GROUP BY tool`, appendArgs([]any{s, hash}, uArgs...)...)
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
	res.Teams = teamSet[hash]
	if res.Teams == nil {
		res.Teams = []TeamRef{}
	}

	return res, true, nil
}

// projectTeams returns, per project_root_hash, the set of teams whose members
// touched it within the window, applying the given spend scope clause.
func projectTeams(ctx context.Context, db *sql.DB, s, scopeSQL string, scopeArgs []any) (map[string][]TeamRef, error) {
	//nolint:gosec // G202: spendCTE and JOIN/WHERE fragments are code constants and scopeSQL is a parameterized scope fragment; all values are bound via ? args.
	q := spendCTE + `
SELECT DISTINCT sp.project_root_hash, t.team_id, t.display_name
  FROM spend sp
  JOIN org_team_members m ON m.user_id = sp.user_id
  JOIN org_teams t        ON t.team_id = m.team_id
 WHERE sp.project_root_hash != '' AND ` + scopeSQL + `
 ORDER BY sp.project_root_hash, t.display_name`
	rows, err := db.QueryContext(ctx, q, appendArgs(spendArgs(s), scopeArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]TeamRef{}
	for rows.Next() {
		var hash string
		var ref TeamRef
		if err := rows.Scan(&hash, &ref.TeamID, &ref.DisplayName); err != nil {
			return nil, err
		}
		out[hash] = append(out[hash], ref)
	}
	return out, rows.Err()
}

// projectRootByID scans the distinct in-scope project_root_hashes in the
// window and returns the hash whose first 16 chars equal id (ProjectIDFromHash
// is just a prefix slice, so this is a deterministic reverse — the URL
// {id} → spend.project_root_hash mapping round-trips without recovering the
// raw path).
func projectRootByID(ctx context.Context, db *sql.DB, s, id, uScope string, uArgs []any) (string, bool, error) {
	rows, err := db.QueryContext(ctx, spendCTE+`
SELECT DISTINCT project_root_hash FROM spend WHERE project_root_hash != '' AND `+uScope,
		append(spendArgs(s), uArgs...)...)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return "", false, err
		}
		if ProjectIDFromHash(hash) == id {
			return hash, true, nil
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
