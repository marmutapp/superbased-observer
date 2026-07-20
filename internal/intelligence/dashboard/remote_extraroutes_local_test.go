package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/db"
)

func noopHandler(http.ResponseWriter, *http.Request) {}

// TestExtraRoutesLocalPropagation is the codex-finding-1 backstop (plan §9.4): a
// mutation-method ExtraRoute must be Local unless allowlisted. A View/Execute
// mutation ExtraRoute is rejected build-time; a Local one (or a read GET) is
// accepted.
func TestExtraRoutesLocalPropagation(t *testing.T) {
	rc, _ := newReadyRemoteController(t)
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(t.TempDir(), "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	newWith := func(rt ExtraRoute) error {
		_, e := New(Options{DB: database, Remote: rc, ExtraRoutes: []ExtraRoute{rt}})
		return e
	}

	cases := []struct {
		name    string
		route   ExtraRoute
		wantErr bool
	}{
		{"mutation POST as View rejected", ExtraRoute{Pattern: "POST /api/plugin/write", Handler: noopHandler, Capability: CapabilityView}, true},
		{"mutation POST as Execute rejected", ExtraRoute{Pattern: "POST /api/plugin/write", Handler: noopHandler, Capability: CapabilityExecute}, true},
		{"mutation PUT as View rejected", ExtraRoute{Pattern: "PUT /api/plugin/write", Handler: noopHandler, Capability: CapabilityView}, true},
		{"mutation DELETE as View rejected", ExtraRoute{Pattern: "DELETE /api/plugin/write", Handler: noopHandler, Capability: CapabilityView}, true},
		{"mutation POST as Local accepted", ExtraRoute{Pattern: "POST /api/plugin/write", Handler: noopHandler, Capability: CapabilityLocal}, false},
		{"read GET as View accepted", ExtraRoute{Pattern: "GET /api/plugin/read", Handler: noopHandler, Capability: CapabilityView}, false},
		{"bare pattern as View accepted", ExtraRoute{Pattern: "/api/plugin/read", Handler: noopHandler, Capability: CapabilityView}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := newWith(tc.route)
			if tc.wantErr && err == nil {
				t.Fatalf("New accepted %q as %s — a mutation ExtraRoute must be Local or allowlisted", tc.route.Pattern, tc.route.Capability)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("New rejected %q as %s: %v", tc.route.Pattern, tc.route.Capability, err)
			}
		})
	}
}

// TestPatternHasUnsafeMethod pins the method-prefix predicate the §9.4 gate uses.
func TestPatternHasUnsafeMethod(t *testing.T) {
	cases := map[string]bool{
		"POST /api/x":   true,
		"PUT /api/x":    true,
		"DELETE /api/x": true,
		"PATCH /api/x":  true,
		"GET /api/x":    false,
		"HEAD /api/x":   false,
		"/api/x":        false, // bare (methodless) — governed by its own cap
		"":              false,
	}
	for pattern, want := range cases {
		if got := patternHasUnsafeMethod(pattern); got != want {
			t.Errorf("patternHasUnsafeMethod(%q) = %v, want %v", pattern, got, want)
		}
	}
}

// TestServeMuxShadowing pins codex finding 1's exact-vs-prefix resolution: the
// capability actually enforced is the one on the MORE-SPECIFIC matched pattern.
func TestServeMuxShadowing(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	mux, capMap := s.registerRoutes(nil)

	cases := []struct {
		name        string
		method      string
		path        string
		wantPattern string
		wantCap     Capability
	}{
		// The more-specific config/section/ subtree (Local) must win over the
		// exact /api/config (View) read — the core exact-vs-prefix guarantee.
		{"config section is more specific than config", http.MethodPut, "/api/config/section/foo", "/api/config/section/", CapabilityLocal},
		{"config exact read", http.MethodGet, "/api/config", "/api/config", CapabilityView},
		// The exact /api/guard/approvals (Local) is distinct from the
		// /api/guard/approvals/ subtree (Local); a sub-path resolves the subtree.
		{"guard approvals exact", http.MethodPost, "/api/guard/approvals", "/api/guard/approvals", CapabilityLocal},
		{"guard approvals subtree", http.MethodDelete, "/api/guard/approvals/abc", "/api/guard/approvals/", CapabilityLocal},
		// The exact /api/config/profiles (create, Local) vs the
		// /api/config/profiles/ subtree (mutate/delete, Local).
		{"profiles exact create", http.MethodPost, "/api/config/profiles", "/api/config/profiles", CapabilityLocal},
		{"profiles subtree", http.MethodDelete, "/api/config/profiles/xyz", "/api/config/profiles/", CapabilityLocal},
		// A View read under the /api/session/ subtree resolves the base pattern.
		{"session subtree read", http.MethodGet, "/api/session/abc/messages", "/api/session/", CapabilityView},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			_, pattern := mux.Handler(req)
			if pattern != tc.wantPattern {
				t.Fatalf("mux.Handler(%s %s) matched %q, want %q", tc.method, tc.path, pattern, tc.wantPattern)
			}
			if cap := capMap[pattern]; cap != tc.wantCap {
				t.Fatalf("capMap[%q] = %s, want %s — the enforced capability must be the more-specific route's", pattern, cap, tc.wantCap)
			}
		})
	}
}
