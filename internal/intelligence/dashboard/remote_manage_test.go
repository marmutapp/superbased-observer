package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remotecfg"
)

// newManageServer builds a Server with a writable config path (so the remote
// management writes work) + a real DB + a ready controller.
func newManageServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.db")
	database, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	cfgPath := filepath.Join(dir, "config.toml")
	body := "[observer]\ndb_path = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	rc, _ := newReadyRemoteController(t)
	s, err := New(Options{DB: database, ConfigPath: cfgPath, Remote: rc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, s.Handler() // loopback path — Local routes reachable
}

// getConfirm performs GET /api/remote/config and returns the confirm cookie +
// token the SPA would echo.
func getConfirm(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/remote/config", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/remote/config = %d", rec.Code)
	}
	var resp struct {
		ConfirmToken string `json:"confirm_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	var ck *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == remoteConfirmCookie {
			ck = c
		}
	}
	if ck == nil || resp.ConfirmToken == "" {
		t.Fatalf("no confirm cookie/token: cookie=%v token=%q", ck, resp.ConfirmToken)
	}
	if ck.Value != resp.ConfirmToken {
		t.Fatalf("cookie/token mismatch: %q vs %q", ck.Value, resp.ConfirmToken)
	}
	return ck, resp.ConfirmToken
}

// TestArmVerbsRequireConfirmToken pins §10 CSRF hardening negatives across
// enable/disable/rotate.
func TestArmVerbsRequireConfirmToken(t *testing.T) {
	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	for _, path := range []string{"/api/remote/enable", "/api/remote/disable", "/api/remote/rotate", "/api/remote/add-device", "/api/remote/allow-terminal", "/api/remote/allow-terminal-view", "/api/remote/allow-remote-takeover", "/api/remote/approve-execute", "/api/remote/standing-terminal/mint", "/api/remote/standing-terminal/revoke", "/api/remote/tailscale/login", "/api/remote/tailscale/install"} {
		// Wrong method → 405.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET = %d, want 405", path, rec.Code)
		}

		// Non-JSON content type → 415.
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader("x"))
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "text/plain")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, token)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s text/plain = %d, want 415", path, rec.Code)
		}

		// Missing confirm token (no cookie/header) → 403.
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s no-token = %d, want 403", path, rec.Code)
		}

		// Mismatched token (cookie present, wrong header) → 403.
		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Host = "127.0.0.1:8080"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(ck)
		req.Header.Set(remoteConfirmHeader, token+"x")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s mismatched-token = %d, want 403", path, rec.Code)
		}
	}
}

// TestRemoteEnableViaAPIReturnsSecretOnce is the happy path: with a valid
// confirm token, enable arms and returns the pairing secret + URL in the POST
// response.
func TestRemoteEnableViaAPIReturnsSecretOnce(t *testing.T) {
	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/remote/enable", strings.NewReader(`{"host":"box.ts.net","allow_terminal":false}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK              bool   `json:"ok"`
		RestartRequired bool   `json:"restart_required"`
		PairingURL      string `json:"pairing_url"`
		PairingSecret   string `json:"pairing_secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || !resp.RestartRequired || resp.PairingSecret == "" || !strings.Contains(resp.PairingURL, "#pair=") {
		t.Fatalf("enable response missing pairing data / restart flag: %+v", resp)
	}
}

// TestRemoteEnableNoHostIsBadRequest pins the live-verify fix: arming with no
// host in the body AND no trusted-host fallback AND no detectable tailnet host
// is a PRECONDITION — a friendly 400, never a raw 500 (the "tailnet host is
// required" 500 the operator hit clicking Arm with an unfilled field). The
// detector is pinned empty so this holds regardless of whether the runner has
// Tailscale.
func TestRemoteEnableNoHostIsBadRequest(t *testing.T) {
	prev := enableHostDetector
	enableHostDetector = func(context.Context) string { return "" }
	defer func() { enableHostDetector = prev }()

	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/remote/enable", strings.NewReader(`{"allow_terminal":false}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable with no host = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tailnet host") {
		t.Errorf("400 body should name the missing tailnet host: %s", rec.Body.String())
	}
}

