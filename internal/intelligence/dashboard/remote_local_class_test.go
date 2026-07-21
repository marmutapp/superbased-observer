package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
)

// capLetter maps the compact test letters to Capability values.
func capLetter(t *testing.T, letter string) Capability {
	t.Helper()
	switch letter {
	case "P":
		return CapabilityPublic
	case "V":
		return CapabilityView
	case "L":
		return CapabilityLocal
	case "X":
		return CapabilityExecute
	}
	t.Fatalf("unknown capability letter %q", letter)
	return CapabilityUnclassified
}

// expectedClassification is the AUTHORITATIVE route→capability table
// (dashboard-management-surface plan §9.3, realised as whole-route classes — see
// the mechanism note in registerRoutes). It is the source of truth
// TestFullRegistryClassification asserts capMap against exactly: a new route
// added without a classification decision fails the build, and any drift is
// loud. P=public, V=view, L=owner-local-only, X=execute.
var expectedClassification = map[string]string{
	"/":                                       "P",
	"/api/action/":                            "V",
	"/api/actions":                            "V",
	"/api/actions/day-counts":                 "V",
	"/api/admin/antigravity-bridge.exe":       "V",
	"/api/admin/restart":                      "L",
	"/api/analysis/cache-savings-trend":       "V",
	"/api/analysis/cost-by-dow-hour":          "V",
	"/api/analysis/cost-by-hour":              "V",
	"/api/analysis/headline":                  "V",
	"/api/analysis/movers":                    "V",
	"/api/analysis/routing-suggestions":       "V",
	"/api/analysis/top-sessions":              "V",
	"/api/analysis/trend":                     "V",
	"/api/attach/sessions":                    "V",
	"/api/backfill/jobs":                      "V",
	"/api/backfill/jobs/":                     "V",
	"/api/backfill/run":                       "L",
	"/api/backfill/status":                    "V",
	"/api/benchmarks":                         "V",
	"/api/benchmarks/":                        "V",
	"/api/budget":                             "V",
	"/api/cache/entry-states":                 "V",
	"/api/cache/events":                       "V",
	"/api/cache/health":                       "V",
	"/api/cache/overview":                     "V",
	"/api/cache/status":                       "V",
	"/api/cache/timeseries":                   "V",
	"/api/codex/support":                      "V",
	"/api/compaction/events":                  "V",
	"/api/compression/by-model":               "V",
	"/api/compression/events":                 "V",
	"/api/compression/retrieval":              "V",
	"/api/compression/rolling-cost":           "V",
	"/api/compression/timeseries":             "V",
	"/api/config":                             "V",
	"/api/config/backup":                      "L",
	"/api/config/pricing":                     "L",
	"/api/config/pricing/defaults":            "V",
	"/api/config/profiles":                    "L",
	"/api/config/profiles/":                   "L",
	"/api/config/reload":                      "L",
	"/api/config/section/":                    "L",
	"/api/cost":                               "V",
	"/api/cowork/reconcile":                   "V",
	"/api/demo":                               "V",
	"/api/demo/start":                         "L",
	"/api/demo/stop":                          "L",
	"/api/discover":                           "V",
	"/api/enrolment/last-payload":             "V",
	"/api/enrolment/status":                   "V",
	"/api/enrolment/unenroll":                 "L",
	"/api/experiments":                        "L",
	"/api/experiments/report":                 "V",
	"/api/experiments/stop":                   "L",
	"/api/export.xlsx":                        "V",
	"/api/file/state":                         "V",
	"/api/guard/approvals":                    "L",
	"/api/guard/approvals/":                   "L",
	"/api/guard/budget":                       "V",
	"/api/guard/conformance":                  "V",
	"/api/guard/events":                       "V",
	"/api/guard/evidence":                     "L",
	"/api/guard/evidence/download":            "V",
	"/api/guard/mcp":                          "V",
	"/api/guard/mcp/approve":                  "L",
	"/api/guard/policy":                       "L",
	"/api/guard/policy/backup":                "L",
	"/api/guard/policy/lint":                  "V",
	"/api/guard/rules":                        "V",
	"/api/guard/simulate":                     "V",
	"/api/guard/summary":                      "V",
	"/api/health/doctor":                      "V",
	"/api/health/failures":                    "V",
	"/api/health/watcher":                     "V",
	"/api/launch/":                            "V",
	"/api/live":                               "V",
	"/api/mcp/value":                          "V",
	"/api/models":                             "V",
	"/api/patterns":                           "V",
	"/api/patterns/timeseries":                "V",
	"/api/privacy/scrub-test":                 "V",
	"/api/process/findings":                   "V",
	"/api/process/network/":                   "V",
	"/api/projects":                           "V",
	"/api/prune/run":                          "L",
	"/api/remote/add-device":                  "L",
	"/api/remote/allow-terminal":              "L",
	"/api/remote/allow-terminal-view":         "L",
	"/api/remote/standing-revoke-on-takeover": "L",
	"/api/remote/approve-execute":             "L",
	"/api/remote/audit":                       "V",
	"/api/remote/config":                      "V",
	"/api/remote/disable":                     "L",
	"/api/remote/enable":                      "L",
	"/api/remote/rotate":                      "L",
	"/api/remote/selfcheck":                   "V",
	"/api/remote/sessions":                    "V",
	"/api/remote/sessions/":                   "L",
	"/api/remote/sessions/revoke-all":         "L",
	"/api/remote/standing-terminal":           "V", // status read (never the secret)
	"/api/remote/standing-terminal/mint":      "L", // mint/rotate the standing secret — owner-local only
	"/api/remote/standing-terminal/revoke":    "L", // revoke standing access — owner-local only
	"/api/remote/tailscale/status":            "V",
	"/api/remote/tailscale/serve":             "L", // runs `tailscale serve` (machine mutation) — owner-local only
	"/api/remote/tailscale/operator-grant":    "L", // spawns `sudo tailscale set --operator` in a local-only PTY — owner-local only
	"/api/remote/tailscale/login":             "L", // spawns `tailscale up` in a local-only PTY — owner-local only
	"/api/remote/tailscale/install":           "L", // spawns the Linux install.sh in a local-only PTY — owner-local only
	"/api/report/monthly":                     "V",
	"/api/routing/apply":                      "L",
	"/api/routing/apply/ledger":               "V",
	"/api/routing/apply/revert":               "L",
	"/api/routing/decisions":                  "V",
	"/api/routing/health":                     "V",
	"/api/routing/policy":                     "V",
	"/api/routing/policy/lint":                "V",
	"/api/routing/savings":                    "V",
	"/api/routing/shadow":                     "V",
	"/api/routing/simulate":                   "V",
	"/api/routing/status":                     "V",
	"/api/routing/tiers":                      "V",
	"/api/scan/run":                           "L",
	"/api/search":                             "V",
	"/api/session/":                           "V",
	"/api/sessions":                           "V",
	"/api/sessions/calendar":                  "V",
	"/api/setup/claude":                       "L",
	"/api/setup/codex":                        "L",
	"/api/setup/codex-hooks":                  "V",
	"/api/setup/hooks":                        "L",
	"/api/setup/mcp":                          "L",
	"/api/status":                             "V",
	"/api/status/scoped":                      "V",
	"/api/storage":                            "V",
	"/api/storage/backup":                     "L",
	"/api/storage/vacuum":                     "L",
	"/api/suggest":                            "V",
	"/api/suggest/write":                      "L",
	"/api/suggestions":                        "V",
	"/api/suggestions/state":                  "L",
	"/api/terminal/":                          "V",
	"/api/terminal/launch":                    "X",
	"/api/terminal/limits":                    "L",
	"/api/terminal/policy":                    "L",
	"/api/terminal/runs":                      "V",
	"/api/terminal/sessions":                  "V",
	"/api/terminal/workspace-layout":          "V",
	"/api/terminal/workspace-layout/save":     "L",
	"/api/timeseries/actions":                 "V",
	"/api/timeseries/cost":                    "V",
	"/api/timeseries/tokens-by-model":         "V",
	"/api/tools":                              "V",
	"/api/tools/breakdown":                    "V",
	"/api/tools/launch":                       "L",
	"/api/tools/status":                       "V",
	"/api/verbosity/aggregate":                "V",
	"/ws/launch/":                             "V",
	"/ws/terminal/status":                     "V",
}

