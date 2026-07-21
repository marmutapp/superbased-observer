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

	// loopbackPred is the loopback single-user predicate (the default bind).
	// allowlistPred is a remote-exposed Host allow-list (never "allow any").
	loopbackPred := hostIsLoopback
	allowlistPred := hostAllowlistPredicate([]string{"obs.example:8080", "10.0.0.5:8081"})

	tests := []struct {
		name   string
		pred   func(string) bool
		method string
		host   string
		origin string
		want   int
	}{
		// Loopback bind — DNS-rebind + CSRF posture (unchanged).
		{"loopback GET same host", loopbackPred, http.MethodGet, "127.0.0.1:8081", "", http.StatusOK},
		{"loopback GET localhost", loopbackPred, http.MethodGet, "localhost:8081", "", http.StatusOK},
		{"loopback GET rebound attacker host", loopbackPred, http.MethodGet, "evil.com", "", http.StatusForbidden},
		{"loopback POST no origin (curl)", loopbackPred, http.MethodPost, "127.0.0.1:8081", "", http.StatusOK},
		{"loopback POST same-origin", loopbackPred, http.MethodPost, "127.0.0.1:8081", "http://127.0.0.1:8081", http.StatusOK},
		{"loopback POST cross-origin (CSRF)", loopbackPred, http.MethodPost, "127.0.0.1:8081", "https://evil.com", http.StatusForbidden},
		{"loopback POST null origin", loopbackPred, http.MethodPost, "127.0.0.1:8081", "null", http.StatusForbidden},

		// Remote-exposed bind — the dashboard.go:494 relaxation is GONE: a Host
		// not on the allow-list is rejected even on a non-loopback bind.
		{"remote GET allowed host", allowlistPred, http.MethodGet, "obs.example:8080", "", http.StatusOK},
		{"remote GET unlisted host", allowlistPred, http.MethodGet, "attacker.example", "", http.StatusForbidden},
		{"remote GET rebound loopback", allowlistPred, http.MethodGet, "127.0.0.1:8080", "", http.StatusForbidden},
		{"remote POST same allowed origin", allowlistPred, http.MethodPost, "obs.example:8080", "https://obs.example:8080", http.StatusOK},
		{"remote POST cross-origin", allowlistPred, http.MethodPost, "obs.example:8080", "https://evil.com", http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := browserGuard(next, tc.pred)
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

	// A nil predicate rejects everything (defensive).
	h := browserGuard(next, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "127.0.0.1:8080"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("nil predicate allowed a request: %d", rec.Code)
	}
}
