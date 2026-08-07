package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

const testRemoteHost = "remote.example:8443"

func newRemoteTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	opts.DB = database
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// newReadyRemoteController builds a real, Ready() controller with a known
// pairing secret; returns the controller + the base64url secret to pair with.
func newReadyRemoteController(t *testing.T) (RemoteController, string) {
	return newReadyRemoteControllerWithTerminal(t, false)
}

func newReadyRemoteControllerWithTerminal(t *testing.T, allowTerminal bool) (RemoteController, string) {
	t.Helper()
	raw, enc, err := remoteauth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	hash, err := remoteauth.HashSecret(raw)
	if err != nil {
		t.Fatalf("HashSecret: %v", err)
	}
	rc := NewRemoteController(RemoteOptions{
		HashedSecret:    hash,
		AllowedHosts:    []string{testRemoteHost},
		RateLimitPerMin: 6,
		AllowTerminal:   allowTerminal,
		Session:         remoteauth.SessionParams{TTL: time.Hour, Idle: time.Hour, Max: 5},
	})
	if !rc.Ready() {
		t.Fatal("controller not Ready with secret + allowlist")
	}
	return rc, enc
}

// TestEveryRouteClassifiedOrFailClosed pins plan §4.1: every built-in route is
// registered with a non-Unclassified capability.
func TestEveryRouteClassifiedOrFailClosed(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	_, capMap := s.registerRoutes(nil)
	if len(capMap) < 50 {
		t.Fatalf("suspiciously few routes classified (%d) — registry may not be wired", len(capMap))
	}
	for pattern, cap := range capMap {
		if cap == CapabilityUnclassified {
			t.Errorf("route %q is UNCLASSIFIED — every route must be public/view/execute (fail closed)", pattern)
		}
	}
}

// TestExtraRoutesRejectedWithoutCapabilityMetadata pins plan §4.1 / codex P2
// #25: an ExtraRoute lacking capability metadata is rejected by New() when a
// RemoteController is present.
func TestExtraRoutesRejectedWithoutCapabilityMetadata(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	database, err := openTestDB(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	_, err = New(Options{
		DB:     database,
		Remote: rc,
		ExtraRoutes: []ExtraRoute{{
			Pattern: "/api/plugin/thing",
			Handler: func(w http.ResponseWriter, _ *http.Request) {},
			// Capability deliberately unset.
		}},
	})
	if err == nil {
		t.Fatal("New accepted an unclassified ExtraRoute alongside a RemoteController")
	}

	// With a capability set, it is accepted.
	if _, err := New(Options{
		DB:     database,
		Remote: rc,
		ExtraRoutes: []ExtraRoute{{
			Pattern:    "/api/plugin/thing",
			Handler:    func(w http.ResponseWriter, _ *http.Request) {},
			Capability: CapabilityView,
		}},
	}); err != nil {
		t.Fatalf("New rejected a classified ExtraRoute: %v", err)
	}

	// Loopback-only (no controller): an unclassified ExtraRoute is tolerated.
	if _, err := New(Options{
		DB: database,
		ExtraRoutes: []ExtraRoute{{
			Pattern: "/api/plugin/thing",
			Handler: func(w http.ResponseWriter, _ *http.Request) {},
		}},
	}); err != nil {
		t.Fatalf("loopback-only New rejected an unclassified ExtraRoute: %v", err)
	}
}

// TestHostAllowlistEnforcedOnNonLoopback pins plan §4.5: the remote-exposed
// handler rejects a Host not on the allow-list (the dashboard.go:494 fix).
func TestHostAllowlistEnforcedOnNonLoopback(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)

	// Unlisted Host → 403 before anything.
	req := httptest.NewRequest(http.MethodGet, "/api/remote/whoami", nil)
	req.Host = "attacker.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unlisted Host = %d, want 403", rec.Code)
	}

	// Allowed Host → passes the host guard (whoami is Public → 200).
	req = httptest.NewRequest(http.MethodGet, "/api/remote/whoami", nil)
	req.Host = testRemoteHost
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("allowed Host whoami = %d, want 200", rec.Code)
	}
}

