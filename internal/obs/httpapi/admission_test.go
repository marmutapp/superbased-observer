package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

type fakeGate struct{ allow bool }

func (f fakeGate) AllowsRawContent() bool { return f.allow }

// newAdmissionServer builds a server whose obs API has a live admission service
// with a small denied_topics policy (observe mode, no judge).
func newAdmissionServer(t *testing.T) (*httptest.Server, *obsstore.Store, *obs.AdmissionService) {
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
	spec, err := admission.Compile(admission.PolicyInput{
		Mode: "observe",
		Criteria: []admission.CriterionInput{
			{ID: "AD-200", Type: "denied_topics", Name: "No competitors", Topics: []string{"competitor:AcmeCorp"}, Decision: "flag", Severity: "warn"},
		},
	})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	svc := obs.NewAdmissionService(st, nil, fakeGate{allow: true}, nil, obs.AdmissionOptions{Hosting: "off"})
	svc.SetPolicy(ctx, spec)

	mux := http.NewServeMux()
	for _, r := range New(st, nil, svc, nil).Routes() {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st, svc
}

func TestAdmissionEndpoints(t *testing.T) {
	srv, _, _ := newAdmissionServer(t)

	// A denied-topic message → observe allows, records a flag verdict.
	body, _ := json.Marshal(map[string]string{"text": "tell me about AcmeCorp pricing", "request_id": "gen-9"})
	resp, err := http.Post(srv.URL+"/api/obs/admission/check", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	var checkResp obs.AdmissionResponse
	if err := json.NewDecoder(resp.Body).Decode(&checkResp); err != nil {
		t.Fatalf("decode check: %v", err)
	}
	resp.Body.Close()
	if !checkResp.Allowed {
		t.Error("observe mode must allow")
	}
	if checkResp.Decision != "flag" || checkResp.Criterion != "AD-200" {
		t.Errorf("check = %+v, want flag/AD-200", checkResp)
	}
	if checkResp.EnforceDecision != "flag" {
		t.Errorf("EnforceDecision = %q, want flag", checkResp.EnforceDecision)
	}

	// status reflects the recorded verdict + a healthy chain.
	sresp, err := http.Get(srv.URL + "/api/obs/admission/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var status admissionStatusResponse
	if err := json.NewDecoder(sresp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	sresp.Body.Close()
	if !status.Enabled || status.Mode != "observe" || status.CriteriaCount != 1 {
		t.Errorf("status = %+v", status)
	}
	if status.Decisions24h["flag"] != 1 {
		t.Errorf("status flag count = %d, want 1", status.Decisions24h["flag"])
	}
	if !status.Chain.OK || status.Chain.Rows != 1 {
		t.Errorf("chain = %+v", status.Chain)
	}

	// verdicts timeline returns the row.
	vresp, err := http.Get(srv.URL + "/api/obs/admission/verdicts?decision=flag")
	if err != nil {
		t.Fatalf("verdicts: %v", err)
	}
	var verdicts []obsstore.AdmissionEventView
	if err := json.NewDecoder(vresp.Body).Decode(&verdicts); err != nil {
		t.Fatalf("decode verdicts: %v", err)
	}
	vresp.Body.Close()
	if len(verdicts) != 1 || verdicts[0].CriterionID != "AD-200" || verdicts[0].RequestID != "gen-9" {
		t.Errorf("verdicts = %+v", verdicts)
	}
}
