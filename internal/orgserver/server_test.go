package orgserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
	gen "github.com/marmutapp/superbased-observer/internal/orgserver/api/gen"
)

func TestLoadSCIMToken(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "tok")
	if err := os.WriteFile(good, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := loadSCIMToken(good)
	if err != nil || tok != "s3cret" {
		t.Fatalf("loadSCIMToken = %q, %v", tok, err)
	}

	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte("  \n"), 0o600)
	if _, err := loadSCIMToken(empty); err == nil {
		t.Error("expected error for empty token file")
	}
	if _, err := loadSCIMToken(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error for missing token file")
	}
}

// fakeVerifier always succeeds.
type fakeVerifier struct{}

func (fakeVerifier) VerifyBearer(context.Context, string) (orgcontract.BearerClaims, error) {
	return orgcontract.BearerClaims{Sub: "u"}, nil
}

func TestBearerSecurityScopeAware(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	guarded := bearerSecurity(fakeVerifier{})(next)

	// No scope marker → pass-through (enroll-style public route).
	called = false
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/agent/enroll", nil))
	if !called || rec.Code != http.StatusOK {
		t.Errorf("unmarked route should pass through: called=%v code=%d", called, rec.Code)
	}

	// Scope marker present but no Authorization header → 401 (push-style).
	called = false
	req := httptest.NewRequest(http.MethodPost, "/api/agent/push", nil)
	req = req.WithContext(context.WithValue(req.Context(), gen.BearerAuthScopes, []string{}))
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if called || rec.Code != http.StatusUnauthorized {
		t.Errorf("marked route without bearer should 401: called=%v code=%d", called, rec.Code)
	}
}
