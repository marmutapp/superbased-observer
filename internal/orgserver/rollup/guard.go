package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// Guard rollups (guard spec §14.3, G14): verdict trends, rule-hit
// leaderboards, per-team posture, and audit-chain continuity over the
// server-side guard_events table (migration 005). Every query reads only
// content-free columns — the full_content opt-in columns (reason,
// target_excerpt, taint_origin) are never selected here.

// decisionBucketsSQL is the shared per-decision aggregate fragment. SQLite
// boolean expressions evaluate to 0/1, so SUM(decision='deny') counts the
// bucket; the COALESCE guards the zero-row group.
const decisionBucketsSQL = `
	COALESCE(SUM(decision='deny'),0), COALESCE(SUM(decision='ask'),0),
	COALESCE(SUM(decision='flag'),0), COALESCE(SUM(decision='mask'),0)`

// chainSegmentsCTE computes each in-scope user's chain-segment count over
// their FULL pushed history (deliberately unwindowed — see GuardAgentChain):
// segments = heads (empty chain_prev) + unlinked (chain_prev absent from the
// user's own chain_hash set). It binds no args itself; the caller appends a
// user-scope fragment via uScope on g.user_id.
const chainSegmentsCTE = `
chain AS (
    SELECT g.user_id,
           COUNT(*) AS events,
           SUM(CASE WHEN g.chain_prev IS NULL OR g.chain_prev = '' THEN 1 ELSE 0 END) AS heads,
           SUM(CASE WHEN g.chain_prev IS NOT NULL AND g.chain_prev != ''
                     AND NOT EXISTS (SELECT 1 FROM guard_events p
                                      WHERE p.user_id = g.user_id AND p.chain_hash = g.chain_prev)
                    THEN 1 ELSE 0 END) AS unlinked,
           MIN(g.timestamp) AS first_seen,
           MAX(g.timestamp) AS last_seen
      FROM guard_events g
     GROUP BY g.user_id
)`

