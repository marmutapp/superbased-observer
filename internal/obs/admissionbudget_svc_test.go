package obs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// newBudgetSvc builds an admission service with the given policy + budget
// options over a temp obs store.
func newBudgetSvc(t *testing.T, in admission.PolicyInput, opts AdmissionOptions) (*AdmissionService, *obsstore.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(ctx, db.Options{Path: filepath.Join(t.TempDir(), "obs.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	st, err := obsstore.Open(ctx, conn)
	if err != nil {
		t.Fatalf("obsstore.Open: %v", err)
	}
	spec, err := admission.Compile(in)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	svc := NewAdmissionService(st, nil, admissionGate{allow: true}, nil, opts)
	svc.SetPolicy(ctx, spec)
	return svc, st
}

// seedSpend attributes one span of `cost` to `user` (empty = anonymous) at now.
func seedSpend(t *testing.T, st *obsstore.Store, user string, cost float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	id := "seed-" + user
	if user == "" {
		id = "seed-anon"
	}
	if err := st.UpsertTrace(ctx, span.Trace{
		TraceID: id, User: user, Source: span.SourceOTLPTrace, StartedAt: now,
	}); err != nil {
		t.Fatalf("UpsertTrace: %v", err)
	}
	c := cost
	if err := st.UpsertSpansBatch(ctx, []span.Span{
		{SpanID: id + "-s", TraceID: id, Kind: span.KindLLM, Name: "chat", CostUSD: &c, StartedAt: now},
	}); err != nil {
		t.Fatalf("UpsertSpansBatch: %v", err)
	}
}

// TestAdmissionBudgetGate covers the per-end-user budget fold in Check
// (org-hosted-app model): a breach is a Deny verdict — observe records a shadow
// would-deny but admits, enforce blocks; anonymous/disabled/under-budget are
// inert; a deterministic deny keeps its own criterion (stricter-wins tie).
func TestAdmissionBudgetGate(t *testing.T) {
	ctx := context.Background()

	t.Run("observe over-budget records would-deny but admits", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "observe"},
			AdmissionOptions{Hosting: "off", BudgetEnabled: true, BudgetMonthlyUSD: 1.0})
		seedSpend(t, st, "alice", 5.0) // over the $1 monthly cap
		resp := svc.Check(ctx, AdmissionCheck{Text: "hi", User: "alice", RequestID: "r1", Persist: true})
		if !resp.Allowed {
			t.Errorf("observe must admit, got %+v", resp)
		}
		if resp.Decision != "deny" || resp.Criterion != admission.BudgetCriterionMonthly {
			t.Errorf("shadow verdict = %+v, want deny/%s", resp, admission.BudgetCriterionMonthly)
		}
		rows, err := st.ListAdmissionEvents(ctx, obsstore.AdmissionListOptions{})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(rows) != 1 || rows[0].Decision != "deny" || rows[0].CriterionID != admission.BudgetCriterionMonthly {
			t.Errorf("recorded rows = %+v, want one deny/%s", rows, admission.BudgetCriterionMonthly)
		}
	})

	t.Run("enforce over weekly cap blocks", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "enforce"},
			AdmissionOptions{Hosting: "off", BudgetEnabled: true, BudgetWeeklyUSD: 2.0})
		seedSpend(t, st, "bob", 5.0)
		resp := svc.Check(ctx, AdmissionCheck{Text: "hi", User: "bob", RequestID: "r2"})
		if resp.Allowed {
			t.Errorf("enforce over-budget must block: %+v", resp)
		}
		if resp.Criterion != admission.BudgetCriterionWeekly {
			t.Errorf("criterion = %q, want %s", resp.Criterion, admission.BudgetCriterionWeekly)
		}
	})

	t.Run("enforce over 5-hour cap blocks", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "enforce"},
			AdmissionOptions{Hosting: "off", BudgetEnabled: true, BudgetFiveHourUSD: 1.0})
		seedSpend(t, st, "faye", 3.0) // seeded at now → within the rolling 5h window
		resp := svc.Check(ctx, AdmissionCheck{Text: "hi", User: "faye", RequestID: "r7"})
		if resp.Allowed {
			t.Errorf("enforce over 5h cap must block: %+v", resp)
		}
		if resp.Criterion != admission.BudgetCriterionFiveHour {
			t.Errorf("criterion = %q, want %s", resp.Criterion, admission.BudgetCriterionFiveHour)
		}
	})

	t.Run("under budget admits", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "enforce"},
			AdmissionOptions{Hosting: "off", BudgetEnabled: true, BudgetMonthlyUSD: 100.0})
		seedSpend(t, st, "carol", 5.0)
		resp := svc.Check(ctx, AdmissionCheck{Text: "hi", User: "carol", RequestID: "r3"})
		if !resp.Allowed || resp.Decision != "allow" {
			t.Errorf("under budget should allow: %+v", resp)
		}
	})

	t.Run("anonymous request inert", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "enforce"},
			AdmissionOptions{Hosting: "off", BudgetEnabled: true, BudgetMonthlyUSD: 1.0})
		seedSpend(t, st, "", 5.0) // anonymous spend never counts against a user
		resp := svc.Check(ctx, AdmissionCheck{Text: "hi", User: "", RequestID: "r4"})
		if !resp.Allowed {
			t.Errorf("no end-user id → budget inert, should allow: %+v", resp)
		}
	})

	t.Run("disabled inert", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "enforce"},
			AdmissionOptions{Hosting: "off", BudgetEnabled: false, BudgetMonthlyUSD: 1.0})
		seedSpend(t, st, "dave", 5.0)
		resp := svc.Check(ctx, AdmissionCheck{Text: "hi", User: "dave", RequestID: "r5"})
		if !resp.Allowed {
			t.Errorf("budget disabled → allow: %+v", resp)
		}
	})

	t.Run("deterministic deny keeps its criterion over an equal budget deny", func(t *testing.T) {
		svc, st := newBudgetSvc(t,
			admission.PolicyInput{Mode: "enforce", Prefilter: admission.PrefilterInput{Deny: []string{"nope"}}},
			AdmissionOptions{Hosting: "off", BudgetEnabled: true, BudgetMonthlyUSD: 1.0})
		seedSpend(t, st, "erin", 5.0)
		resp := svc.Check(ctx, AdmissionCheck{Text: "nope", User: "erin", RequestID: "r6"})
		if resp.Allowed {
			t.Errorf("should block: %+v", resp)
		}
		if resp.Criterion != "prefilter.deny" {
			t.Errorf("criterion = %q, want prefilter.deny (an existing deny is not overridden by an equal budget deny)", resp.Criterion)
		}
	})
}
