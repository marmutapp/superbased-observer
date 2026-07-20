//go:build !no_obs

package main

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/obs/httpapi"
)

// TestObsRouteCapability pins the codex-finding-1 fix
// (dashboard-management-surface plan §9.4): every obs MUTATION-method route (a
// POST) is classified Local (an owner side effect must never ride the remote
// listener as an auto-escalating View), while every obs read stays View.
func TestObsRouteCapability(t *testing.T) {
	cases := map[string]dashboard.Capability{
		"POST /api/obs/admission/policy": dashboard.CapabilityLocal,
		"POST /api/obs/admission/check":  dashboard.CapabilityLocal,
		"GET /api/obs/admission/policy":  dashboard.CapabilityView,
		"GET /api/obs/traces":            dashboard.CapabilityView,
		"GET /api/obs/admission/status":  dashboard.CapabilityView,
		"GET /api/obs/egress/decisions":  dashboard.CapabilityView,
	}
	for pattern, want := range cases {
		if got := obsRouteCapability(pattern); got != want {
			t.Errorf("obsRouteCapability(%q) = %s, want %s", pattern, got, want)
		}
	}
}

// TestObsMutationRoutesAreLocal is the REGRESSION test for the daemon-startup
// brick: obs mounts its httpapi routes as dashboard ExtraRoutes, and
// dashboard.New fails closed if ANY mutation-method ExtraRoute is left as View
// (plan §9.4). The prior bug shipped because the classification was tested
// against a HARDCODED expectation, not the live route table — so a POST route
// classified View passed the unit test but crashed `dashboard.New` at real
// daemon startup. This test drives the assertion from the ACTUAL
// httpapi.Routes() list, so any NEW obs POST/PUT/DELETE route that isn't
// classified Local fails here instead of at a user's daemon boot.
func TestObsMutationRoutesAreLocal(t *testing.T) {
	// nil deps are fine: Routes() only enumerates the static pattern→handler
	// table, it does not touch the store/enricher/admission.
	api := httpapi.New(nil, nil, nil, slog.Default())
	for _, r := range api.Routes() {
		method, _, ok := strings.Cut(r.Pattern, " ")
		if !ok {
			t.Fatalf("route pattern %q is not method-prefixed", r.Pattern)
		}
		isMutation := method == "POST" || method == "PUT" || method == "DELETE" || method == "PATCH"
		if !isMutation {
			continue
		}
		if got := obsRouteCapability(r.Pattern); got != dashboard.CapabilityLocal {
			t.Errorf("mutation-method obs route %q is classified %s — must be CapabilityLocal, "+
				"or dashboard.New will fail closed at daemon startup (plan §9.4)", r.Pattern, got)
		}
	}
}
