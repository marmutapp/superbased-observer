// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// obscost.go is the OP6 org observability cost-attribution surface (obs-org-tier
// plan §5.3 / §6): who and what is spending on custom-app / agent trajectories,
// over the content-free obs_summaries aggregate. It is DISTINCT from the
// api_turns-based budget/cost surfaces: obs cost includes non-proxied spans
// (tools, local models, third-party providers the proxy never saw), so it is a
// separate attribution view, NOT folded into the budget caps (which already
// count api_turns and would double-count proxy-routed spans). Admin-only.

// ObsCostBucket is one attribution row (a developer, project, or model).
type ObsCostBucket struct {
	Key       string  `json:"key"`   // email / project_hash / model id
	Label     string  `json:"label"` // display label (developer name, else key)
	CostUSD   float64 `json:"cost_usd"`
	Tokens    int64   `json:"tokens"`
	Traces    int64   `json:"traces"`
	CostShare float64 `json:"cost_share"` // fraction of total cost
}

// ObsCostResult is the GET /api/org/obs/cost body.
type ObsCostResult struct {
	WindowDays   int             `json:"window_days"`
	Configured   bool            `json:"configured"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	ByDeveloper  []ObsCostBucket `json:"by_developer"`
	ByProject    []ObsCostBucket `json:"by_project"`
	ByModel      []ObsCostBucket `json:"by_model"`
}

// ObsCost aggregates obs_summaries cost by developer / project / model.
func ObsCost(ctx context.Context, db *sql.DB, w Window, now time.Time) (ObsCostResult, error) {
	sinceDay := now.UTC().AddDate(0, 0, -w.days()).Format("2006-01-02")
	res := ObsCostResult{
		WindowDays:  w.days(),
		ByDeveloper: []ObsCostBucket{},
		ByProject:   []ObsCostBucket{},
		ByModel:     []ObsCostBucket{},
	}
	byDev := map[string]*ObsCostBucket{}
	byProj := map[string]*ObsCostBucket{}
	byModel := map[string]*ObsCostBucket{}

	q := `
SELECT COALESCE(user_email,''), COALESCE(project_hash,''), COALESCE(model,''),
       COALESCE(SUM(cost_usd),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(traces),0)
  FROM obs_summaries
 WHERE day >= ?
 GROUP BY user_email, project_hash, model`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(rows *sql.Rows) error {
		var email, project, model string
		var cost float64
		var tokens, traces int64
		if err := rows.Scan(&email, &project, &model, &cost, &tokens, &traces); err != nil {
			return err
		}
		res.TotalCostUSD += cost
		accumObsCost(byDev, email, cost, tokens, traces)
		accumObsCost(byProj, project, cost, tokens, traces)
		accumObsCost(byModel, model, cost, tokens, traces)
		return nil
	}); err != nil {
		return ObsCostResult{}, fmt.Errorf("rollup.ObsCost: %w", err)
	}
	res.Configured = res.TotalCostUSD > 0 || len(byDev) > 0

	// Developer label is the agent-stamped email (a readable identity already).
	res.ByDeveloper = finalizeObsCost(byDev, res.TotalCostUSD, func(key string) string {
		if key == "" {
			return "(unattributed)"
		}
		return key
	})
	res.ByProject = finalizeObsCost(byProj, res.TotalCostUSD, func(key string) string {
		if key == "" {
			return "(none)"
		}
		return ProjectIDFromHash(key)
	})
	res.ByModel = finalizeObsCost(byModel, res.TotalCostUSD, func(key string) string {
		if key == "" {
			return "(unknown)"
		}
		return key
	})
	return res, nil
}

func accumObsCost(m map[string]*ObsCostBucket, key string, cost float64, tokens, traces int64) {
	b := m[key]
	if b == nil {
		b = &ObsCostBucket{Key: key}
		m[key] = b
	}
	b.CostUSD += cost
	b.Tokens += tokens
	b.Traces += traces
}

func finalizeObsCost(m map[string]*ObsCostBucket, total float64, label func(string) string) []ObsCostBucket {
	out := make([]ObsCostBucket, 0, len(m))
	for _, b := range m {
		b.Label = label(b.Key)
		if total > 0 {
			b.CostShare = b.CostUSD / total
		}
		out = append(out, *b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CostUSD != out[j].CostUSD {
			return out[i].CostUSD > out[j].CostUSD
		}
		return out[i].Key < out[j].Key
	})
	return out
}
