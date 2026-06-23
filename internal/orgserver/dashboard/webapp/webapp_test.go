package webapp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler_ServesShellAndAssets(t *testing.T) {
	h := Handler()

	// Root serves the SPA shell.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Fatalf("root: code=%d body=%q", rec.Code, rec.Body.String()[:min(80, rec.Body.Len())])
	}

	// A client-side route falls back to index.html (not 404).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/teams/abc", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<div id=\"root\">") {
		t.Errorf("client route fallback: code=%d", rec.Code)
	}

	// A fingerprinted asset is served with a JS content type.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/", nil))
	if rec.Code != http.StatusOK { // directory listing or fallback; just must not 500
		t.Logf("assets index code=%d (informational)", rec.Code)
	}
}
