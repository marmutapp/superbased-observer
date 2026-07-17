package dashboard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTailnetBackendRequiresAuthDespiteLoopback pins plan §4.4: the
// tailnet-serve backend is classified remote-exposed AT CONSTRUCTION, so it
// requires auth for EVERY request even though the peer (tailscale serve) is
// loopback — there is NO RemoteAddr-based owner-trust bypass.
func TestTailnetBackendRequiresAuthDespiteLoopback(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteBackendHandler(rc)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = testRemoteHost
	req.RemoteAddr = "127.0.0.1:54321" // proxied loopback (tailscale serve)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous proxied-loopback GET /api/status = %d, want 401 (no RemoteAddr bypass)", rec.Code)
	}
}

// TestBackendStripsForwardedIdentityHeaders pins plan §4.4: the forwarded
// identity headers are consumed ONLY on the backend socket and STRIPPED before
// any downstream handler runs, so a spoofed copy can never be read. The plain
// remoteGuardedHandler (no backend wrapper — models the direct listener) does
// NOT strip, proving capture/strip is backend-socket-only.
func TestBackendStripsForwardedIdentityHeaders(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	echo := ExtraRoute{
		Pattern:    "/api/echo-tsheader",
		Capability: CapabilityView,
		Handler: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(r.Header.Get("Tailscale-User-Login")))
		},
	}
	s := newRemoteTestServer(t, Options{Remote: rc, ExtraRoutes: []ExtraRoute{echo}})

	backend := s.remoteBackendHandler(rc)
	cookie, csrf := pairSession(t, backend, enc)

	get := func(h http.Handler) string {
		req := httptest.NewRequest(http.MethodGet, "/api/echo-tsheader", nil)
		req.Host = testRemoteHost
		req.Header.Set("Tailscale-User-Login", "alice@example.com")
		req.AddCookie(cookie)
		req.Header.Set(remoteCSRFHeader, csrf)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("echo route = %d, want 200 (view GET)", rec.Code)
		}
		return rec.Body.String()
	}

	if got := get(backend); got != "" {
		t.Errorf("backend did NOT strip Tailscale-User-Login: downstream saw %q", got)
	}
	// The plain (non-backend) handler is the direct-listener model: it never
	// strips because it never captures — the header passes straight through.
	if got := get(s.remoteGuardedHandler(rc)); got != "alice@example.com" {
		t.Errorf("direct-listener handler unexpectedly altered the header: %q", got)
	}
}

// TestBackendCapturesTailnetIdentityForAudit pins that the tailnet login is
// captured into the audit trail on the backend socket ONLY (audit-only in
// Phase 2 — never an auth channel), and is absent on the direct listener.
func TestBackendCapturesTailnetIdentityForAudit(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	var records []RemoteAuditRecord
	s := newRemoteTestServer(t, Options{
		Remote:      rc,
		RemoteAudit: func(r RemoteAuditRecord) { records = append(records, r) },
	})

	send := func(h http.Handler) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil) // View → anon denied+audited
		req.Host = testRemoteHost
		req.Header.Set("Tailscale-User-Login", "bob@example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	records = nil
	send(s.remoteBackendHandler(rc))
	if len(records) == 0 || !strings.Contains(records[len(records)-1].Detail, "tailnet:bob@example.com") {
		t.Errorf("backend request did not capture the tailnet identity in audit: %+v", records)
	}

	records = nil
	send(s.remoteGuardedHandler(rc))
	for _, r := range records {
		if strings.Contains(r.Detail, "tailnet:") {
			t.Errorf("direct-listener request captured a tailnet identity (must be ignored off the backend): %+v", r)
		}
	}
}

// TestFunnelRefusesExecuteRoute pins plan §9: a request arriving over tailscale
// funnel (public) is refused for execute-tier routes outright — even with a
// valid execute capability — and the refusal does NOT consume that single-use
// capability (the same capability still works on a non-funnel request).
func TestFunnelRefusesExecuteRoute(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	backend := s.remoteBackendHandler(rc)
	cookie, csrf := pairSession(t, backend, enc)

	mc := rc.(*remoteController)
	// Use a genuinely Execute-tier route (/api/terminal/launch) — the
	// maintenance mutations like /api/scan/run are now CapabilityLocal (refused
	// before the principal resolves, on funnel and non-funnel alike).
	action := http.MethodPost + " /api/terminal/launch"
	tok, err := mc.MintExecute(cookie.Value, action)
	if err != nil {
		t.Fatalf("MintExecute: %v", err)
	}

	call := func(funnel bool) int {
		req := httptest.NewRequest(http.MethodPost, "/api/terminal/launch", strings.NewReader("{}"))
		req.Host = testRemoteHost
		req.Header.Set("Origin", "https://"+testRemoteHost)
		req.AddCookie(cookie)
		req.Header.Set(remoteCSRFHeader, csrf)
		req.Header.Set(remoteExecuteHeader, tok)
		if funnel {
			req.Header.Set(tailnetFunnelHeader, "?1")
		}
		rec := httptest.NewRecorder()
		backend.ServeHTTP(rec, req)
		return rec.Code
	}

	// Over funnel: execute route refused (403), capability NOT consumed.
	if code := call(true); code != http.StatusForbidden {
		t.Fatalf("funnel execute request = %d, want 403", code)
	}
	// Same capability over a normal (non-funnel) tailnet request: authorized
	// through authz on first use (proves the funnel refusal didn't consume it).
	if code := call(false); code == http.StatusUnauthorized || code == http.StatusForbidden {
		t.Fatalf("non-funnel execute request blocked by authz (%d) — funnel refusal wrongly consumed the capability", code)
	}
}

// TestTailnetBackendRefusesWithoutReadySubstrate pins that the backend listener
// fails closed when [remote] is not Ready() and when the addr is non-loopback.
func TestTailnetBackendRefusesWithoutReadySubstrate(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	if err := s.ListenAndServeTailnetBackend(t.Context(), "127.0.0.1:0"); err == nil {
		t.Error("backend started without a Ready() substrate — must fail closed")
	}

	rc, _ := newReadyRemoteController(t)
	s2 := newRemoteTestServer(t, Options{Remote: rc})
	if err := s2.ListenAndServeTailnetBackend(t.Context(), "0.0.0.0:9999"); err == nil {
		t.Error("backend bound a non-loopback addr — the tailnet backend must be loopback-only")
	}
}

// TestTailnetBackendServesAndShutsDown drives the live serve path: with a
// Ready() substrate the backend binds a loopback port, serves over a REAL
// socket (an anonymous request gets the authz 401, never a loopback trust
// pass), and shuts down cleanly on ctx-cancel.
func TestTailnetBackendServesAndShutsDown(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})

	addr := reserveLoopbackPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServeTailnetBackend(ctx, addr) }()

	// Wait for the listener, then hit it anonymously with the allow-listed Host.
	var resp *http.Response
	for range 100 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/status", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Host = testRemoteHost
		resp, err = http.DefaultClient.Do(req)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("backend never came up")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous request over the live backend socket = %d, want 401 (auth required despite loopback peer)", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("backend shutdown returned %v, want nil on ctx-cancel", err)
	}
}

// reserveLoopbackPort grabs a free loopback port and releases it for the server
// under test to rebind (the same reserve-then-rebind model the CLI uses).
func reserveLoopbackPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}