// GuardOverview computes the org-wide (admin / org-wide guard reader) or
// led-teams (lead) guard overview for the trailing window.
func GuardOverview(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (GuardOverviewResult, error) {
	s := since(w, now)
	res := GuardOverviewResult{
		WindowDays: w.days(),
		TrendByDay: []GuardTrendPoint{},
		TopRules:   []GuardRuleHit{},
	}
	uScope, uArgs := userScopeSQL("user_id", scope)

	//nolint:gosec // G202: SQL structure is code-constant; uScope is a parameterized scope fragment, values bind via ?.
	totalsQ := `
SELECT COUNT(*), ` + decisionBucketsSQL + `,
       COALESCE(SUM(COALESCE(enforced,0)),0),
       COUNT(DISTINCT user_id),
       COUNT(DISTINCT CASE WHEN rule_id != '' THEN rule_id END)
  FROM guard_events
 WHERE timestamp >= ? AND ` + uScope
	if err := db.QueryRowContext(ctx, totalsQ, append([]any{s}, uArgs...)...).Scan(
		&res.TotalEvents, &res.DenyCount, &res.AskCount, &res.FlagCount, &res.MaskCount,
		&res.EnforcedCount, &res.ActiveAgents, &res.RuleCount,
	); err != nil {
		return GuardOverviewResult{}, fmt.Errorf("rollup.GuardOverview: totals: %w", err)
	}

	// Broken chains: users in scope whose full history splits into more than
	// one chain segment (unwindowed by design).
	gScope, gArgs := userScopeSQL("g.user_id", scope)
	//nolint:gosec // G202: code-constant CTE + parameterized scope fragment.
	brokenQ := `WITH ` + chainSegmentsCTE + `
SELECT COUNT(*) FROM chain c
 WHERE c.heads + c.unlinked > 1
   AND c.user_id IN (SELECT g.user_id FROM guard_events g WHERE ` + gScope + `)`
	if err := db.QueryRowContext(ctx, brokenQ, gArgs...).Scan(&res.BrokenChainAgents); err != nil {
		return GuardOverviewResult{}, fmt.Errorf("rollup.GuardOverview: broken chains: %w", err)
	}

	// Per-day trend.
	//nolint:gosec // G202: code-constant SQL + parameterized scope fragment.
	trendQ := `
SELECT substr(timestamp,1,10) AS day, ` + decisionBucketsSQL + `,
       COALESCE(SUM(decision NOT IN ('deny','ask','flag','mask')),0), COUNT(*)
  FROM guard_events
 WHERE timestamp >= ? AND ` + uScope + `
 GROUP BY day
 ORDER BY day`
	rows, err := db.QueryContext(ctx, trendQ, append([]any{s}, uArgs...)...)
	if err != nil {
		return GuardOverviewResult{}, fmt.Errorf("rollup.GuardOverview: trend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p GuardTrendPoint
		if err := rows.Scan(&p.Date, &p.Deny, &p.Ask, &p.Flag, &p.Mask, &p.Other, &p.Total); err != nil {
			return GuardOverviewResult{}, fmt.Errorf("rollup.GuardOverview: trend scan: %w", err)
		}
		res.TrendByDay = append(res.TrendByDay, p)
	}
	if err := rows.Err(); err != nil {
		return GuardOverviewResult{}, fmt.Errorf("rollup.GuardOverview: trend rows: %w", err)
	}

	if res.TopRules, err = guardRuleHits(ctx, db, s, uScope, uArgs, topN); err != nil {
		return GuardOverviewResult{}, fmt.Errorf("rollup.GuardOverview: top rules: %w", err)
	}
	return res, nil
}

// GuardRules computes the full rule-hit leaderboard for the window.
func GuardRules(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (GuardRulesResult, error) {
	uScope, uArgs := userScopeSQL("user_id", scope)
	hits, err := guardRuleHits(ctx, db, since(w, now), uScope, uArgs, 0)
	if err != nil {
		return GuardRulesResult{}, fmt.Errorf("rollup.GuardRules: %w", err)
	}
	return GuardRulesResult{WindowDays: w.days(), Rules: hits}, nil
}

// guardRuleHits runs the leaderboard query; limit 0 means no LIMIT.
func guardRuleHits(ctx context.Context, db *sql.DB, s, uScope string, uArgs []any, limit int) ([]GuardRuleHit, error) {
	//nolint:gosec // G202: code-constant SQL + parameterized scope fragment.
	q := `
SELECT rule_id, COALESCE(MAX(category),''), COALESCE(MAX(severity),''),
       COUNT(*), COUNT(DISTINCT user_id), COALESCE(SUM(decision='deny'),0),
       COALESCE(MAX(timestamp),'')
  FROM guard_events
 WHERE timestamp >= ? AND rule_id != '' AND ` + uScope + `
 GROUP BY rule_id
 ORDER BY COUNT(*) DESC, rule_id`
	args := append([]any{s}, uArgs...)
	if limit > 0 {
		q += `
 LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GuardRuleHit{}
	for rows.Next() {
		var h GuardRuleHit
		if err := rows.Scan(&h.RuleID, &h.Category, &h.Severity, &h.Hits, &h.Agents, &h.DenyCount, &h.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GuardTeams computes the per-team posture rollup. A user belonging to two
// teams contributes their events to both rows (posture is per team, not a
// partition). MemberCount comes from a correlated subquery so the event join
// cannot inflate it.
func GuardTeams(ctx context.Context, db *sql.DB, w Window, scope Scope, now time.Time) (GuardTeamsResult, error) {
	s := since(w, now)
	res := GuardTeamsResult{WindowDays: w.days(), Teams: []GuardTeamPosture{}}
	tScope, tArgs := teamScopeSQL("t.team_id", scope)

	//nolint:gosec // G202: code-constant SQL + parameterized scope fragment.
	q := `
SELECT t.team_id, t.display_name,
       (SELECT COUNT(*) FROM org_team_members m2 WHERE m2.team_id = t.team_id),
       COUNT(DISTINCT g.user_id),
       COUNT(g.chain_hash),
       COALESCE(SUM(g.decision='deny'),0), COALESCE(SUM(g.decision='ask'),0),
       COALESCE(SUM(g.decision='flag'),0), COALESCE(SUM(g.decision='mask'),0),
       COALESCE(SUM(COALESCE(g.enforced,0)),0)
  FROM org_teams t
  JOIN org_team_members m ON m.team_id = t.team_id
  LEFT JOIN guard_events g ON g.user_id = m.user_id AND g.timestamp >= ?
 WHERE ` + tScope + `
 GROUP BY t.team_id, t.display_name
 ORDER BY t.display_name, t.team_id`
	rows, err := db.QueryContext(ctx, q, append([]any{s}, tArgs...)...)
	if err != nil {
		return GuardTeamsResult{}, fmt.Errorf("rollup.GuardTeams: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p GuardTeamPosture
		var enforced int64
		if err := rows.Scan(&p.TeamID, &p.DisplayName, &p.MemberCount, &p.ActiveAgents, &p.Events,
			&p.DenyCount, &p.AskCount, &p.FlagCount, &p.MaskCount, &enforced); err != nil {
			return GuardTeamsResult{}, fmt.Errorf("rollup.GuardTeams: scan: %w", err)
		}
		if p.Events > 0 {
			p.EnforcedShare = float64(enforced) / float64(p.Events)
		}
		res.Teams = append(res.Teams, p)
	}
	if err := rows.Err(); err != nil {
		return GuardTeamsResult{}, fmt.Errorf("rollup.GuardTeams: rows: %w", err)
	}

	// Broken-chain agents per team (unwindowed, like GuardOverview).
	brokenQ := `WITH ` + chainSegmentsCTE + `
SELECT m.team_id, COUNT(*)
  FROM chain c
  JOIN org_team_members m ON m.user_id = c.user_id
 WHERE c.heads + c.unlinked > 1
 GROUP BY m.team_id`
	bRows, err := db.QueryContext(ctx, brokenQ)
	if err != nil {
		return GuardTeamsResult{}, fmt.Errorf("rollup.GuardTeams: broken chains: %w", err)
	}
	defer bRows.Close()
	broken := map[string]int64{}
	for bRows.Next() {
		var team string
		var n int64
		if err := bRows.Scan(&team, &n); err != nil {
			return GuardTeamsResult{}, fmt.Errorf("rollup.GuardTeams: broken scan: %w", err)
		}
		broken[team] = n
	}
	if err := bRows.Err(); err != nil {
		return GuardTeamsResult{}, fmt.Errorf("rollup.GuardTeams: broken rows: %w", err)
	}
	for i := range res.Teams {
		res.Teams[i].BrokenChainAgents = broken[res.Teams[i].TeamID]
	}
	return res, nil
}

// GuardAgents computes the per-developer audit-chain continuity report over
// each in-scope user's FULL pushed history. Broken (discontinuous) agents
// sort first, then by email. The caller is responsible for the audit-log
// write — per-developer rows are a privacy-sensitive disclosure.
func GuardAgents(ctx context.Context, db *sql.DB, scope Scope) (GuardAgentsResult, error) {
	res := GuardAgentsResult{Agents: []GuardAgentChain{}}
	cScope, cArgs := userScopeSQL("c.user_id", scope)

	//nolint:gosec // G202: code-constant CTE + parameterized scope fragment.
	q := `WITH ` + chainSegmentsCTE + `
SELECT c.user_id, COALESCE(om.email,''), COALESCE(om.display_name,''),
       c.events, c.heads, c.unlinked, c.first_seen, c.last_seen
  FROM chain c
  LEFT JOIN org_members om ON om.user_id = c.user_id
 WHERE ` + cScope
	rows, err := db.QueryContext(ctx, q, cArgs...)
	if err != nil {
		return GuardAgentsResult{}, fmt.Errorf("rollup.GuardAgents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a GuardAgentChain
		if err := rows.Scan(&a.UserID, &a.Email, &a.DisplayName,
			&a.Events, &a.Heads, &a.Unlinked, &a.FirstSeen, &a.LastSeen); err != nil {
			return GuardAgentsResult{}, fmt.Errorf("rollup.GuardAgents: scan: %w", err)
		}
		a.Segments = a.Heads + a.Unlinked
		a.Broken = a.Segments > 1
		res.Agents = append(res.Agents, a)
	}
	if err := rows.Err(); err != nil {
		return GuardAgentsResult{}, fmt.Errorf("rollup.GuardAgents: rows: %w", err)
	}
	sort.Slice(res.Agents, func(i, j int) bool {
		if res.Agents[i].Broken != res.Agents[j].Broken {
			return res.Agents[i].Broken
		}
		if res.Agents[i].Email != res.Agents[j].Email {
			return res.Agents[i].Email < res.Agents[j].Email
		}
		return res.Agents[i].UserID < res.Agents[j].UserID
	})
	return res, nil
}

// GuardRuleHitsForIDs computes the §14.2 dry-run statistic for a draft
// bundle's referenced rule ids: org-wide windowed hit counts per id
// (publishing is org-wide, so no scope applies). IDs absent from the pushed
// events return zero rows here; the caller fills those in (and decides
// Computable) — overrides of known rules are computable, newly declared
// matchers are not.
func GuardRuleHitsForIDs(ctx context.Context, db *sql.DB, w Window, ids []string, now time.Time) (map[string]GuardRuleDryRun, error) {
	out := map[string]GuardRuleDryRun{}
	if len(ids) == 0 {
		return out, nil
	}
	args := []any{since(w, now)}
	for _, id := range ids {
		args = append(args, id)
	}
	//nolint:gosec // G202: code-constant SQL; the IN list is ? placeholders, values bind via args.
	q := `
SELECT rule_id, COUNT(*), COUNT(DISTINCT user_id)
  FROM guard_events
 WHERE timestamp >= ? AND rule_id IN (` + placeholders(len(ids)) + `)
 GROUP BY rule_id`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup.GuardRuleHitsForIDs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d GuardRuleDryRun
		if err := rows.Scan(&d.RuleID, &d.Hits, &d.Agents); err != nil {
			return nil, fmt.Errorf("rollup.GuardRuleHitsForIDs: scan: %w", err)
		}
		out[d.RuleID] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollup.GuardRuleHitsForIDs: rows: %w", err)
	}
	return out, nil
}
