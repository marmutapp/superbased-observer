// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// admEvent is one seeded obs_admission_events row for the rollup tests.
type admEvent struct {
	userEmail, pushedBy        string
	ts, decision, criterion    string
	prev, row, endUser, reason string
	judgeUsed                  bool
	judgeHosting               string
}

func seedAdmEvent(t *testing.T, d *sql.DB, e admEvent) {
	t.Helper()
	judge := 0
	if e.judgeUsed {
		judge = 1
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO obs_admission_events
		   (org_id, user_email, row_hash, prev_hash, ts, mode, decision, severity,
		    criterion_id, policy_hash, judge_used, judge_hosting, degraded, latency_ms,
		    trace_id, session_id, request_id, message_hash, tenant, end_user, reason_excerpt,
		    pushed_at, pushed_by_user_id)
		 VALUES ('org1', ?, ?, ?, ?, 'enforce', ?, 'high', ?, 'p-new', ?, ?, '', 10,
		         'tr', 'se', ?, 'mh', NULL, ?, ?, '2026-05-26T11:00:00Z', ?)`,
		e.userEmail, e.row, e.prev, e.ts, e.decision, e.criterion, judge, e.judgeHosting,
		e.row /*request_id reuse*/, nullString(e.endUser), nullString(e.reason), e.pushedBy); err != nil {
		t.Fatalf("seed event %s: %v", e.row, err)
	}
}

func seedAdmPolicy(t *testing.T, d *sql.DB, hash, userEmail, createdAt, mode, scope, body string, criteria int64) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO obs_admission_policy_versions
		   (org_id, user_email, policy_hash, created_at, mode, scope, criteria_count, body,
		    pushed_at, pushed_by_user_id)
		 VALUES ('org1', ?, ?, ?, ?, ?, ?, ?, '2026-05-26T11:00:00Z', 'u-a')`,
		userEmail, hash, createdAt, mode, scope, criteria, body); err != nil {
		t.Fatalf("seed policy %s: %v", hash, err)
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// TestObsAdmission_PostureAndMapping seeds one intact node whose oldest
// in-window row dangles (prev references pre-window history) and asserts:
// posture (mode/criteria from newest policy, judge_hosting from newest judged
// verdict), the 24h ask+deny→would_block fold, raw decision verbatim in the
// verdict list, the per-end-user overlay, and the tolerant chain-head rule.
func TestObsAdmission_PostureAndMapping(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	// Two policy versions; the newest (by created_at) fixes the posture.
	seedAdmPolicy(t, d, "p-old", "nodeA@x", "2026-05-20T09:00:00Z", "observe", "global", `{"old":1}`, 2)
	seedAdmPolicy(t, d, "p-new", "nodeA@x", "2026-05-26T05:00:00Z", "enforce", "tenant:acme", `{"new":1}`, 5)

	// Node A chain: A0 dangles (prev references pre-window history) → the single
	// tolerated boundary; A1..A4 link cleanly. A0 is in the 30d window but not
	// the 24h slice.
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: "2026-05-20T10:00:00Z", decision: "allow", prev: "pre-hist", row: "a0", endUser: "cust-1"})
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: "2026-05-26T06:00:00Z", decision: "allow", prev: "a0", row: "a1", endUser: "cust-1"})
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: "2026-05-26T07:00:00Z", decision: "flag", prev: "a1", row: "a2", endUser: "cust-1", reason: "flag reason"})
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: "2026-05-26T08:00:00Z", decision: "deny", prev: "a2", row: "a3", endUser: "cust-2", judgeUsed: true, judgeHosting: "provider", reason: "deny reason"})
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: "2026-05-26T09:00:00Z", decision: "ask", prev: "a3", row: "a4", endUser: "cust-2"})

	res, err := ObsAdmission(ctx, d, w30, adminScope, "u-a", fixedNow)
	if err != nil {
		t.Fatalf("ObsAdmission: %v", err)
	}
	if !res.Configured || res.WindowDays != 30 {
		t.Fatalf("configured=%v window=%d", res.Configured, res.WindowDays)
	}
	// Posture from newest policy + newest judged verdict.
	if res.Mode != "enforce" || res.CriteriaCount != 5 {
		t.Errorf("posture = (mode=%q,criteria=%d), want (enforce,5) from newest policy", res.Mode, res.CriteriaCount)
	}
	if res.JudgeHosting != "provider" {
		t.Errorf("judge_hosting = %q, want provider (newest judged verdict)", res.JudgeHosting)
	}
	// 24h counts: A1 allow, A2 flag, A3 deny + A4 ask → would_block 2. A0 excluded.
	if c := res.Verdicts24h; c.Allow != 1 || c.Flag != 1 || c.WouldBlock != 2 {
		t.Errorf("verdicts_24h = %+v, want allow1 flag1 would_block2 (ask+deny)", c)
	}
	// verdicts[] over 30d, newest first, RAW decision verbatim (ask NOT folded).
	if len(res.Verdicts) != 5 {
		t.Fatalf("verdicts = %d, want 5", len(res.Verdicts))
	}
	if res.Verdicts[0].Decision != "ask" || res.Verdicts[1].Decision != "deny" {
		t.Errorf("verdicts[0..1] decisions = %q,%q, want ask,deny (raw verbatim)", res.Verdicts[0].Decision, res.Verdicts[1].Decision)
	}
	if res.Verdicts[0].EndUser != "cust-2" {
		t.Errorf("verdicts[0].end_user = %q, want cust-2", res.Verdicts[0].EndUser)
	}
	// Chain: single node, tolerant head → intact.
	if !res.Chain.OK || len(res.Chain.Nodes) != 1 || !res.Chain.Nodes[0].OK || res.Chain.Nodes[0].Rows != 5 {
		t.Errorf("chain = %+v, want ok single node 5 rows", res.Chain)
	}
	// Overlay: cust-2 (deny+ask→2), cust-1 (flag 1); ranked by would_block.
	if len(res.WouldBlockByUser) != 2 {
		t.Fatalf("would_block_by_user = %+v, want 2", res.WouldBlockByUser)
	}
	if res.WouldBlockByUser[0].EndUser != "cust-2" || res.WouldBlockByUser[0].WouldBlock != 2 {
		t.Errorf("overlay[0] = %+v, want cust-2 would_block 2", res.WouldBlockByUser[0])
	}
	if res.WouldBlockByUser[1].EndUser != "cust-1" || res.WouldBlockByUser[1].Flag != 1 || res.WouldBlockByUser[1].WouldBlock != 0 {
		t.Errorf("overlay[1] = %+v, want cust-1 flag 1 would_block 0", res.WouldBlockByUser[1])
	}
}

