package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/egress"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// newEgressServer builds the httpapi over a temp obs store with an
// AdmissionService carrying the given egress policy (nil svc = admission not
// wired, the egress-off posture).
func newEgressServer(t *testing.T, spec *egress.PolicySpec) (*httptest.Server, *obsstore.Store) {
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
	var svc *obs.AdmissionService
	if spec != nil {
		svc = obs.NewAdmissionService(st, nil, nil, nil, obs.AdmissionOptions{})
		svc.SetEgressPolicy(*spec)
	}
	mux := http.NewServeMux()
	for _, r := range New(st, nil, svc, nil).Routes() {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st
}

// getEgressJSON decodes a 200 response via the shared getJSON helper,
// failing the test on any non-OK status.
func getEgressJSON[T any](t *testing.T, url string) T {
	t.Helper()
	var out T
	if code := getJSON(t, url, &out); code != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, code)
	}
	return out
}

// TestEgressStatus_OffPosture: with no admission service wired (egress cannot
// run), the status endpoint reports the honest off posture with empty (never
// null) rules/targets and an intact zero-row chain — no fabricated counts.
func TestEgressStatus_OffPosture(t *testing.T) {
	srv, _ := newEgressServer(t, nil)
	st := getEgressJSON[egressStatusResponse](t, srv.URL+"/api/obs/egress/status")
	if st.Enabled || st.Mode != "off" {
		t.Errorf("off posture wrong: enabled=%v mode=%q", st.Enabled, st.Mode)
	}
	if st.Rules == nil || len(st.Rules) != 0 {
		t.Errorf("rules must be empty []: %#v", st.Rules)
	}
	if st.Targets == nil || len(st.Targets) != 0 {
		t.Errorf("targets must be empty []: %#v", st.Targets)
	}
	if !st.Chain.OK || st.Chain.Rows != 0 {
		t.Errorf("empty chain must verify intact with 0 rows: %+v", st.Chain)
	}

	dec := getEgressJSON[egressDecisionsResponse](t, srv.URL+"/api/obs/egress/decisions")
	if dec.Decisions == nil || len(dec.Decisions) != 0 {
		t.Errorf("decisions must be empty []: %#v", dec.Decisions)
	}
}

// TestEgressStatus_InstalledPolicyAndDecisions: with an enforce policy
// installed and two recorded decisions (one carrying a proxy-realized
// outcome), the surface renders policy + rows VERBATIM — mode, hash, the
// pinned derivation, by-action counts, newest-first order, and the realized
// outcome exactly as stored.
func TestEgressStatus_InstalledPolicyAndDecisions(t *testing.T) {
	ctx := context.Background()
	spec, err := egress.Compile(egress.PolicyInput{
		Mode: "enforce",
		Targets: []egress.TargetInput{
			{ID: "ollama-local", URL: "http://127.0.0.1:11434", Shape: "anthropic"},
		},
		Rules: []egress.RuleInput{
			{
				Name:            "flagged-to-local",
				When:            egress.WhenInput{VerdictAtLeast: "flag"},
				RouteToUpstream: "ollama-local",
				OnUnavailable:   "deny",
				ReasonCode:      "egress_flagged_local",
			},
			{
				Name:         "budget-cheaper",
				When:         egress.WhenInput{BudgetBandAtLeast: 0.8, BudgetBandSet: true},
				RouteToModel: "claude-3-5-haiku-20241022",
			},
		},
	})
	if err != nil {
		t.Fatalf("egress.Compile: %v", err)
	}
	srv, st := newEgressServer(t, &spec)

	id1, err := st.InsertEgressDecision(ctx, obsstore.EgressDecisionRow{
		Mode: "enforce", RuleName: "flagged-to-local", PolicyHash: spec.Hash,
		Action: "route_upstream", UpstreamID: "ollama-local", TargetShape: "anthropic",
		ReasonCode: "egress_flagged_local", MustUseTarget: true,
		VerdictDecision: "flag", RequestID: "req-1", User: "u1",
	})
	if err != nil {
		t.Fatalf("InsertEgressDecision 1: %v", err)
	}
	if _, err := st.InsertEgressDecision(ctx, obsstore.EgressDecisionRow{
		Mode: "enforce", RuleName: "budget-cheaper", PolicyHash: spec.Hash,
		Action: "route_model", ModelFrom: "claude-opus-4-8", ModelTo: "claude-3-5-haiku-20241022",
		ReasonCode: "egress_budget_band", VerdictDecision: "allow", RequestID: "req-2",
	}); err != nil {
		t.Fatalf("InsertEgressDecision 2: %v", err)
	}
	// The proxy realized the first decision as fail-closed.
	if err := st.UpdateEgressRealized(ctx, id1, false, true, "upstream_error"); err != nil {
		t.Fatalf("UpdateEgressRealized: %v", err)
	}

	stat := getEgressJSON[egressStatusResponse](t, srv.URL+"/api/obs/egress/status")
	if !stat.Enabled || stat.Mode != "enforce" || stat.PolicyHash != spec.Hash {
		t.Errorf("posture wrong: %+v", stat)
	}
	if len(stat.Rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(stat.Rules))
	}
	r0 := stat.Rules[0]
	if r0.Name != "flagged-to-local" || r0.Action != "route_upstream" || r0.Target != "ollama-local" ||
		r0.OnUnavailable != "deny" || !r0.Pinned || r0.ReasonCode != "egress_flagged_local" {
		t.Errorf("rule 0 rendered wrong: %+v", r0)
	}
	if r1 := stat.Rules[1]; r1.Pinned || r1.Target != "claude-3-5-haiku-20241022" {
		t.Errorf("rule 1 rendered wrong: %+v", r1)
	}
	if len(stat.Targets) != 1 || stat.Targets[0].ID != "ollama-local" || stat.Targets[0].Shape != "anthropic" {
		t.Errorf("targets rendered wrong: %+v", stat.Targets)
	}
	if stat.DecisionsByAction["route_upstream"] != 1 || stat.DecisionsByAction["route_model"] != 1 {
		t.Errorf("by-action counts wrong: %+v", stat.DecisionsByAction)
	}
	if stat.Decisions24h != 2 {
		t.Errorf("decisions_24h wrong: %d", stat.Decisions24h)
	}
	if !stat.Chain.OK || stat.Chain.Rows != 2 {
		t.Errorf("chain must verify intact over 2 rows (realized update must NOT break it): %+v", stat.Chain)
	}

	dec := getEgressJSON[egressDecisionsResponse](t, srv.URL+"/api/obs/egress/decisions")
	if len(dec.Decisions) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(dec.Decisions))
	}
	// Newest first: the route_model row was inserted second.
	if dec.Decisions[0].RuleName != "budget-cheaper" || dec.Decisions[1].RuleName != "flagged-to-local" {
		t.Errorf("order wrong: %q then %q", dec.Decisions[0].RuleName, dec.Decisions[1].RuleName)
	}
	d1 := dec.Decisions[1]
	if !d1.FailClosed || d1.Applied || d1.RealizedOutcome != "upstream_error" {
		t.Errorf("realized outcome not rendered verbatim: %+v", d1)
	}
	if !d1.MustUseTarget || d1.VerdictDecision != "flag" || d1.RequestID != "req-1" || d1.User != "u1" {
		t.Errorf("decision fields not rendered verbatim: %+v", d1)
	}
}
