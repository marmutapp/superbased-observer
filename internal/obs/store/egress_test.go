package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func sampleEgressRow(i int) EgressDecisionRow {
	return EgressDecisionRow{
		TS:              time.Now().UTC(),
		Mode:            "advise",
		RuleName:        "flagged-to-local",
		PolicyHash:      "pol-hash",
		Action:          "route_upstream",
		UpstreamID:      "ollama-local",
		TargetShape:     "openai",
		ModelFrom:       "claude-opus-4-8",
		ReasonCode:      "egress_flagged_local",
		MustUseTarget:   true,
		VerdictDecision: "flag",
		CriterionID:     "valid_use_case",
		MessageHash:     "msg-hash",
		RequestID:       "sbo-req",
		SessionID:       "sess",
		User:            "alice@example.com",
	}
}

func TestEgressInsertVerifyTamper(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	for i := 0; i < 5; i++ {
		if _, err := s.InsertEgressDecision(ctx, sampleEgressRow(i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	res, err := s.VerifyEgressChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK || res.Rows != 5 {
		t.Fatalf("chain not intact: %+v", res)
	}

	// Tamper with a middle row's stored user (content) and re-verify → broken.
	if _, err := s.db.ExecContext(ctx, `UPDATE obs_egress_decisions SET user = 'mallory' WHERE id = 3`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	res, err = s.VerifyEgressChain(ctx)
	if err != nil {
		t.Fatalf("verify after tamper: %v", err)
	}
	if res.OK || res.BreakAt != 3 {
		t.Fatalf("tamper not detected: %+v", res)
	}
}

// TestEgressChainNoForkUnderConcurrency drives parallel inserts and asserts the
// serialized chain (egressChainMu + UNIQUE(prev_hash)) never forks — every row
// links and VerifyEgressChain passes (design finding 15: the egress chain must
// NOT copy InsertAdmissionEvent's deferred-tx race).
func TestEgressChainNoForkUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	const n = 40
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.InsertEgressDecision(ctx, sampleEgressRow(i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent insert failed (chain fork or lock issue): %v", err)
	}
	res, err := s.VerifyEgressChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK || res.Rows != n {
		t.Fatalf("concurrent chain forked or lost rows: %+v", res)
	}
}

// TestEgressUpdateRealizedKeepsChainIntact proves the wave-2 realized-outcome
// callback: UpdateEgressRealized mutates applied/fail_closed/realized_outcome in
// place, the view reflects it, AND the tamper-evident chain still verifies
// (those columns are excluded from the hash preimage — canonicalBytes).
func TestEgressUpdateRealizedKeepsChainIntact(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		id, err := s.InsertEgressDecision(ctx, sampleEgressRow(i))
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	// Report a realized outcome on the middle row.
	if err := s.UpdateEgressRealized(ctx, ids[1], true, false, "applied"); err != nil {
		t.Fatalf("update realized: %v", err)
	}
	// A pinned failure on the last row.
	if err := s.UpdateEgressRealized(ctx, ids[2], false, true, "fail_closed"); err != nil {
		t.Fatalf("update realized (fail-closed): %v", err)
	}
	// Zero id is a documented no-op.
	if err := s.UpdateEgressRealized(ctx, 0, true, false, "applied"); err != nil {
		t.Fatalf("zero-id update should no-op: %v", err)
	}

	res, err := s.VerifyEgressChain(ctx)
	if err != nil || !res.OK || res.Rows != 3 {
		t.Fatalf("chain broke after realized update: %+v (err=%v)", res, err)
	}
	views, err := s.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// Newest-first: views[0] = last row (fail_closed), views[1] = middle (applied).
	if !views[0].FailClosed || views[0].RealizedOutcome != "fail_closed" {
		t.Errorf("fail-closed realized outcome not persisted: %+v", views[0])
	}
	if !views[1].Applied || views[1].RealizedOutcome != "applied" {
		t.Errorf("applied realized outcome not persisted: %+v", views[1])
	}
}

func TestEgressActionCountsAndList(t *testing.T) {
	ctx := context.Background()
	s := newDB(t)
	if _, err := s.InsertEgressDecision(ctx, sampleEgressRow(0)); err != nil {
		t.Fatalf("insert: %v", err)
	}
	counts, err := s.EgressActionCounts(ctx, time.Time{})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts["route_upstream"] != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	views, err := s.ListEgressDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 1 || views[0].RuleName != "flagged-to-local" || !views[0].MustUseTarget {
		t.Fatalf("list = %+v", views)
	}
}
