package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthDoctor pins the endpoint shape: every diag check arrives
// with a name, a string status from the ok/warn/fail vocabulary, and
// counts that sum to the check count. Statuses themselves are NOT
// asserted (they depend on the machine); the home is sandboxed via
// setupWizardHome so hook/MCP checks read a temp dir, not the
// developer's real config.
func TestHealthDoctor(t *testing.T) {
	server, _ := wizardTestServer(t) // sandboxes setupWizardHome + temp config/DB

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/health/doctor", nil))
	if rr.Code != 200 {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
		OK    int  `json:"ok"`
		Warn  int  `json:"warn"`
		Fail  int  `json:"fail"`
		AllOK bool `json:"all_ok"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Checks) == 0 {
		t.Fatal("no checks returned")
	}
	valid := map[string]bool{"ok": true, "warn": true, "fail": true}
	for _, c := range got.Checks {
		if c.Name == "" || !valid[c.Status] {
			t.Errorf("bad check row: %+v", c)
		}
	}
	if got.OK+got.Warn+got.Fail != len(got.Checks) {
		t.Errorf("counts %d+%d+%d != %d checks", got.OK, got.Warn, got.Fail, len(got.Checks))
	}
	if got.AllOK != (got.Warn == 0 && got.Fail == 0) {
		t.Errorf("all_ok inconsistent: %v vs warn=%d fail=%d", got.AllOK, got.Warn, got.Fail)
	}

	// Method guard.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/health/doctor", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d want 405", rr.Code)
	}
}
