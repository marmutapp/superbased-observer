package store

import (
	"context"
	"testing"
	"time"
)

func TestAdmissionChain_InsertVerifyTamper(t *testing.T) {
	s := newDB(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	rows := []AdmissionEventRow{
		{TS: base, Mode: "observe", Decision: "allow", Severity: "info", PolicyHash: "p1", MessageHash: "m1", RequestID: "gen-1"},
		{TS: base.Add(time.Second), Mode: "observe", Decision: "flag", Severity: "warn", CriterionID: "AD-200", PolicyHash: "p1", MessageHash: "m2"},
		{TS: base.Add(2 * time.Second), Mode: "observe", Decision: "deny", Severity: "high", CriterionID: "AD-100", PolicyHash: "p1", JudgeUsed: true, JudgeHosting: "local", MessageHash: "m3", TraceID: "t1"},
	}
	for i, r := range rows {
		if _, err := s.InsertAdmissionEvent(ctx, r); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	// Chain verifies clean.
	res, err := s.VerifyAdmissionChain(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !res.OK || res.Rows != 3 {
		t.Fatalf("verify = %+v, want OK/3 rows", res)
	}

	// Decision counts.
	counts, err := s.AdmissionDecisionCounts(ctx, time.Time{})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts["allow"] != 1 || counts["flag"] != 1 || counts["deny"] != 1 {
		t.Errorf("counts = %v", counts)
	}

	// Enrichment soft-join by request_id.
	byReq, err := s.AdmissionEventsForRequest(ctx, "gen-1")
	if err != nil {
		t.Fatalf("for request: %v", err)
	}
	if len(byReq) != 1 || byReq[0].Decision != "allow" {
		t.Errorf("AdmissionEventsForRequest = %+v", byReq)
	}

	// List newest-first + decision filter.
	denies, err := s.ListAdmissionEvents(ctx, AdmissionListOptions{Decision: "deny"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(denies) != 1 || denies[0].CriterionID != "AD-100" || !denies[0].JudgeUsed {
		t.Errorf("deny list = %+v", denies)
	}

	// Tamper: mutate a stored decision out from under the chain → verify breaks.
	if _, err := s.db.ExecContext(ctx, `UPDATE obs_admission_events SET decision='allow' WHERE criterion_id='AD-100'`); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	res, err = s.VerifyAdmissionChain(ctx)
	if err != nil {
		t.Fatalf("verify after tamper: %v", err)
	}
	if res.OK {
		t.Errorf("chain still OK after tamper: %+v", res)
	}
	if res.BreakAt == 0 {
		t.Errorf("expected a break id, got %+v", res)
	}
}

func TestAdmissionPolicyVersion_Idempotent(t *testing.T) {
	s := newDB(t)
	ctx := context.Background()
	if err := s.UpsertPolicyVersion(ctx, "hash1", "observe", "last_user", 3, "body-v1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// second upsert of the same hash is a no-op (ON CONFLICT DO NOTHING).
	if err := s.UpsertPolicyVersion(ctx, "hash1", "enforce", "conversation", 9, "body-v2"); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	var mode, body string
	if err := s.db.QueryRowContext(ctx, `SELECT mode, body FROM obs_admission_policy_versions WHERE policy_hash='hash1'`).Scan(&mode, &body); err != nil {
		t.Fatalf("read: %v", err)
	}
	if mode != "observe" || body != "body-v1" {
		t.Errorf("content-addressed row changed: mode=%q body=%q", mode, body)
	}
}