// TestObsAdmission_BrokenChain seeds a node whose events split into two
// segments (a head plus a row whose prev is absent from the window set) and
// asserts the node — and the fleet chain — report broken.
func TestObsAdmission_BrokenChain(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	seedAdmEvent(t, d, admEvent{userEmail: "nodeB@x", pushedBy: "u-b", ts: "2026-05-26T06:00:00Z", decision: "allow", prev: "", row: "b1"})
	seedAdmEvent(t, d, admEvent{userEmail: "nodeB@x", pushedBy: "u-b", ts: "2026-05-26T07:00:00Z", decision: "allow", prev: "missing-xyz", row: "b2"})

	res, err := ObsAdmission(ctx, d, w30, adminScope, "u-b", fixedNow)
	if err != nil {
		t.Fatalf("ObsAdmission: %v", err)
	}
	if res.Chain.OK {
		t.Errorf("chain.ok = true, want false (2 segments)")
	}
	if len(res.Chain.Nodes) != 1 || res.Chain.Nodes[0].OK {
		t.Errorf("chain node = %+v, want single broken node", res.Chain.Nodes)
	}
}

// TestObsAdmission_EmptyNotConfigured confirms an empty DB reports
// configured=false without error.
func TestObsAdmission_EmptyNotConfigured(t *testing.T) {
	d := newDB(t)
	res, err := ObsAdmission(context.Background(), d, w30, adminScope, "u-a", fixedNow)
	if err != nil {
		t.Fatalf("ObsAdmission: %v", err)
	}
	if res.Configured {
		t.Errorf("configured=true on empty DB, want false")
	}
}

// TestObsAdmissionReasons_OnlyNonEmpty confirms only non-empty reason excerpts
// (from full-content nodes) are returned.
func TestObsAdmissionReasons_OnlyNonEmpty(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	// ObsAdmissionReasons windows against real time.Now() (no now param), so seed
	// with recent timestamps rather than the fixedNow era.
	now := time.Now().UTC()
	ts1 := now.Add(-2 * time.Hour).Format(time.RFC3339)
	ts2 := now.Add(-1 * time.Hour).Format(time.RFC3339)
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: ts1, decision: "deny", prev: "", row: "a1", reason: "quoted request text"})
	seedAdmEvent(t, d, admEvent{userEmail: "nodeA@x", pushedBy: "u-a", ts: ts2, decision: "allow", prev: "a1", row: "a2"}) // no reason

	res, err := ObsAdmissionReasons(ctx, d, w30, adminScope, "u-a")
	if err != nil {
		t.Fatalf("ObsAdmissionReasons: %v", err)
	}
	if len(res.Reasons) != 1 || res.Reasons[0].ReasonExcerpt != "quoted request text" {
		t.Fatalf("reasons = %+v, want 1 non-empty excerpt", res.Reasons)
	}
}
