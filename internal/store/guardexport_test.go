package store

import (
	"context"
	"testing"
	"time"
)

// TestGuardOTelCursor pins the G16 OTel-tail cursor: independent of
// the cloud cursor, 0 when unset, save/overwrite round-trips.
func TestGuardOTelCursor(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	if cur, err := s.LoadGuardOTelCursor(ctx); err != nil || cur != 0 {
		t.Errorf("unset cursor = %d, %v; want 0, nil", cur, err)
	}
	if err := s.SaveGuardOTelCursor(ctx, 17); err != nil {
		t.Fatal(err)
	}
	if cur, err := s.LoadGuardOTelCursor(ctx); err != nil || cur != 17 {
		t.Errorf("cursor = %d, %v; want 17", cur, err)
	}
	if err := s.SaveGuardOTelCursor(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if cur, _ := s.LoadGuardOTelCursor(ctx); cur != 42 {
		t.Errorf("overwritten cursor = %d, want 42", cur)
	}

	// Independence: the OTel cursor never reads or writes the cloud
	// dispatcher's position (separate consumers, separate keys).
	if cur, err := s.LoadGuardCloudCursor(ctx); err != nil || cur != 0 {
		t.Errorf("cloud cursor disturbed by otel cursor writes: %d, %v", cur, err)
	}
}

// TestSummarizeGuardEventsBetween pins the §14.4 windowed aggregate:
// half-open [since, until), zero times unbounded, and the since-only
// wrapper unchanged.
func TestSummarizeGuardEventsBetween(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	mk := func(at time.Time, decision, severity string, enforced bool) GuardEventRow {
		return GuardEventRow{
			TS: at, SessionID: "s1", RuleID: "R-101", Category: "destructive",
			Severity: severity, Decision: decision, Enforced: enforced, Source: "builtin",
		}
	}
	if _, err := s.InsertGuardEvents(ctx, []GuardEventRow{
		mk(base, "flag", "warn", false),                       // at since: in
		mk(base.Add(24*time.Hour), "deny", "critical", true),  // inside
		mk(base.Add(48*time.Hour), "flag", "high", false),     // at until: out
		mk(base.Add(-24*time.Hour), "deny", "critical", true), // before: out
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sum, err := s.SummarizeGuardEventsBetween(ctx, base, base.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("SummarizeGuardEventsBetween: %v", err)
	}
	if sum.Total != 2 || sum.Enforced != 1 {
		t.Errorf("window total=%d enforced=%d, want 2/1", sum.Total, sum.Enforced)
	}
	if sum.ByDecision["flag"] != 1 || sum.ByDecision["deny"] != 1 {
		t.Errorf("ByDecision = %v", sum.ByDecision)
	}

	// Zero until = unbounded above (3 rows from base onward).
	open, err := s.SummarizeGuardEventsBetween(ctx, base, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if open.Total != 3 {
		t.Errorf("open-ended total = %d, want 3", open.Total)
	}

	// Zero both = everything; the since-only wrapper agrees.
	all, err := s.SummarizeGuardEventsBetween(ctx, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := s.SummarizeGuardEvents(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Total != 4 || wrapped.Total != 4 {
		t.Errorf("unbounded totals = %d (between), %d (wrapper); want 4/4", all.Total, wrapped.Total)
	}
}

// TestLoadGuardPolicyStates pins the §14.4 full-log read: every
// version row, id (load) order, signature COALESCEd.
func TestLoadGuardPolicyStates(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	at := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	for i, row := range []GuardPolicyStateRow{
		{Layer: "user", Path: "/u/policy.toml", ContentHash: "h1", LoadedAt: at},
		{Layer: "org", Path: "https://org#policy-key", Version: "3", ContentHash: "k1", Signature: "sig", LoadedAt: at.Add(time.Hour)},
		{Layer: "user", Path: "/u/policy.toml", ContentHash: "h2", LoadedAt: at.Add(2 * time.Hour)},
	} {
		appended, err := s.RecordGuardPolicyState(ctx, row)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !appended {
			t.Fatalf("record %d: not appended", i)
		}
	}

	log, err := s.LoadGuardPolicyStates(ctx)
	if err != nil {
		t.Fatalf("LoadGuardPolicyStates: %v", err)
	}
	if len(log) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(log), log)
	}
	if log[0].ContentHash != "h1" || log[1].Signature != "sig" || log[2].ContentHash != "h2" {
		t.Errorf("rows out of load order or fields dropped: %+v", log)
	}
	if !log[2].LoadedAt.Equal(at.Add(2 * time.Hour)) {
		t.Errorf("loaded_at round-trip: %v", log[2].LoadedAt)
	}
}
