package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// fakeEnrolment is a test double for the EnrolmentService interface.
type fakeEnrolment struct {
	state       orgclient.EnrolmentState
	statusErr   error
	payload     []byte
	unenrolled  bool
	unenrollErr error
}

func (f *fakeEnrolment) Status(context.Context) (orgclient.EnrolmentState, error) {
	return f.state, f.statusErr
}
func (f *fakeEnrolment) LastPayload(context.Context) ([]byte, error) { return f.payload, nil }
func (f *fakeEnrolment) Unenroll(context.Context) error {
	if f.unenrollErr != nil {
		return f.unenrollErr
	}
	f.unenrolled = true
	return nil
}

func newEnrolmentServer(t *testing.T, oc EnrolmentService) *Server {
	t.Helper()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	srv, err := New(Options{DB: database, OrgClient: oc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestEnrolmentStatus_OrgModeOff(t *testing.T) {
	srv := newEnrolmentServer(t, nil) // nil OrgClient = solo-local
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/enrolment/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var resp enrolmentStatusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Enrolled {
		t.Errorf("solo-local should report enrolled=false, got %+v", resp)
	}

	// last-payload returns the JSON literal null.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/enrolment/last-payload", nil))
	if got := rec.Body.String(); got != "null" {
		t.Errorf("last-payload (off) = %q, want null", got)
	}

	// unenroll is a no-op.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/enrolment/unenroll", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("unenroll (off) code=%d", rec.Code)
	}
}

func TestEnrolmentStatus_Enrolled(t *testing.T) {
	fake := &fakeEnrolment{
		state: orgclient.EnrolmentState{
			Enrolled: true, OrgID: "org-1", OrgName: "Acme", OrgServerURL: "https://org.acme.example",
			UserEmail: "dev@acme.example", EnrolledAt: "2026-05-26T10:00:00Z", Backend: "keychain",
			LastPush: &store.PushLogEntry{PushedAt: "2026-05-26T10:05:00Z", Status: "ok", RowCount: 12, Bytes: 480},
		},
		payload: []byte(`{"agent_version":"test","sessions":[]}`),
	}
	srv := newEnrolmentServer(t, fake)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/enrolment/status", nil))
	var resp enrolmentStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Enrolled || resp.OrgName != "Acme" || resp.UserEmail != "dev@acme.example" || resp.CredentialStore != "keychain" {
		t.Fatalf("status = %+v", resp)
	}
	if resp.LastPush == nil || resp.LastPush.Status != "ok" || resp.LastPush.RowCount != 12 {
		t.Fatalf("last_push = %+v", resp.LastPush)
	}

	// last-payload is served byte-for-byte.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/enrolment/last-payload", nil))
	if got := rec.Body.String(); got != string(fake.payload) {
		t.Errorf("last-payload = %q, want %q", got, fake.payload)
	}

	// unenroll calls through.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/enrolment/unenroll", nil))
	if rec.Code != http.StatusOK || !fake.unenrolled {
		t.Errorf("unenroll code=%d unenrolled=%v", rec.Code, fake.unenrolled)
	}
}

// A Status error is a 500 isolated to this endpoint — other endpoints still
// serve normally (P1: an org failure never breaks the host dashboard).
func TestEnrolmentStatus_ErrorIsolated(t *testing.T) {
	fake := &fakeEnrolment{statusErr: errors.New("keychain locked")}
	srv := newEnrolmentServer(t, fake)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/enrolment/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status (err) code=%d, want 500", rec.Code)
	}

	// An unrelated endpoint is unaffected.
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/api/status code=%d after enrolment 500, want 200", rec.Code)
	}
}

func TestEnrolmentUnenroll_MethodNotAllowed(t *testing.T) {
	srv := newEnrolmentServer(t, &fakeEnrolment{})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/enrolment/unenroll", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET unenroll code=%d, want 405", rec.Code)
	}
}
