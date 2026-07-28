package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDenyAuditDistinguishesMissingFromRejectedCookie pins the discriminator
// that turns a 401 audit row into a diagnosis.
//
// Principal collapses four distinct worlds into CapabilityPublic — no cookie
// presented, an unknown session, an expired session, a failed CSRF check — and
// the resulting `http_request … deny` row said only which capability was
// required. That is not enough to act on, because the two big cases have
// OPPOSITE fixes:
//
//   - no_cookie       → the browser never sent a credential. A cookie-attribute
//     problem (missing Max-Age, SameSite, Secure, Path) or a
//     wiped profile. Widening a server-side TTL cannot help.
//   - cookie_rejected → the browser still HAD a credential and the server
//     refused it. A session-lifecycle problem (idle/absolute
//     expiry, generation fence, restart eviction).
//
// The 2026-07-25 mobile-401 was "fixed" by widening the session TTL when the
// truth was the first case, so the defect survived that fix by two days. This
// test exists so the next occurrence is read off a row instead of re-derived.
func TestDenyAuditDistinguishesMissingFromRejectedCookie(t *testing.T) {
	tests := []struct {
		name    string
		cookie  string // "" ⇒ send no cookie at all
		want    string
		notWant string
	}{
		{
			name:    "no cookie presented",
			cookie:  "",
			want:    "no_cookie",
			notWant: "cookie_rejected",
		},
		{
			name:    "cookie presented but not a live session",
			cookie:  "not-a-real-session-token",
			want:    "cookie_rejected",
			notWant: "no_cookie",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc, _ := newReadyRemoteController(t)
			var records []RemoteAuditRecord
			s := newRemoteTestServer(t, Options{
				Remote:      rc,
				RemoteAudit: func(r RemoteAuditRecord) { records = append(records, r) },
			})

			// /api/status is a View route, so an unauthenticated caller is
			// denied and audited — the exact shape the operator's phone hit.
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			req.Host = testRemoteHost
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: remoteSessionCookie, Value: tc.cookie})
			}
			rec := httptest.NewRecorder()
			s.remoteGuardedHandler(rc).ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d (anonymous on a View route)", rec.Code, http.StatusUnauthorized)
			}
			if len(records) == 0 {
				t.Fatal("the denial wrote no audit record at all")
			}
			last := records[len(records)-1]
			if last.Decision != "deny" {
				t.Fatalf("audit decision = %q, want \"deny\"", last.Decision)
			}
			if !strings.Contains(last.Detail, tc.want) {
				t.Errorf("audit detail = %q, want it to contain %q", last.Detail, tc.want)
			}
			if strings.Contains(last.Detail, tc.notWant) {
				t.Errorf("audit detail = %q must NOT contain %q — the two cases have opposite fixes",
					last.Detail, tc.notWant)
			}
			// The required capability must survive alongside the new reason;
			// the reason is additive, not a replacement.
			if !strings.Contains(last.Detail, CapabilityView.String()) {
				t.Errorf("audit detail = %q lost the required-capability token %q",
					last.Detail, CapabilityView.String())
			}
		})
	}
}