// TestFullRegistryClassification is the codex-finding-2 backstop (plan §9): the
// WHOLE built-in capMap must match the authoritative table exactly. A new
// mutation route that lands as plain View — or any classification drift — fails
// the build.
func TestFullRegistryClassification(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	_, capMap := s.registerRoutes(nil)

	for pattern, cap := range capMap {
		want, ok := expectedClassification[pattern]
		if !ok {
			t.Errorf("route %q is registered but NOT in the §9.3 classification table — classify it (public/view/local/execute) and add it to expectedClassification", pattern)
			continue
		}
		if cap != capLetter(t, want) {
			t.Errorf("route %q classified %s, want %s (§9.3)", pattern, cap, capLetter(t, want))
		}
	}
	for pattern := range expectedClassification {
		if _, ok := capMap[pattern]; !ok {
			t.Errorf("expected route %q from the §9.3 table is not registered", pattern)
		}
	}
}

// mutationRoutes is the set of built-in routes whose handler writes config,
// writes an AI-client file, or drives daemon lifecycle. Every one MUST be Local
// or Execute — never plain View (which would auto-escalate an unsafe method to
// Execute and become reachable by a remote execute principal). This is the §2A
// invariant. (Routes whose unsafe verb is DELIBERATELY Execute via bare-View
// auto-escalation — /api/launch/ DELETE, /api/session/ sub-routes — are covered
// by TestSessionSubRouteCapabilities + the remote authz matrix, not here.)
var mutationRoutes = []string{
	"/api/admin/restart",
	"/api/backfill/run",
	"/api/config/backup",
	"/api/config/pricing",
	"/api/config/profiles",
	"/api/config/profiles/",
	"/api/config/reload",
	"/api/config/section/",
	"/api/demo/start",
	"/api/demo/stop",
	"/api/enrolment/unenroll",
	"/api/experiments",
	"/api/experiments/stop",
	"/api/guard/approvals",
	"/api/guard/approvals/",
	"/api/guard/evidence",
	"/api/guard/mcp/approve",
	"/api/guard/policy",
	"/api/guard/policy/backup",
	"/api/prune/run",
	"/api/remote/allow-terminal",
	"/api/remote/allow-terminal-view",
	"/api/remote/approve-execute",
	"/api/remote/standing-revoke-on-takeover",
	"/api/remote/standing-terminal/mint",
	"/api/remote/standing-terminal/revoke",
	// The tailscale guided-setup POSTs all reach the machine (run `tailscale
	// serve`, or spawn a privileged sudo PTY), so every one MUST be Local — not
	// merely present in the editable classification table (the review flagged
	// the two-source gap). serve + operator-grant were already Local; login +
	// install are the new arrivals.
	"/api/remote/tailscale/serve",
	"/api/remote/tailscale/operator-grant",
	"/api/remote/tailscale/login",
	"/api/remote/tailscale/install",
	"/api/routing/apply",
	"/api/routing/apply/revert",
	"/api/scan/run",
	"/api/setup/claude",
	"/api/setup/codex",
	"/api/setup/hooks",
	"/api/setup/mcp",
	"/api/storage/backup",
	"/api/storage/vacuum",
	"/api/suggest/write",
	"/api/suggestions/state",
	"/api/tools/launch",
	"/api/terminal/launch",
	"/api/terminal/limits",
	"/api/terminal/policy",
}

