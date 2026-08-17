package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/govern"
	"github.com/marmutapp/superbased-observer/internal/policyfam/nodegov"
)

// loopbackRequest builds a request the loopback browserGuard accepts: same
// Host as the bind, and an Origin matching it so the CSRF check passes for
// unsafe methods. The governance guard must be provably reached through the
// REAL loopback chain, not around it.
func loopbackRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Host = "127.0.0.1:8081"
	r.Header.Set("Origin", "http://127.0.0.1:8081")
	return r
}

// governedProvider builds a GovernanceProvider that returns an ACTIVE
// posture hiding/locking the given ids — the shape a real node resolves when
// its grant carries dashboard.visibility and the org published a body.
func governedProvider(t *testing.T, hidden, readOnly, settingsHidden, settingsReadOnly []string) GovernanceProvider {
	t.Helper()
	spec, err := nodegov.Compile(nodegov.PolicyInput{
		HiddenSections:   hidden,
		ReadOnlySections: readOnly,
		HiddenSettings:   settingsHidden,
		ReadOnlySettings: settingsReadOnly,
		Notice:           nodegov.Notice{OrgDisplayName: "Acme Platform Engineering"},
	})
	if err != nil {
		t.Fatalf("nodegov.Compile: %v", err)
	}
	now := time.Now().UTC()
	grant := &govern.Grant{
		OrgKey: "ok", Generation: 1, OrgName: "Acme", KeyPinSHA256: "pin",
		Authority: []string{govern.AuthorityDashboardVisibility},
		ExpiresAt: now.Add(24 * time.Hour),
	}
	live := govern.LiveIdentity{Enrolled: true, OrgKey: "ok", Generation: 1, KeyPinSHA256: "pin"}
	delivered := govern.Delivered{Present: true, Version: 14, BodyHash: "bh", Spec: spec}
	eff := govern.Resolve(delivered, grant, live, now)
	if !eff.Active || eff.State != govern.StateApplied {
		t.Fatalf("test fixture did not resolve to an applied posture: %+v", eff)
	}
	return func(context.Context) govern.Effective { return eff }
}

// TestGovernanceAPIRefusesHiddenRoutes is THE load-bearing test of Phase 1a.
//
// It drives the LOOPBACK listener chain — s.guardedHandler on a loopback
// address, with no RemoteController — which is exactly where the adversarial
// review found the enforcement would have been missing: capMap (the obvious
// place to hang a second predicate) is consulted only by remoteAuthz, and
// Server.Handler() discards it. A UI-only hide, or a guard installed only on
// the remote branch, fails here.
func TestGovernanceAPIRefusesHiddenRoutes(t *testing.T) {
	s := newRemoteTestServer(t, Options{
		Governance: governedProvider(
			t,
			[]string{"benchmarks", "terminals"}, // hidden nav sections
			[]string{"policies"},                // read-only nav section
			[]string{"process"},                 // hidden settings section
			[]string{"guard"},                   // read-only settings section
		),
	})
	// The loopback branch: no remote controller, a loopback bind.
	h := s.guardedHandler("127.0.0.1:8081")

	cases := []struct {
		name   string
		method string
		path   string
		want   int
		code   string
	}{
		{"hidden section GET is 404", http.MethodGet, "/api/benchmarks", http.StatusNotFound, "governance_hidden"},
		{"hidden section subtree GET is 404", http.MethodGet, "/api/benchmarks/x", http.StatusNotFound, "governance_hidden"},
		{"hidden terminals route is 404", http.MethodGet, "/api/terminal/sessions", http.StatusNotFound, "governance_hidden"},
		{"read-only section mutation is 409", http.MethodPut, "/api/guard/policy", http.StatusConflict, "governance_read_only"},
		{"hidden settings section write is 404", http.MethodPut, "/api/config/section/process", http.StatusNotFound, "governance_hidden"},
		{"read-only settings section write is 409", http.MethodPut, "/api/config/section/guard", http.StatusConflict, "governance_read_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, loopbackRequest(tc.method, tc.path))
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (body %s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
			var refusal governanceRefusal
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("refusal body is not JSON: %v (%s)", err, rec.Body.String())
			}
			if refusal.Error != tc.code {
				t.Fatalf("refusal code = %q, want %q", refusal.Error, tc.code)
			}
			if refusal.Message == "" || refusal.OrgName == "" {
				t.Fatalf("refusal must NAME its cause and the organization, got %+v", refusal)
			}
		})
	}
}

// TestGovernanceAPIAllowsUngovernedRoutes is the other half: governance
// refuses what it was told to refuse and NOTHING else. Without it the guard
// could pass the test above by refusing everything.
func TestGovernanceAPIAllowsUngovernedRoutes(t *testing.T) {
	s := newRemoteTestServer(t, Options{
		Governance: governedProvider(t, []string{"benchmarks"}, []string{"policies"}, nil, nil),
	})
	h := s.guardedHandler("127.0.0.1:8081")

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/status"},                  // secNone, never hidden
		{http.MethodGet, "/api/sessions"},                // a governed-but-visible section
		{http.MethodGet, "/api/guard/policy"},            // read-only section: READS still work
		{http.MethodGet, "/api/governance"},              // the honesty endpoint itself
		{http.MethodPut, "/api/config/section/observer"}, // an ungoverned settings section
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest(tc.method, tc.path))
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusConflict {
			var refusal governanceRefusal
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err == nil && refusal.Family == "node.governance" {
				t.Fatalf("%s %s was refused by governance (%s) but is not governed", tc.method, tc.path, refusal.Error)
			}
		}
	}
}

