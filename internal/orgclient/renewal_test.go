package orgclient

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestClassifyRenewalTable is one row per status class (§4.2). The two rows
// a naive "2xx only" rule would get wrong — 304 and 404 — are called out by
// name so a future simplification fails loudly rather than silently expiring
// every converged node.
func TestClassifyRenewalTable(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		err        error
		wantSignal bool
		wantAuth   bool
		wantDenied bool
	}{
		{name: "200 push accepted", status: 200, wantSignal: true, wantAuth: true},
		{name: "204", status: 204, wantSignal: true, wantAuth: true},
		{name: "304 on the policy poll (the steady state)", status: http.StatusNotModified, wantSignal: true, wantAuth: true},
		{name: "404 no policy published", status: http.StatusNotFound, wantSignal: true, wantAuth: true},
		{name: "401", status: http.StatusUnauthorized, wantSignal: true, wantDenied: true},
		{name: "403", status: http.StatusForbidden, wantSignal: true, wantDenied: true},
		{name: "429 is not a verdict on the credential", status: http.StatusTooManyRequests},
		{name: "400 is not a verdict on the credential", status: http.StatusBadRequest},
		{name: "500 does not prove the server accepted the bearer", status: http.StatusInternalServerError},
		{name: "503", status: http.StatusServiceUnavailable},
		{name: "transport error", status: 200, err: errors.New("dial tcp: refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := classifyRenewal(tc.status, tc.err)
			if ok != tc.wantSignal {
				t.Fatalf("signal = %v, want %v", ok, tc.wantSignal)
			}
			if out.Authorized != tc.wantAuth || out.Denied != tc.wantDenied {
				t.Fatalf("outcome = %+v, want authorized=%v denied=%v", out, tc.wantAuth, tc.wantDenied)
			}
		})
	}
}

// TestRenewalStopsOn401 / …On403: a denial on ANY authenticated path stops
// renewal, no matter how many 2xx or 304 responses arrive elsewhere. This is
// parent §11.3's exact case: push revoked, poll still answering.
func TestRenewalStopsOn401(t *testing.T) { assertDenialLatches(t, http.StatusUnauthorized) }
func TestRenewalStopsOn403(t *testing.T) { assertDenialLatches(t, http.StatusForbidden) }

func assertDenialLatches(t *testing.T, status int) {
	t.Helper()
	var tr RenewalTracker
	deny, _ := classifyRenewal(status, nil)
	deny.Path = RenewalPathPush
	tr.Observe(deny)
	if !tr.Latched() {
		t.Fatalf("a %d did not latch renewal off", status)
	}
	// The poll keeps answering happily. It must change nothing.
	for _, s := range []int{200, http.StatusNotModified, http.StatusNotFound} {
		o, _ := classifyRenewal(s, nil)
		o.Path = RenewalPathOther
		tr.Observe(o)
	}
	if !tr.Latched() {
		t.Fatal("a still-working policy poll cleared the latch — an org that revokes push but leaves the poll open would keep governing the machine")
	}
}

// TestRenewalLatchClearsOnlyOnPush states R5's rule positively.
func TestRenewalLatchClearsOnlyOnPush(t *testing.T) {
	var tr RenewalTracker
	tr.Observe(RenewalOutcome{Denied: true, Path: RenewalPathOther})
	tr.Observe(RenewalOutcome{Authorized: true, Path: RenewalPathOther})
	if !tr.Latched() {
		t.Fatal("an authorized non-push response cleared the latch")
	}
	tr.Observe(RenewalOutcome{Authorized: true, Path: RenewalPathPush})
	if tr.Latched() {
		t.Fatal("an authorized push did not clear the latch")
	}
}

// TestRenewalOn304PolicyPoll / TestRenewalOn404NoPolicy are the two cases
// operator ruling R4 called out explicitly, each as its own named test.
func TestRenewalOn304PolicyPoll(t *testing.T) {
	out, ok := classifyRenewal(http.StatusNotModified, nil)
	if !ok || !out.Authorized {
		t.Fatal("a 304 on the policy poll is not a renewal signal — every converged node would silently expire")
	}
}

func TestRenewalOn404NoPolicy(t *testing.T) {
	out, ok := classifyRenewal(http.StatusNotFound, nil)
	if !ok || !out.Authorized {
		t.Fatal("a 404 (no policy published) is not a renewal signal — an org that governs nobody would expire every grant")
	}
}

