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

// newPolicyTestServer builds an obs API over a temp DB with an admission
// service seeded to a minimal observe policy, and returns a live test server.
func newPolicyTestServer(t *testing.T) (*httptest.Server, *obs.AdmissionService) {
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
	spec, err := admission.Compile(admission.PolicyInput{Mode: "observe"})
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
	return srv, svc
}

// TestAdmissionGetPolicy confirms GET returns the current live policy in
// editable form (the editor prefill).
func TestAdmissionGetPolicy(t *testing.T) {
	srv, _ := newPolicyTestServer(t)
	resp, err := http.Get(srv.URL + "/api/obs/admission/policy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got admissionPolicyGetResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.Policy.Mode != "observe" {
		t.Errorf("got = %+v, want enabled observe", got)
	}
}

// TestAdmissionSetPolicyAppliesGood confirms a valid policy hot-reloads and the
// service's live policy hash changes.
func TestAdmissionSetPolicyAppliesGood(t *testing.T) {
	srv, svc := newPolicyTestServer(t)
	before, _ := svc.Policy()

	body := admissionPolicyDTO{
		Mode: "observe",
		Criteria: []admissionCriterionDTO{
			{ID: "AD-scope", Type: "custom", Name: "on scope", Definition: "Must be about supported coding tasks.", Decision: "deny", Severity: "high"},
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/obs/admission/policy", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got admissionPolicyApplyResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Applied || got.PolicyHash == "" {
		t.Fatalf("apply response = %+v, want applied with hash", got)
	}
	after, ok := svc.Policy()
	if !ok || after.Hash == before.Hash {
		t.Errorf("live policy hash did not change: before=%s after=%s", before.Hash, after.Hash)
	}
	if len(after.Criteria) != 1 || after.Criteria[0].ID != "AD-scope" {
		t.Errorf("hot-reloaded criteria = %+v, want AD-scope", after.Criteria)
	}
}

// TestAdmissionSetPolicyRejectsFatal confirms a fatal-lint policy is a 422 and
// the live policy is left untouched.
func TestAdmissionSetPolicyRejectsFatal(t *testing.T) {
	srv, svc := newPolicyTestServer(t)
	before, _ := svc.Policy()

	// A judged criterion with no definition is a fatal lint (nothing for the
	// judge to evaluate).
	body := admissionPolicyDTO{
		Mode: "observe",
		Criteria: []admissionCriterionDTO{
			{ID: "AD-empty", Type: "custom", Name: "empty", Decision: "deny"},
		},
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/obs/admission/policy", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var got admissionPolicyApplyResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Applied {
		t.Error("fatal-lint policy should not apply")
	}
	if !anyFatalIssue(got.Issues) {
		t.Errorf("expected a fatal issue, got %+v", got.Issues)
	}
	after, _ := svc.Policy()
	if after.Hash != before.Hash {
		t.Errorf("live policy changed on a rejected apply: before=%s after=%s", before.Hash, after.Hash)
	}
}
