package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard/webapp"
)

// TestSecurityHeadersPresent pins that the securityHeaders middleware emits the
// execute-tier hardening headers on every response (plan §8.1 item 7),
// regardless of the wrapped handler's status.
func TestSecurityHeadersPresent(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("Content-Security-Policy header is missing")
	}
}

// TestExecuteTierCSP pins the exact shape of the shipped Content-Security-Policy:
// no unsafe-eval anywhere, no unsafe-inline in script-src, and the strict
// directives that harden the execute tier. It also proves the CSP is not so
// strict it forbids the bundle's own scripts/styles.
func TestExecuteTierCSP(t *testing.T) {
	csp := cspOnce()

	// Hard prohibitions: an XSS foothold must not find eval, and script-src must
	// never fall back to 'unsafe-inline'.
	if strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP must never allow 'unsafe-eval': %q", csp)
	}
	script := cspDirective(t, csp, "script-src")
	if strings.Contains(script, "unsafe-inline") {
		t.Errorf("script-src must not allow 'unsafe-inline' (defeats the hash): %q", script)
	}
	if !strings.Contains(script, "'self'") {
		t.Errorf("script-src must allow 'self' for the bundled module scripts: %q", script)
	}

	// Fixed strict directives.
	for _, want := range []string{
		"default-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"img-src 'self' data:",
		"connect-src 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %q", want, csp)
		}
	}

	// Documented residual: style-src carries 'unsafe-inline' (React/Recharts/
	// xterm inline styles). This assertion documents-and-pins the residual so a
	// future tightening is a conscious, test-visible change.
	style := cspDirective(t, csp, "style-src")
	if !strings.Contains(style, "unsafe-inline") {
		t.Errorf("style-src is expected to carry the documented 'unsafe-inline' residual: %q", style)
	}
}

// TestCSPCoversEmbeddedInlineScripts proves the CSP stays in lock-step with the
// actual built HTML: EVERY bare inline <script> block in the embedded
// index.html has its SHA-256 hash present in script-src. If the build's inline
// theme pre-paint script changes without the CSP being recomputed, this fails
// loudly instead of silently blocking the script (and reintroducing the theme
// flash) in production.
func TestCSPCoversEmbeddedInlineScripts(t *testing.T) {
	idx := webapp.IndexHTML()
	if len(idx) == 0 {
		t.Skip("embedded index.html unavailable (broken embed) — nothing to cover")
	}
	hashes := inlineScriptHashes(idx)
	// The current build ships exactly one inline block (theme pre-paint). Guard
	// the expectation without over-constraining a future no-inline build.
	if strings.Contains(string(idx), "<script>") && len(hashes) == 0 {
		t.Fatal("index.html contains a bare inline <script> but inlineScriptHashes found none")
	}
	script := cspDirective(t, cspOnce(), "script-src")
	for _, hstr := range hashes {
		if !strings.Contains(script, hstr) {
			t.Errorf("script-src is missing the embedded inline-script hash %s: %q", hstr, script)
		}
	}
}

// TestGuardedHandlerEmitsSecurityHeaders exercises the real loopback serving
// path (guardedHandler → securityHeaders) end-to-end so a future refactor that
// drops the wrapper is caught.
func TestGuardedHandlerEmitsSecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.guardedHandler("127.0.0.1:0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "127.0.0.1"
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("guardedHandler did not emit the CSP header on a served response")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("guardedHandler did not emit X-Frame-Options: DENY")
	}
}

// cspDirective extracts the single named directive's value from a CSP string
// (the "; "-joined form buildCSP emits), failing the test if it is absent.
func cspDirective(t *testing.T, csp, name string) string {
	t.Helper()
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if strings.HasPrefix(d, name+" ") || d == name {
			return d
		}
	}
	t.Fatalf("CSP has no %q directive: %q", name, csp)
	return ""
}
