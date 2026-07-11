package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestPrivacyScrubTest pins the P6.5 tester: a secret-bearing string
// comes back redacted with changed=true, a clean string round-trips,
// the enabled flag mirrors config, extra_patterns apply, and the
// method guard holds.
func TestPrivacyScrubTest(t *testing.T) {
	server, _ := wizardTestServer(t)
	post := func(body string) (int, map[string]any) {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/privacy/scrub-test", strings.NewReader(body))
		server.Handler().ServeHTTP(rr, req)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	// A GitHub-PAT-shaped token must be redacted.
	code, out := post(`{"text":"token is ghp_abcdefghijklmnopqrstuvwx123456 ok"}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if out["changed"] != true {
		t.Errorf("secret not flagged: %+v", out)
	}
	if s, _ := out["scrubbed"].(string); strings.Contains(s, "ghp_abcdefghijklmnopqrstuvwx123456") {
		t.Errorf("secret survived: %q", s)
	}

	// Clean text round-trips unchanged.
	if _, out := post(`{"text":"just a plain sentence"}`); out["changed"] != false {
		t.Errorf("clean text marked changed: %+v", out)
	}

	// enabled mirrors config; extra patterns apply.
	if err := os.WriteFile(server.opts.ConfigPath, []byte(
		"[observer.secrets]\nenable_scrubbing = false\nextra_patterns = [\"MYCO-[0-9]{4}\"]\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	_, out = post(`{"text":"id MYCO-1234 here"}`)
	if out["enabled"] != false {
		t.Errorf("enabled flag: %+v", out)
	}
	if out["changed"] != true {
		t.Errorf("extra pattern not applied: %+v", out)
	}

	// Method guard.
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/privacy/scrub-test", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d want 405", rr.Code)
	}
}
