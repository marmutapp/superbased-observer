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

// obsenduser.go is the T5 per-END-USER spend attribution surface (org-budget
// guardrails plan §2.1): who among the hosted-app's END-USERS is spending, over
// the obs_enduser_spend aggregate. DISTINCT from obscost.go (which attributes
// to the DEVELOPER/project/model over obs_summaries) — this attributes to the
// app's end-user identity, and is CROSS-INSTANCE: obs_enduser_spend carries one
// row per (node user_email, day, end_user), so summing across user_email yields
// the total spend for an end_user regardless of which node observed it.
// Admin-only, alert/report posture (the org can observe; the node chokepoint
// enforces).

// ObsEndUserBucket is one end-user's attributed spend.
type ObsEndUserBucket struct {
	EndUser     string  `json:"end_user"`
	CostUSD     float64 `json:"cost_usd"`
	Traces      int64   `json:"traces"`
	TotalTokens int64   `json:"total_tokens"`
	CostShare   float64 `json:"cost_share"` // fraction of total cost
}

// ObsEndUserSpendResult is the GET /api/org/obs/enduser-spend body.
type ObsEndUserSpendResult struct {
	WindowDays   int                `json:"window_days"`
	Configured   bool               `json:"configured"`
	TotalCostUSD float64            `json:"total_cost_usd"`
	Users        []ObsEndUserBucket `json:"users"`
}

// ObsEndUserSpend aggregates obs_enduser_spend by end_user across every node
// (SUM over user_email = cross-instance total). Single-org-per-server
// convention (no org param), matching rollup.ObsCost.
func ObsEndUserSpend(ctx context.Context, db *sql.DB, w Window, now time.Time) (ObsEndUserSpendResult, error) {
	sinceDay := now.UTC().AddDate(0, 0, -w.days()).Format("2006-01-02")
	res := ObsEndUserSpendResult{
		WindowDays: w.days(),
		Users:      []ObsEndUserBucket{},
	}
	byUser := map[string]*ObsEndUserBucket{}

	q := `
SELECT end_user, COALESCE(SUM(cost_usd),0), COALESCE(SUM(traces),0), COALESCE(SUM(total_tokens),0)
  FROM obs_enduser_spend
 WHERE day >= ? AND end_user != ''
 GROUP BY end_user`
	if err := eachRow(ctx, db, q, []any{sinceDay}, func(rows *sql.Rows) error {
		var eu string
		var cost float64
		var traces, tokens int64
		if err := rows.Scan(&eu, &cost, &traces, &tokens); err != nil {
			return err
		}
		res.TotalCostUSD += cost
		b := byUser[eu]
		if b == nil {
			b = &ObsEndUserBucket{EndUser: eu}
			byUser[eu] = b
		}
		b.CostUSD += cost
		b.Traces += traces
		b.TotalTokens += tokens
		return nil
	}); err != nil {
		return ObsEndUserSpendResult{}, fmt.Errorf("rollup.ObsEndUserSpend: %w", err)
	}
	res.Configured = res.TotalCostUSD > 0 || len(byUser) > 0

	res.Users = make([]ObsEndUserBucket, 0, len(byUser))
	for _, b := range byUser {
		if res.TotalCostUSD > 0 {
			b.CostShare = b.CostUSD / res.TotalCostUSD
		}
		res.Users = append(res.Users, *b)
	}
	sort.SliceStable(res.Users, func(i, j int) bool {
		if res.Users[i].CostUSD != res.Users[j].CostUSD {
			return res.Users[i].CostUSD > res.Users[j].CostUSD
		}
		return res.Users[i].EndUser < res.Users[j].EndUser
	})
	return res, nil
}
