// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"testing"
)

// TestObsEndUserSpend_CrossInstance seeds obs_enduser_spend across two nodes
// (distinct user_email, same end_user) and asserts the rollup SUMs across
// nodes, ranks by cost, filters the window, and excludes empty end-users.
func TestObsEndUserSpend_CrossInstance(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	ins := func(userEmail, day, endUser string, cost float64, traces, tokens int64) {
		t.Helper()
		if _, err := d.ExecContext(ctx,
			`INSERT INTO obs_enduser_spend
			   (org_id, user_email, day, end_user, cost_usd, traces, total_tokens, pushed_at, pushed_by_user_id)
			 VALUES ('org1', ?, ?, ?, ?, ?, ?, '2026-05-26T06:00:00Z', 'u-node')`,
			userEmail, day, endUser, cost, traces, tokens); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// cust-42 observed by two nodes in-window → SUM to 3.0 / 5 traces / 300 tokens.
	ins("nodeA@x", "2026-05-20", "cust-42", 1.0, 2, 100)
	ins("nodeB@x", "2026-05-20", "cust-42", 2.0, 3, 200)
	// cust-99 one node, in-window.
	ins("nodeA@x", "2026-05-21", "cust-99", 0.5, 1, 40)
	// out-of-window cust-42 spend — excluded.
	ins("nodeA@x", "2026-01-01", "cust-42", 99.0, 9, 9000)
	// empty end_user — excluded (unattributed).
	ins("nodeA@x", "2026-05-20", "", 5.0, 1, 10)

	res, err := ObsEndUserSpend(ctx, d, w30, fixedNow)
	if err != nil {
		t.Fatalf("ObsEndUserSpend: %v", err)
	}
	if !res.Configured || res.WindowDays != 30 {
		t.Fatalf("configured=%v window=%d", res.Configured, res.WindowDays)
	}
	if !near(res.TotalCostUSD, 3.5) {
		t.Errorf("total cost = %v, want 3.5 (out-of-window + empty-user excluded)", res.TotalCostUSD)
	}
	if len(res.Users) != 2 {
		t.Fatalf("users = %+v, want 2 (cust-42, cust-99)", res.Users)
	}
	// Ranked by cost: cust-42 (3.0) leads cust-99 (0.5).
	top := res.Users[0]
	if top.EndUser != "cust-42" || !near(top.CostUSD, 3.0) || top.Traces != 5 || top.TotalTokens != 300 {
		t.Errorf("top = %+v, want cust-42 cost 3.0 / traces 5 / tokens 300", top)
	}
	if !near(top.CostShare, 3.0/3.5) {
		t.Errorf("cust-42 share = %v, want %v", top.CostShare, 3.0/3.5)
	}
	if res.Users[1].EndUser != "cust-99" {
		t.Errorf("second = %q, want cust-99", res.Users[1].EndUser)
	}
}