// TestNoConfigMutationRouteIsPlainView pins the §2A / §9.2 invariant: no route
// in the declared mutation set is left as plain View. Build-breaking and
// fail-closed.
func TestNoConfigMutationRouteIsPlainView(t *testing.T) {
	s := newRemoteTestServer(t, Options{})
	_, capMap := s.registerRoutes(nil)
	for _, pattern := range mutationRoutes {
		cap, ok := capMap[pattern]
		if !ok {
			t.Errorf("mutation route %q not registered", pattern)
			continue
		}
		if cap != CapabilityLocal && cap != CapabilityExecute {
			t.Errorf("mutation route %q is %s — must be Local or Execute, never plain View (auto-escalation would expose it to a remote execute principal)", pattern, cap)
		}
	}
}

// TestLocalRoutesRefusedOnRemoteListener is the core §A safety property: a View
// AND an Execute principal both get 403 on every Local route on the
// remotely-exposed listener, while a Local route stays reachable on the direct
// loopback listener (which never runs remoteAuthz).
func TestLocalRoutesRefusedOnRemoteListener(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	remoteH := s.remoteGuardedHandler(rc)
	loopH := s.Handler() // direct loopback path — no remoteAuthz

	cookie, csrf := pairSession(t, remoteH, enc)
	mc := rc.(*remoteController)

	_, capMap := s.registerRoutes(nil)
	type probe struct{ method, path string }
	var locals []probe
	for pattern, cap := range capMap {
		if cap != CapabilityLocal {
			continue
		}
		method := http.MethodPost
		path := pattern
		if m, p, found := strings.Cut(pattern, " "); found {
			method, path = m, p
		}
		if strings.HasSuffix(path, "/") {
			path += "probe"
		}
		locals = append(locals, probe{method, path})
	}
	if len(locals) < 15 {
		t.Fatalf("suspiciously few Local routes discovered (%d)", len(locals))
	}

	call := func(h http.Handler, method, path string, withCap bool) int {
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Host = testRemoteHost
		req.Header.Set("Origin", "https://"+testRemoteHost)
		req.AddCookie(cookie)
		req.Header.Set(remoteCSRFHeader, csrf)
		if withCap {
			tok, err := mc.MintExecute(cookie.Value, method+" "+path)
			if err != nil {
				t.Fatalf("MintExecute: %v", err)
			}
			req.Header.Set(remoteExecuteHeader, tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Every Local route is refused on the remote listener for BOTH a view and an
	// execute principal. (We only probe authz — the handlers never execute, so
	// no real config-write/restart side effect fires.)
	for _, p := range locals {
		if code := call(remoteH, p.method, p.path, false); code != http.StatusForbidden {
			t.Errorf("view principal on Local route %s %s = %d, want 403", p.method, p.path, code)
		}
		if code := call(remoteH, p.method, p.path, true); code != http.StatusForbidden {
			t.Errorf("execute principal on Local route %s %s = %d, want 403 (Local is orthogonal to the ladder)", p.method, p.path, code)
		}
	}

	// Direct loopback listener: a Local route is NOT gated by capability there
	// (browserGuard only — Local is metadata). Probe ONE benign Local write
	// (/api/suggestions/state, writes only node-local advisor state to the test
	// DB) and assert it is not refused by an authz 401/403: it reaches its
	// handler. Executing restart/vacuum/etc. on loopback would fire real side
	// effects, so we deliberately probe only this safe route.
	{
		body := `{"dedup_key":"local-class-test","status":"dismissed"}`
		req := httptest.NewRequest(http.MethodPost, "/api/suggestions/state", strings.NewReader(body))
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		loopH.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Errorf("loopback listener refused a Local route (%d) — Local routes must stay open on the owner-trusted listener", rec.Code)
		}
	}
}

// TestLocalRouteNeverConsumesExecuteCapability pins the funnel-parity guarantee
// (plan §A): a Local-route refusal must NOT consume a single-use execute
// capability (the refusal happens before the principal is resolved).
func TestLocalRouteNeverConsumesExecuteCapability(t *testing.T) {
	rc, enc := newReadyRemoteController(t)
	s := newRemoteTestServer(t, Options{Remote: rc})
	h := s.remoteGuardedHandler(rc)
	cookie, csrf := pairSession(t, h, enc)
	mc := rc.(*remoteController)

	action := http.MethodPost + " /api/scan/run"
	tok, err := mc.MintExecute(cookie.Value, action)
	if err != nil {
		t.Fatalf("MintExecute: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/scan/run", strings.NewReader("{}"))
	req.Host = testRemoteHost
	req.Header.Set("Origin", "https://"+testRemoteHost)
	req.AddCookie(cookie)
	req.Header.Set(remoteCSRFHeader, csrf)
	req.Header.Set(remoteExecuteHeader, tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Local route with an execute capability = %d, want 403", rec.Code)
	}
	// Capabilities are keyed by the session HASH (matching Principal's Consume),
	// so verify survival with the same currency MintExecute used.
	if !mc.caps.Consume(tok, remoteauth.HashSessionID(cookie.Value), action) {
		t.Error("execute capability was consumed by a refused Local-route request — it must survive (funnel-parity)")
	}
}

// TestSessionSubRouteCapabilities enumerates the /api/session/ suffix table
// (plan §9.1): every mutating sub-route resolves to an explicit Execute tier,
// and none is Local (a Local sub-route would need its own pattern).
func TestSessionSubRouteCapabilities(t *testing.T) {
	if len(sessionSubRouteCapabilities) == 0 {
		t.Fatal("sessionSubRouteCapabilities is empty — the /api/session/ mutating sub-routes must be enumerated")
	}
	for suffix, cap := range sessionSubRouteCapabilities {
		if !strings.HasPrefix(suffix, "/") {
			t.Errorf("sub-route suffix %q must start with '/'", suffix)
		}
		if cap != CapabilityExecute {
			t.Errorf("/api/session/…%s is %s — mutating session sub-routes are Execute (a Local sub-route needs a dedicated pattern)", suffix, cap)
		}
	}
}
