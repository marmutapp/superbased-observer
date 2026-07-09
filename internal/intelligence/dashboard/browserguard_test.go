package dashboard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowserGuard(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name     string
		bindAddr string
		method   string
		host     string
		origin   string
		want     int
	}{
		// Loopback bind (the default) — the DNS-rebind + CSRF posture.
		{"GET same loopback host", "127.0.0.1:8081", http.MethodGet, "127.0.0.1:8081", "", http.StatusOK},
		{"GET localhost host", "127.0.0.1:8081", http.MethodGet, "localhost:8081", "", http.StatusOK},
		{"GET rebound attacker host", "127.0.0.1:8081", http.MethodGet, "evil.com", "", http.StatusForbidden},
		{"POST no origin (curl/extension)", "127.0.0.1:8081", http.MethodPost, "127.0.0.1:8081", "", http.StatusOK},
		{"POST same-origin", "127.0.0.1:8081", http.MethodPost, "127.0.0.1:8081", "http://127.0.0.1:8081", http.StatusOK},
		{"POST cross-origin (CSRF)", "127.0.0.1:8081", http.MethodPost, "127.0.0.1:8081", "https://evil.com", http.StatusForbidden},
		{"POST null origin (sandboxed)", "127.0.0.1:8081", http.MethodPost, "127.0.0.1:8081", "null", http.StatusForbidden},
		{"DELETE cross-origin", "127.0.0.1:8081", http.MethodDelete, "127.0.0.1:8081", "https://evil.com", http.StatusForbidden},
		{"PUT rebound host beats method check", "127.0.0.1:8081", http.MethodPut, "evil.com", "http://evil.com", http.StatusForbidden},

		// Non-loopback bind — Host allow-list relaxed, CSRF check still on.
		{"non-loopback GET any host", "0.0.0.0:8081", http.MethodGet, "10.0.0.5:8081", "", http.StatusOK},
		{"non-loopback POST no origin", "0.0.0.0:8081", http.MethodPost, "10.0.0.5:8081", "", http.StatusOK},
		{"non-loopback POST same-origin", "0.0.0.0:8081", http.MethodPost, "10.0.0.5:8081", "http://10.0.0.5:8081", http.StatusOK},
		{"non-loopback POST cross-origin", "0.0.0.0:8081", http.MethodPost, "10.0.0.5:8081", "https://evil.com", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := browserGuard(next, tc.bindAddr)
			req := httptest.NewRequest(tc.method, "/api/admin/restart", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("code=%d, want %d", rec.Code, tc.want)
			}
		})
	}
}