// TestGovernanceCannotHideItsOwnEvidence is T8 at the ROUTE level: even if a
// posture somehow named them, the routes a developer needs in order to see
// what is happening to them keep working. (The compiler refuses such a body
// outright — this is the second, independent line.)
func TestGovernanceCannotHideItsOwnEvidence(t *testing.T) {
	eff := govern.Effective{
		Active: true, State: govern.StateApplied, Version: 9,
		HiddenSections:   []string{"settings", "privacy", "benchmarks"},
		HiddenSettings:   []string{"enrolment", "org"},
		ReadOnlySections: []string{},
		ReadOnlySettings: []string{},
	}
	s := newRemoteTestServer(t, Options{Governance: func(context.Context) govern.Effective { return eff }})
	h := s.guardedHandler("127.0.0.1:8081")

	for _, path := range []string{
		"/api/enrolment/status",   // secSettings — the unhideable enrolment view
		"/api/privacy/scrub-test", // secPrivacy — the unhideable privacy page
		"/api/governance",         // how the SPA learns it is managed at all
		"/api/config",             // secSettings — the settings page's own read
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, loopbackRequest(http.MethodGet, path))
		if rec.Code == http.StatusNotFound {
			var refusal governanceRefusal
			if err := json.Unmarshal(rec.Body.Bytes(), &refusal); err == nil && refusal.Error == "governance_hidden" {
				t.Fatalf("%s was hidden by governance — a developer could not see what their organization configured", path)
			}
		}
	}
}

// TestEveryDashboardRouteHasASection is the coverage gate (adversarial
// review A9): every registered route carries an EXPLICIT section decision,
// so a new route cannot silently inherit membership in a hidden page — or
// silently leak one.
func TestEveryDashboardRouteHasASection(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	_, capMap, sections := s.registerRoutes(nil)
	if len(capMap) < 50 {
		t.Fatalf("suspiciously few routes registered (%d) — the registry may not be wired", len(capMap))
	}
	known := map[Section]bool{SectionNone: true}
	for _, sec := range AllSections {
		known[sec] = true
	}
	for pattern := range capMap {
		sec, ok := sections[pattern]
		if !ok {
			t.Errorf("route %q has no section entry — every reg() must name a Section (or SectionNone)", pattern)
			continue
		}
		if !known[sec] {
			t.Errorf("route %q names section %q, which is not in the closed vocabulary", pattern, sec)
		}
	}
	if len(sections) != len(capMap) {
		t.Errorf("section map has %d entries, capability map has %d — the two registries have diverged", len(sections), len(capMap))
	}
}

// TestDashboardSectionsMatchNodegovVocabulary pins the route-side section
// vocabulary to the POLICY-side one. Without it, an admin could publish a
// perfectly valid body naming a page whose routes no guard would ever match.
func TestDashboardSectionsMatchNodegovVocabulary(t *testing.T) {
	policy := map[string]bool{}
	for _, id := range nodegov.NavSectionIDs {
		policy[id] = true
	}
	dash := map[string]bool{}
	for _, sec := range AllSections {
		dash[string(sec)] = true
	}
	for id := range policy {
		if !dash[id] {
			t.Errorf("nodegov knows nav section %q but internal/intelligence/dashboard does not — a published directive for it would enforce nothing", id)
		}
	}
	for id := range dash {
		if !policy[id] {
			t.Errorf("dashboard knows section %q but nodegov does not — no org could ever address it", id)
		}
	}
}

// TestGovernanceEndpointDormantWithoutProvider pins the solo shape at the
// HTTP layer: the route exists, answers, and says nothing is governed.
func TestGovernanceEndpointDormantWithoutProvider(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	rec := httptest.NewRecorder()
	s.guardedHandler("127.0.0.1:8081").ServeHTTP(rec, loopbackRequest(http.MethodGet, "/api/governance"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/governance = %d, want 200", rec.Code)
	}
	var eff govern.Effective
	if err := json.Unmarshal(rec.Body.Bytes(), &eff); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if eff.Active {
		t.Fatalf("a node with no governance provider reported Active: %+v", eff)
	}
	if len(eff.HiddenSections)+len(eff.ReadOnlySections)+len(eff.HiddenSettings)+len(eff.ReadOnlySettings) != 0 {
		t.Fatalf("dormant posture carries directives: %+v", eff)
	}
	if eff.HiddenSections == nil {
		t.Fatal("hidden_sections is null rather than [] — the SPA would have to null-check every list")
	}
}