// TestWSUpgradeRejectsMismatchedOrigin pins plan §4.5: a WebSocket upgrade with
// a mismatched Origin is rejected by browserGuard (in addition to
// coder/websocket.Accept's own cross-origin reject).
func TestWSUpgradeRejectsMismatchedOrigin(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)

	req := httptest.NewRequest(http.MethodGet, "/ws/launch/abc", nil)
	req.Host = testRemoteHost
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("WS upgrade with mismatched Origin = %d, want 403", rec.Code)
	}
}

// TestSecretNeverInQueryOrSubprotocol pins plan §4.3/§4.7 / codex P1 #7: the
// pairing secret is read from the JSON body only — never a query param — and WS
// auth never consults Sec-WebSocket-Protocol.
func TestSecretNeverInQueryOrSubprotocol(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)

	// Secret in a QUERY param + empty body → rejected (not read).
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair?secret="+enc, strings.NewReader(`{}`))
	req.Host = testRemoteHost
	req.Header.Set("Origin", "https://"+testRemoteHost)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Error("pairing succeeded with the secret in a query param — it must be body-only")
	}

	// A WS upgrade carrying a 'secret' subprotocol but NO session cookie →
	// still unauthenticated (401), proving the subprotocol is never an auth
	// channel.
	req = httptest.NewRequest(http.MethodGet, "/ws/launch/abc", nil)
	req.Host = testRemoteHost
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Origin", "https://"+testRemoteHost)
	req.Header.Set("Sec-WebSocket-Protocol", "secret."+enc)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("WS with a secret subprotocol authenticated (%d) — subprotocols must never carry auth", rec.Code)
	}
}

// pairSession pairs via the real flow and returns the session cookie + CSRF.
func pairSession(t *testing.T, h http.Handler, enc string) (*http.Cookie, string) {
	t.Helper()
	body := `{"secret":"` + enc + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/remote/pair", strings.NewReader(body))
	req.Host = testRemoteHost
	req.Header.Set("Origin", "https://"+testRemoteHost)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pair failed: %d %s", rec.Code, rec.Body.String())
	}
	var pr struct {
		OK   bool   `json:"ok"`
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil || !pr.OK {
		t.Fatalf("pair response: %v %s", err, rec.Body.String())
	}
	var cookie *http.Cookie
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == remoteSessionCookie {
			cookie = ck
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie set on pair")
	}
	return cookie, pr.CSRF
}

// TestRemoteAuthzMatrix is the assembled-server matrix (plan §7 Phase 1 / codex
// P2 #28): anonymous and view principals must fail every execute/local-only
// route. It drives the FULL remoteGuardedHandler chain (host guard + authz +
// mux + controller routes) — the identical assembly `observer dashboard` and
// `observer start` both construct, so exercising the assembled Server covers
// both entrypoints.
func TestRemoteAuthzMatrix(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)

	viewGetRoutes := []string{"/api/status", "/api/sessions", "/api/cost", "/api/attach/sessions"}
	// Execute-required: unsafe methods on View routes + the terminal GET.
	type mreq struct {
		method string
		path   string
	}
	executeRoutes := []mreq{
		{http.MethodPost, "/api/admin/restart"},
		{http.MethodPost, "/api/suggestions/state"},
		{http.MethodPost, "/api/scan/run"},
		{http.MethodDelete, "/api/launch/abc"},
		// POST /api/session/<id>/resume is an Execute sub-route (session-attach
		// Phase 3, sessionSubRouteCapabilities): a view principal must be refused.
		{http.MethodPost, "/api/session/abc/resume"},
	}

	do := func(method, path string, cookie *http.Cookie, csrf string) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Host = testRemoteHost
		if isUnsafeMethod(method) {
			req.Header.Set("Origin", "https://"+testRemoteHost)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if csrf != "" {
			req.Header.Set(remoteCSRFHeader, csrf)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// --- Anonymous principal: fails everything protected. ---
	for _, p := range viewGetRoutes {
		if code := do(http.MethodGet, p, nil, ""); code != http.StatusUnauthorized {
			t.Errorf("anon GET %s = %d, want 401", p, code)
		}
	}
	for _, m := range executeRoutes {
		if code := do(m.method, m.path, nil, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
			t.Errorf("anon %s %s = %d, want 401/403", m.method, m.path, code)
		}
	}

	// --- View principal (paired session + CSRF, no execute capability). ---
	cookie, csrf := pairSession(t, h, enc)
	for _, p := range viewGetRoutes {
		if code := do(http.MethodGet, p, cookie, csrf); code == http.StatusUnauthorized || code == http.StatusForbidden {
			t.Errorf("view GET %s = %d, want it ALLOWED through authz", p, code)
		}
	}
	for _, m := range executeRoutes {
		if code := do(m.method, m.path, cookie, csrf); code != http.StatusForbidden {
			t.Errorf("view principal reached execute route %s %s = %d, want 403", m.method, m.path, code)
		}
	}
	// /ws/launch GET is View: paired devices connect read-only; writer role is gated in-band.
	if code := do(http.MethodGet, "/ws/launch/abc", nil, ""); code != http.StatusUnauthorized && code != http.StatusForbidden {
		t.Errorf("anon GET /ws/launch/abc = %d, want 401/403", code)
	}
	if code := do(http.MethodGet, "/ws/launch/abc", cookie, csrf); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("view GET /ws/launch/abc = %d, want it ALLOWED through authz", code)
	}

	// whoami reports the view principal as authenticated.
	{
		req := httptest.NewRequest(http.MethodGet, "/api/remote/whoami", nil)
		req.Host = testRemoteHost
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), `"authenticated":true`) {
			t.Errorf("whoami for view principal = %s", rec.Body.String())
		}
	}
}