// TestProbeOnlyFiresWhileLatched: a healthy idle node stays completely
// silent, so an idle fleet does not become chatty.
func TestProbeOnlyFiresWhileLatched(t *testing.T) {
	now := time.Now()
	var tr RenewalTracker
	if tr.ShouldProbe(now, time.Minute) {
		t.Fatal("an unlatched tracker asked to probe")
	}
	tr.Observe(RenewalOutcome{Denied: true, Path: RenewalPathOther})
	if !tr.ShouldProbe(now, time.Minute) {
		t.Fatal("a latched tracker did not ask to probe")
	}
	if tr.ShouldProbe(now.Add(30*time.Second), time.Minute) {
		t.Fatal("the probe fired twice inside its own interval")
	}
	if !tr.ShouldProbe(now.Add(2*time.Minute), time.Minute) {
		t.Fatal("the probe never fired again after its interval")
	}
	tr.Observe(RenewalOutcome{Authorized: true, Path: RenewalPathPush})
	if tr.ShouldProbe(now.Add(10*time.Minute), time.Minute) {
		t.Fatal("a cleared tracker kept probing")
	}
}

// TestLatchClearsOnIdleNode is review M2's case: the node has nothing to
// push, so PushOnce short-circuits before any request is sent. Without the
// explicit probe, one transient 401 would latch renewal off for the daemon's
// lifetime and an availability blip would silently offboard a compliant node
// up to 30 days later.
func TestLatchClearsOnIdleNode(t *testing.T) {
	now := time.Now()
	var tr RenewalTracker
	tr.Observe(RenewalOutcome{Denied: true, Path: RenewalPathOther}) // the blip

	// Only empty pushes (no round trip at all) and healthy polls follow.
	for i := 0; i < 5; i++ {
		tr.Observe(RenewalOutcome{Authorized: true, Path: RenewalPathOther})
	}
	if !tr.Latched() {
		t.Fatal("polls alone cleared the latch")
	}
	if !tr.ShouldProbe(now, time.Minute) {
		t.Fatal("an idle latched node never probes — renewal would be off forever")
	}
	// The probe's own response is classified exactly like any other push.
	out, ok := classifyRenewal(http.StatusOK, nil)
	if !ok {
		t.Fatal("the probe response produced no signal")
	}
	out.Path = RenewalPathPush
	tr.Observe(out)
	if tr.Latched() {
		t.Fatal("an authorized probe on the push path did not resume renewal")
	}
}

// TestNilTrackerAndSinkAreInert: a build with no governance wiring behaves
// exactly as Phase 1a did.
func TestNilTrackerAndSinkAreInert(t *testing.T) {
	var tr *RenewalTracker
	tr.Observe(RenewalOutcome{Denied: true})
	if tr.Latched() || tr.ShouldProbe(time.Now(), time.Minute) {
		t.Fatal("a nil tracker is not inert")
	}
	var c *Client
	c.noteRenewal(RenewalPathPush, 401, nil) // must not panic
}

// TestShareProviderDefaultParity: with no provider installed, the push
// share posture is EXACTLY the cfg-derived value Phase 1a built inline.
func TestShareProviderDefaultParity(t *testing.T) {
	cfg := config.OrgClientConfig{}
	cfg.Share.FullContent = true
	cfg.Share.RoutingSummary = true
	cfg.Share.TargetActionAllowlist = []string{"read_file"}
	cfg.Share.Obs.Traces = true

	c := &Client{cfg: cfg}
	if !reflect.DeepEqual(c.shareOptions(), ShareOptionsFromConfig(cfg)) {
		t.Fatal("the default share provider is not the cfg-derived value")
	}

	// And an installed provider wins — the hot-lowering seam (§2.4).
	lowered := ShareOptionsFromConfig(cfg)
	lowered.FullContent = false
	c.SetShareProvider(func() store.ShareOptions { return lowered })
	if c.shareOptions().FullContent {
		t.Fatal("the installed provider did not win — a lowering directive would take hours to apply")
	}
	c.SetShareProvider(nil)
	if !c.shareOptions().FullContent {
		t.Fatal("clearing the provider did not restore the cfg-derived default")
	}
}
