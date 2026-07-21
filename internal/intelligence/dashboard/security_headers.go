package dashboard

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard/webapp"
)

// Execute-tier security headers + Content-Security-Policy (remote-dashboard-
// access plan §8.1 item 7). The dashboard now hosts an execute tier (the remote
// terminal writer bridge), so the served SPA must be hardened against a
// same-origin script-injection foothold that browserGuard (Host/Origin/CSRF)
// does not cover. These headers ride EVERY response on BOTH the direct listener
// (loopback + a deliberately-armed remote bind) and the tailnet-serve backend —
// they are a transport property of the dashboard, not a per-route capability
// branch, so they wrap the outermost handler in guardedHandler +
// remoteBackendHandler.
//
// The CSP is as strict as the built Vite SPA tolerates:
//   - default-src 'self'                 — same-origin only baseline.
//   - script-src 'self' 'sha256-…'       — NO 'unsafe-inline', NO 'unsafe-eval'.
//     The only inline script the build emits is the theme pre-paint block in
//     index.html; its SHA-256 hash is computed from the embedded HTML at start-
//     up (buildCSP), so the bundled module scripts load under 'self' and the one
//     inline block loads by hash. A hash in script-src makes the browser ignore
//     any 'unsafe-inline' entirely, so an injected inline <script> is refused.
//   - style-src 'self' 'unsafe-inline'   — DOCUMENTED RESIDUAL. React inline
//     style attributes (the terminal-dock transform, the QR <img>, Recharts SVG
//     styling) and xterm.js's per-cell inline styles require inline styles; a
//     nonce/hash approach would need per-request HTML templating the embedded
//     FileServer does not do. Inline STYLE is not script-capable, so this does
//     not create a script-execution foothold (§8.1 explicitly permits a
//     documented residual, not a broken dashboard).
//   - img-src 'self' data:               — favicons ('self') + the pairing QR,
//     which qrcode.toDataURL() renders as a data: URL.
//   - connect-src 'self'                 — the /api fetches and the same-origin
//     WebSocket bridges (/ws/launch, /ws/terminal/status); CSP3 'self' matches
//     same-origin ws/wss.
//   - object-src 'none' / base-uri 'none' / frame-ancestors 'none' /
//     form-action 'self' — no plugins, no <base> injection, no framing, no
//     off-origin form posts.
//
// Nothing in the production bundle uses eval() or new Function(), so 'unsafe-
// eval' is deliberately absent (Vite's dev-only eval never ships in the build).

// inlineScriptRe matches a bare inline <script>…</script> block — one with NO
// attributes. The build's module bundles are emitted as <script type="module" …
// src=…> and never match, so only genuine inline blocks (the theme pre-paint)
// are hashed.
var inlineScriptRe = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// cspOnce computes the Content-Security-Policy exactly once (the embedded HTML
// is immutable for the process lifetime) and caches the finished header value.
var cspOnce = sync.OnceValue(func() string { return buildCSP(webapp.IndexHTML()) })

// inlineScriptHashes returns the CSP source-list tokens ('sha256-<b64>') for
// every bare inline <script> block in the given HTML, de-duplicated and sorted
// for a stable policy string. The hash is over the exact bytes between the tags,
// matching how a browser computes an inline-script hash.
func inlineScriptHashes(indexHTML []byte) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, m := range inlineScriptRe.FindAllSubmatch(indexHTML, -1) {
		sum := sha256.Sum256(m[1])
		tok := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// buildCSP assembles the Content-Security-Policy string. The script-src source
// list is 'self' plus a hash for each inline block in indexHTML; every other
// directive is fixed. A nil/empty indexHTML (broken embed) still yields a valid,
// strict policy (script-src 'self') — the app simply loses its inline theme
// pre-paint, never its security posture.
func buildCSP(indexHTML []byte) string {
	scriptSrc := append([]string{"'self'"}, inlineScriptHashes(indexHTML)...)
	directives := []string{
		"default-src 'self'",
		"base-uri 'none'",
		"object-src 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'",
		"img-src 'self' data:",
		"font-src 'self'",
		// DOCUMENTED RESIDUAL: 'unsafe-inline' for STYLE only (see file header).
		"style-src 'self' 'unsafe-inline'",
		"script-src " + strings.Join(scriptSrc, " "),
		"connect-src 'self'",
		"worker-src 'self'",
		"manifest-src 'self'",
	}
	return strings.Join(directives, "; ")
}

// securityHeaders wraps a handler so every response carries the execute-tier
// hardening headers (§8.1 item 7). It sets them BEFORE calling next, so they
// ride error responses (the 403s browserGuard/remoteAuthz emit) as well as
// success responses. For a WebSocket upgrade the pre-set header map is discarded
// when the connection is hijacked — harmless, and CSP on a 101 is irrelevant.
func securityHeaders(next http.Handler) http.Handler {
	csp := cspOnce()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