// TestExecutePrincipalReachesExecuteRoute proves the execute tier works: a
// minted single-use capability lets one execute-route request through, and only
// once.
func TestExecutePrincipalReachesExecuteRoute(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)
	cookie, csrf := pairSession(t, h, enc)

	// Mint an execute capability for the exact (session, action). Use a
	// genuinely Execute-tier route (/api/terminal/launch) — the maintenance
	// mutations like /api/scan/run are now Local (refused on the remote listener
	// before the principal is even resolved, so an execute capability can never
	// reach them).
	mc, ok := rc.(*remoteController)
	if !ok {
		t.Fatal("expected *remoteController")
	}
	action := http.MethodPost + " /api/terminal/launch"
	tok, err := mc.MintExecute(cookie.Value, action)
	if err != nil {
		t.Fatalf("MintExecute: %v", err)
	}

	call := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/terminal/launch", strings.NewReader("{}"))
		req.Host = testRemoteHost
		req.Header.Set("Origin", "https://"+testRemoteHost)
		req.AddCookie(cookie)
		req.Header.Set(remoteCSRFHeader, csrf)
		req.Header.Set(remoteExecuteHeader, tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// First call: authorized through authz (handler may 4xx internally, but it
	// must NOT be the authz 401/403).
	if code := call(); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Errorf("execute principal blocked by authz on first use: %d", code)
	}
	// Second call: capability consumed → back to view → 403.
	if code := call(); code != http.StatusForbidden {
		t.Errorf("execute capability was reusable: second call = %d, want 403", code)
	}
}

func TestRemoteViewCanLaunchFreshTerminalWhenAllowed(t *testing.T) {
	rc, enc := newReadyRemoteControllerWithTerminal(t, true)
	lm := &fakeLaunchManager{}
	s := newRemoteTestServer(t, Options{Remote: rc, LaunchManager: lm})
	h := s.remoteGuardedHandler(rc)
	cookie, csrf := pairSession(t, h, enc)

	req := httptest.NewRequest(http.MethodPost, "/api/terminal/launch",
		strings.NewReader(`{"tool":"claude-code"}`))
	req.Host = testRemoteHost
	req.Header.Set("Origin", "https://"+testRemoteHost)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set(remoteCSRFHeader, csrf)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("paired remote view was blocked from fresh terminal launch: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh terminal launch = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if lm.lastFreshSpec.Tool != "claude-code" {
		t.Fatalf("fresh launch not reached: %+v", lm.lastFreshSpec)
	}
}
