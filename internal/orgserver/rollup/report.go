package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// KeyCost is one (dimension key → cost) row in a report breakdown. For the
// project breakdown the key is the hash-derived ProjectID, never a raw path.
type KeyCost struct {
	Key     string  `json:"key"`
	CostUSD float64 `json:"cost_usd"`
	Tokens  int64   `json:"tokens"`
}

// ReportSession is one expensive session in the monthly statement (content-free).
type ReportSession struct {
	SessionID string  `json:"session_id"`
	Email     string  `json:"email,omitempty"`
	Tool      string  `json:"tool,omitempty"`
	CostUSD   float64 `json:"cost_usd"`
}

// ReportResult powers GET /api/org/report — a print-friendly monthly cost
// statement for chargeback. Spend by model / tool / project + the most
// expensive sessions, for one calendar month. Scoped (admin/lead). Content-free.
type ReportResult struct {
	Month       string          `json:"month"` // YYYY-MM
	TotalUSD    float64         `json:"total_usd"`
	ByModel     []KeyCost       `json:"by_model"`
	ByTool      []KeyCost       `json:"by_tool"`
	ByProject   []KeyCost       `json:"by_project"`
	TopSessions []ReportSession `json:"top_sessions"`
}

// Report builds the monthly statement for the given month (YYYY-MM; defaults to
// the current UTC month when empty/unparseable), scoped to the caller.
func Report(ctx context.Context, db *sql.DB, scope Scope, month string, now time.Time) (ReportResult, error) {
	start, end, label := monthBounds(month, now)
	res := ReportResult{
		Month:       label,
		ByModel:     []KeyCost{},
		ByTool:      []KeyCost{},
		ByProject:   []KeyCost{},
		TopSessions: []ReportSession{},
	}
	uScope, uArgs := userScopeSQL("user_id", scope)

	byModel, err := reportDim(ctx, db, "model", start, end, uScope, uArgs, false)
	if err != nil {
		return ReportResult{}, fmt.Errorf("rollup.Report: by model: %w", err)
	}
	res.ByModel = byModel
	byTool, err := reportDim(ctx, db, "tool", start, end, uScope, uArgs, false)
	if err != nil {
		return ReportResult{}, fmt.Errorf("rollup.Report: by tool: %w", err)
	}
	res.ByTool = byTool
	byProj, err := reportDim(ctx, db, "project_root_hash", start, end, uScope, uArgs, true)
	if err != nil {
		return ReportResult{}, fmt.Errorf("rollup.Report: by project: %w", err)
	}
	res.ByProject = byProj
	for _, m := range byModel {
		res.TotalUSD += m.CostUSD
	}

	top, err := reportTopSessions(ctx, db, start, end, uScope, uArgs)
	if err != nil {
		return ReportResult{}, fmt.Errorf("rollup.Report: top sessions: %w", err)
	}
	res.TopSessions = top
	return res, nil
}

// reportDim sums spend + tokens by a spend-CTE column within [start, end).
func reportDim(ctx context.Context, db *sql.DB, col, start, end, uScope string, uArgs []any, isProject bool) ([]KeyCost, error) {
	//nolint:gosec // G202: spendCTE is a code constant; col is code-constant; uScope parameterized; values bind via ?.
	q := spendCTE + `
SELECT ` + col + `, COALESCE(SUM(cost),0), COALESCE(SUM(tokens),0)
  FROM spend
 WHERE ` + col + ` != '' AND ts < ? AND ` + uScope + `
 GROUP BY ` + col + `
 ORDER BY 2 DESC`
	args := append(append(spendArgs(start), end), uArgs...)
	out := []KeyCost{}
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var k KeyCost
		if err := rows.Scan(&k.Key, &k.CostUSD, &k.Tokens); err != nil {
			return err
		}
		if isProject {
			k.Key = ProjectIDFromHash(k.Key)
		}
		out = append(out, k)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// reportTopSessions ranks the month's sessions by cost (top 25).
func reportTopSessions(ctx context.Context, db *sql.DB, start, end, uScope string, uArgs []any) ([]ReportSession, error) {
	//nolint:gosec // G202: sessionSpendCTE is a code constant; uScope parameterized; values bind via ?.
	q := sessionSpendCTE + `
SELECT sid, COALESCE(SUM(cost),0) c FROM sspend
 WHERE sid != '' AND ts < ? AND ` + uScope + `
 GROUP BY sid ORDER BY c DESC LIMIT 25`
	args := append(append(spendArgs(start), end), uArgs...)
	out := []ReportSession{}
	ids := []string{}
	cost := map[string]float64{}
	if err := eachRow(ctx, db, q, args, func(rows *sql.Rows) error {
		var sid string
		var c float64
		if err := rows.Scan(&sid, &c); err != nil {
			return err
		}
		ids = append(ids, sid)
		cost[sid] = c
		return nil
	}); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	// Resolve session email/tool for the top set.
	args2 := make([]any, len(ids))
	for i, id := range ids {
		args2[i] = id
	}
	//nolint:gosec // G201: only a ?-placeholder list is interpolated; ids bind via ?.
	mq := `SELECT s.id, COALESCE(m.email, s.user_email, ''), COALESCE(s.tool,'')
             FROM sessions s LEFT JOIN org_members m ON m.user_id = s.user_id
            WHERE s.id IN (` + placeholders(len(ids)) + `)`
	meta := map[string][2]string{}
	if err := eachRow(ctx, db, mq, args2, func(rows *sql.Rows) error {
		var id, email, tool string
		if err := rows.Scan(&id, &email, &tool); err != nil {
			return err
		}
		meta[id] = [2]string{email, tool}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, id := range ids {
		out = append(out, ReportSession{SessionID: id, Email: meta[id][0], Tool: meta[id][1], CostUSD: cost[id]})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
	return out, nil
}

// monthBounds returns the RFC3339 [start, end) bounds + the YYYY-MM label for a
// month string. Empty/invalid → the current UTC month.
func monthBounds(month string, now time.Time) (start, end, label string) {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		t = time.Date(now.UTC().Year(), now.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	} else {
		t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	next := t.AddDate(0, 1, 0)
	return t.Format(time.RFC3339), next.Format(time.RFC3339), t.Format("2006-01")
}