// TestRemoteEnableAutoDetectsHost pins the other half of the fix: an empty-body
// arm SUCCEEDS by auto-detecting the tailnet host (CLI parity), so the operator
// need not re-type what the Tailscale card already resolved.
func TestRemoteEnableAutoDetectsHost(t *testing.T) {
	prev := enableHostDetector
	enableHostDetector = func(context.Context) string { return "auto.detected.ts.net" }
	defer func() { enableHostDetector = prev }()

	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	req := httptest.NewRequest(http.MethodPost, "/api/remote/enable", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-detect enable = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "auto.detected.ts.net") {
		t.Errorf("expected the auto-detected host in the response: %s", rec.Body.String())
	}
}

// TestPairingSecretAbsentFromEveryGET pins §11: after arming, the pairing secret
// + URL never appear in ANY GET response body.
func TestPairingSecretAbsentFromEveryGET(t *testing.T) {
	_, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	// Arm and capture the secret from the enable POST response.
	req := httptest.NewRequest(http.MethodPost, "/api/remote/enable", strings.NewReader(`{"host":"box.ts.net"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	var enableResp struct {
		PairingURL    string `json:"pairing_url"`
		PairingSecret string `json:"pairing_secret"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &enableResp)
	if enableResp.PairingSecret == "" {
		t.Fatal("no secret returned by enable")
	}

	for _, path := range []string{"/api/remote/config", "/api/remote/selfcheck", "/api/remote/audit", "/api/remote/sessions"} {
		g := httptest.NewRequest(http.MethodGet, path, nil)
		g.Host = "127.0.0.1:8080"
		grec := httptest.NewRecorder()
		h.ServeHTTP(grec, g)
		bodyStr := grec.Body.String()
		if strings.Contains(bodyStr, enableResp.PairingSecret) {
			t.Errorf("%s GET body leaked the pairing secret", path)
		}
		if strings.Contains(bodyStr, "#pair=") || strings.Contains(bodyStr, "pairing_url") || strings.Contains(bodyStr, "pairing_secret") {
			t.Errorf("%s GET body leaked pairing URL/secret field:\n%s", path, bodyStr)
		}
	}
}

// TestRemoteConfigNeverReturnsSecret pins that /api/remote/config exposes only a
// masked fingerprint, never the hash-at-rest or the secret.
func TestRemoteConfigNeverReturnsSecret(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)
	req := httptest.NewRequest(http.MethodPost, "/api/remote/enable", strings.NewReader(`{"host":"box.ts.net"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// Read the raw hash-at-rest and assert the config GET never contains it.
	cfg, _ := loadConfigForDashboard(s.opts.ConfigPath)
	hashBytes, _ := os.ReadFile(remotecfg.SecretPath(cfg))
	rawHash := strings.TrimSpace(string(hashBytes))

	g := httptest.NewRequest(http.MethodGet, "/api/remote/config", nil)
	g.Host = "127.0.0.1:8080"
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, g)
	var resp struct {
		SecretPresent     bool   `json:"secret_present"`
		SecretFingerprint string `json:"secret_fingerprint"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &resp)
	if !resp.SecretPresent || resp.SecretFingerprint == "" {
		t.Fatal("config did not report the provisioned secret's presence/fingerprint")
	}
	if rawHash != "" && strings.Contains(grec.Body.String(), rawHash) {
		t.Error("config GET leaked the full hash-at-rest")
	}
}

// TestRemoteSessionsRevokeFlow exercises the live-session viewer + single revoke
// + revoke-all through the assembled handler.
func TestRemoteSessionsRevokeFlow(t *testing.T) {
	s, h := newManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	// Create two live sessions directly on the store.
	id1, _ := mc.sessions.Create()
	id2, _ := mc.sessions.Create()
	if id1 == "" || id2 == "" {
		t.Fatal("could not create sessions")
	}

	// List returns fingerprints (never full ids).
	g := httptest.NewRequest(http.MethodGet, "/api/remote/sessions", nil)
	g.Host = "127.0.0.1:8080"
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, g)
	body := grec.Body.String()
	if strings.Contains(body, id1) || strings.Contains(body, id2) {
		t.Error("sessions list leaked a full session id")
	}
	var listResp struct {
		Sessions []struct {
			Fingerprint string `json:"fingerprint"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &listResp)
	if len(listResp.Sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(listResp.Sessions))
	}
	fp := listResp.Sessions[0].Fingerprint

	// Revoke one by fingerprint.
	d := httptest.NewRequest(http.MethodDelete, "/api/remote/sessions/"+fp, nil)
	d.Host = "127.0.0.1:8080"
	drec := httptest.NewRecorder()
	h.ServeHTTP(drec, d)
	if drec.Code != http.StatusOK {
		t.Fatalf("revoke = %d: %s", drec.Code, drec.Body.String())
	}
	if mc.sessions.Count() != 1 {
		t.Errorf("want 1 session after single revoke, got %d", mc.sessions.Count())
	}

	// Revoke-all.
	p := httptest.NewRequest(http.MethodPost, "/api/remote/sessions/revoke-all", strings.NewReader("{}"))
	p.Host = "127.0.0.1:8080"
	prec := httptest.NewRecorder()
	h.ServeHTTP(prec, p)
	if prec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d", prec.Code)
	}
	if mc.sessions.Count() != 0 {
		t.Errorf("want 0 sessions after revoke-all, got %d", mc.sessions.Count())
	}
}

// postConfirm issues a Local mutation POST with the double-submit confirm token
// (cookie + header) and JSON content-type — the shape every /api/remote/* Local
// route requires.
func postConfirm(t *testing.T, h http.Handler, path, body string, ck *http.Cookie, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(ck)
	req.Header.Set(remoteConfirmHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAddDeviceKeepsSessionsWhileRotateInvalidates pins the multi-device
// invariant: "Add a device" mints a FRESH pairing secret + QR but leaves
// already-paired device sessions live (they auth by cookie, not the secret),
// while "Rotate secret" is the destructive control that unpairs everyone.
func TestAddDeviceKeepsSessionsWhileRotateInvalidates(t *testing.T) {
	s, h := newManageServer(t)
	mc := s.opts.Remote.(*remoteController)
	ck, token := getConfirm(t, h)

	// Arm remote so Rotate/AddDevice have an enabled config to mint against.
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net"}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}

	// Two devices already paired (live sessions on the real store).
	if _, err := mc.sessions.Create(); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := mc.sessions.Create(); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if mc.sessions.Count() != 2 {
		t.Fatalf("want 2 live sessions, got %d", mc.sessions.Count())
	}

	// Add a device: fresh QR, existing devices STAY connected.
	rec := postConfirm(t, h, "/api/remote/add-device", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("add-device = %d: %s", rec.Code, rec.Body.String())
	}
	var ad struct {
		PairingURL      string `json:"pairing_url"`
		PairingSecret   string `json:"pairing_secret"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ad); err != nil {
		t.Fatalf("decode add-device: %v", err)
	}
	if ad.PairingSecret == "" || !strings.Contains(ad.PairingURL, "#pair=") {
		t.Errorf("add-device must return a fresh pairing secret + URL, got %+v", ad)
	}
	if ad.RestartRequired {
		t.Error("add-device hot-reloads the live controller → restart_required must be false")
	}
	if mc.sessions.Count() != 2 {
		t.Fatalf("add-device disconnected devices (sessions %d, want 2) — it must keep existing devices connected", mc.sessions.Count())
	}

	// Rotate: the destructive control — unpairs every device.
	rec = postConfirm(t, h, "/api/remote/rotate", `{}`, ck, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body.String())
	}
	if mc.sessions.Count() != 0 {
		t.Fatalf("rotate must unpair all devices (sessions %d, want 0)", mc.sessions.Count())
	}
}
