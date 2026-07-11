package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/obs"
	"github.com/marmutapp/superbased-observer/internal/obs/admission"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// newPersistTestServer builds an obs API over a temp DB with a minimal observe
// policy, optionally injecting a persister, and returns a live test server.
func newPersistTestServer(t *testing.T, persister PolicyPersistFunc) *httptest.Server {
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

	api := New(st, nil, svc, nil)
	if persister != nil {
		api.SetPolicyPersister(persister)
	}
	mux := http.NewServeMux()
	for _, r := range api.Routes() {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// validPolicyBody is a minimal lint-clean editor policy.
func validPolicyBody() []byte {
	raw, _ := json.Marshal(admissionPolicyDTO{
		Mode: "observe",
		Criteria: []admissionCriterionDTO{
			{ID: "AD-scope", Type: "custom", Name: "on scope", Definition: "Must be about supported coding tasks.", Decision: "deny", Severity: "high"},
		},
	})
	return raw
}

// TestAdmissionSetPolicyPersist is the table-driven matrix over the opt-in
// write-through seam: flag off never touches the persister; flag on routes the
// applied JSON to it; a persister failure or an unwired persister stays a 200
// with applied:true/persisted:false + persist_error.
func TestAdmissionSetPolicyPersist(t *testing.T) {
	type recorder struct {
		calls int
		body  []byte
	}
	tests := []struct {
		name          string
		query         string
		wirePersister bool
		persistErr    error
		wantStatus    int
		wantApplied   bool
		wantPersisted bool
		wantErrSubstr string
		wantCalls     int
	}{
		{
			name:          "persist off — default in-memory only, persister never called",
			query:         "",
			wirePersister: true,
			wantStatus:    http.StatusOK,
			wantApplied:   true,
			wantPersisted: false,
			wantCalls:     0,
		},
		{
			name:          "persist on + success — applied and persisted",
			query:         "?persist=1",
			wirePersister: true,
			wantStatus:    http.StatusOK,
			wantApplied:   true,
			wantPersisted: true,
			wantCalls:     1,
		},
		{
			name:          "persist on + persister fails — applied true, persisted false, 200",
			query:         "?persist=true",
			wirePersister: true,
			persistErr:    errors.New("disk full"),
			wantStatus:    http.StatusOK,
			wantApplied:   true,
			wantPersisted: false,
			wantErrSubstr: "disk full",
			wantCalls:     1,
		},
		{
			name:          "persist on but no persister wired — applied true, persisted false, 200",
			query:         "?persist=yes",
			wirePersister: false,
			wantStatus:    http.StatusOK,
			wantApplied:   true,
			wantPersisted: false,
			wantErrSubstr: "not available",
			wantCalls:     0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			var persister PolicyPersistFunc
			if tc.wirePersister {
				persister = func(_ context.Context, policyJSON []byte) error {
					rec.calls++
					rec.body = policyJSON
					return tc.persistErr
				}
			}
			srv := newPersistTestServer(t, persister)

			resp, err := http.Post(srv.URL+"/api/obs/admission/policy"+tc.query, "application/json", bytes.NewReader(validPolicyBody()))
			if err != nil {
				t.Fatalf("post: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var got admissionPolicyApplyResponse
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Applied != tc.wantApplied {
				t.Errorf("applied = %v, want %v", got.Applied, tc.wantApplied)
			}
			if got.Persisted != tc.wantPersisted {
				t.Errorf("persisted = %v, want %v", got.Persisted, tc.wantPersisted)
			}
			if tc.wantErrSubstr == "" && got.PersistError != "" {
				t.Errorf("unexpected persist_error = %q", got.PersistError)
			}
			if tc.wantErrSubstr != "" && !bytes.Contains([]byte(got.PersistError), []byte(tc.wantErrSubstr)) {
				t.Errorf("persist_error = %q, want substring %q", got.PersistError, tc.wantErrSubstr)
			}
			if rec.calls != tc.wantCalls {
				t.Errorf("persister calls = %d, want %d", rec.calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				// The persister must receive the raw edited policy JSON.
				var echoed admissionPolicyDTO
				if err := json.Unmarshal(rec.body, &echoed); err != nil {
					t.Fatalf("persister body not valid policy JSON: %v", err)
				}
				if len(echoed.Criteria) != 1 || echoed.Criteria[0].ID != "AD-scope" {
					t.Errorf("persister body criteria = %+v, want AD-scope", echoed.Criteria)
				}
			}
		})
	}
}

// TestAdmissionSetPolicyLintRejectNeverPersists confirms a fatal-lint policy is
// a 422 that never reaches the persister, even with persist=1.
func TestAdmissionSetPolicyLintRejectNeverPersists(t *testing.T) {
	calls := 0
	persister := func(_ context.Context, _ []byte) error { calls++; return nil }
	srv := newPersistTestServer(t, persister)

	// A judged criterion with no definition is a fatal lint.
	raw, _ := json.Marshal(admissionPolicyDTO{
		Mode: "observe",
		Criteria: []admissionCriterionDTO{
			{ID: "AD-empty", Type: "custom", Name: "empty", Decision: "deny"},
		},
	})
	resp, err := http.Post(srv.URL+"/api/obs/admission/policy?persist=1", "application/json", bytes.NewReader(raw))
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
	if got.Applied || got.Persisted {
		t.Errorf("rejected policy must be neither applied nor persisted: %+v", got)
	}
	if calls != 0 {
		t.Errorf("persister called %d times on a lint-rejected policy, want 0", calls)
	}
}
