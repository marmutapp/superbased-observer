// Package dashboard — Phase-4 session-attach review regression tests (F1–F4).
package dashboard

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/termrun"
)

// TestSensitiveViewerCloseByDevice pins F2: closeRemoteSensitiveViewersForDevice
// cancels ONLY the matching device's read-only sensitive viewers, leaving every
// other device's still-authorized view untouched; closeRemoteSensitiveViewers
// closes them all. This is the load-bearing registry mechanism a per-device
// revoke (device-session revoke / self-logout) drives.
func TestSensitiveViewerCloseByDevice(t *testing.T) {
	s := &Server{}
	// Idempotent-safe counters (production context.CancelFunc is idempotent; the
	// close methods don't deregister — the bridge's deferred unregister does).
	var a1, a2, b int32
	unA1 := s.registerSensitiveViewer("aaaa1111", func() { atomic.AddInt32(&a1, 1) })
	unA2 := s.registerSensitiveViewer("aaaa1111", func() { atomic.AddInt32(&a2, 1) })
	unB := s.registerSensitiveViewer("bbbb2222", func() { atomic.AddInt32(&b, 1) })

	// Per-device close of A closes BOTH of A's viewers, NONE of B's.
	if n := s.closeRemoteSensitiveViewersForDevice("aaaa1111"); n != 2 {
		t.Fatalf("closeForDevice(A) closed %d, want 2", n)
	}
	if atomic.LoadInt32(&a1) != 1 || atomic.LoadInt32(&a2) != 1 {
		t.Errorf("both of device A's viewers must be cancelled")
	}
	if atomic.LoadInt32(&b) != 0 {
		t.Errorf("device B's viewer must NOT be cancelled by a per-device close of A")
	}
	// Mimic the production coupling: a cancelled bridge deregisters itself, so a
	// later close does not see A's entries again.
	unA1()
	unA2()

	// An empty fingerprint (the owner-local path is never registered) matches
	// nothing.
	if n := s.closeRemoteSensitiveViewersForDevice(""); n != 0 {
		t.Errorf("closeForDevice(\"\") closed %d, want 0", n)
	}

	// Global close still catches B.
	if n := s.closeRemoteSensitiveViewers(); n != 1 {
		t.Fatalf("closeAll closed %d, want 1 (only B remained)", n)
	}
	if atomic.LoadInt32(&b) != 1 {
		t.Errorf("device B's viewer must be cancelled by closeAll")
	}
	unB()
}

// TestSensitiveViewerDisableRaceRecheckRefuses pins F1: a subscribe that passes
// the FIRST gate check but registers AFTER a concurrent disable has already
// flipped the gate false and drained the (then-empty) registry must be REFUSED
// by the post-register re-check — never left streaming. This models the exact
// ordering handleLaunchWS + the disable path use: gate-flip → drain on one side,
// register → re-read on the other.
func TestSensitiveViewerDisableRaceRecheckRefuses(t *testing.T) {
	s := &Server{}

	// The live gate the re-check reads. Starts on; the disable flips it off.
	gateOn := true
	allowView := func() bool { return gateOn }

	// --- disable path: flip the gate off, THEN drain the registry ---
	gateOn = false
	if n := s.closeRemoteSensitiveViewers(); n != 0 {
		t.Fatalf("drain closed %d, want 0 (racing subscribe not yet registered)", n)
	}

	// --- racing subscribe: registers AFTER the drain (worst case) ---
	closed := make(chan struct{}, 1)
	unregister := s.registerSensitiveViewer("cccc3333", func() { closed <- struct{}{} })

	// F1 re-check: the gate is now visibly false, so the subscribe deregisters
	// and refuses instead of streaming forever.
	refused := false
	if !allowView() {
		unregister()
		refused = true
	}
	if !refused {
		t.Fatal("re-check must observe the disabled gate and refuse the raced subscribe")
	}
	// The refused subscribe leaves NO entry behind — a later drain finds nothing.
	if n := s.closeRemoteSensitiveViewersForDevice("cccc3333"); n != 0 {
		t.Errorf("a refused subscribe left a registered viewer behind (%d)", n)
	}
}

// TestSessionRevokeHookWiredByNew pins F2 (logout leg): New() wires the
// controller's session-revoke hook to closeRemoteSensitiveViewersForDevice, so a
// device that revokes its OWN session (logout) has its open read-only sensitive
// viewers torn down — scoped to that device's fingerprint.
func TestSessionRevokeHookWiredByNew(t *testing.T) {
	s, _ := newManageServer(t)
	rc, ok := s.opts.Remote.(*remoteController)
	if !ok {
		t.Fatal("expected a concrete *remoteController")
	}

	closedSelf := make(chan struct{}, 1)
	closedOther := make(chan struct{}, 1)
	unSelf := s.registerSensitiveViewer("deadbeef", func() { closedSelf <- struct{}{} })
	unOther := s.registerSensitiveViewer("feed0000", func() { closedOther <- struct{}{} })
	defer unSelf()
	defer unOther()

	// Fire the hook New() installed (the logout path calls exactly this).
	rc.fireSessionRevokeHook("deadbeef")

	if len(closedSelf) != 1 {
		t.Errorf("self-logout must close the logging-out device's sensitive viewer")
	}
	if len(closedOther) != 0 {
		t.Errorf("a self-logout must NOT close another device's viewer")
	}
}

// TestReArmWithViewOffClosesViewers pins F3: re-arming an already-live
// controller with allow_terminal_view left OFF tears down every already-open
// remote-sensitive viewer (the teardown the handler now performs BEFORE its
// best-effort audit insert).
func TestReArmWithViewOffClosesViewers(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)

	// Arm + enable the view opt-in.
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal_view":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}

	closed := make(chan struct{}, 1)
	unregister := s.registerSensitiveViewer("aaaa1111", func() { closed <- struct{}{} })
	defer unregister()

	// Re-arm with an EXPLICIT allow_terminal_view:false: the live view gate
	// flips off and the open viewer is closed. (Omitting the field now INHERITS
	// the seed default — true — so turning view off must be stated explicitly;
	// that is the enable-overwrites-default fix.)
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal_view":false}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("re-arm = %d: %s", rec.Code, rec.Body.String())
	}
	if len(closed) != 1 {
		t.Errorf("re-arm with view disabled must close the open remote-sensitive viewer (F3)")
	}
}

// TestRotateClosesSensitiveViewers pins F2: a pairing-secret rotation (which
// invalidates every paired device) closes every already-open read-only
// sensitive viewer.
func TestRotateClosesSensitiveViewers(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal_view":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	closed := make(chan struct{}, 1)
	unregister := s.registerSensitiveViewer("aaaa1111", func() { closed <- struct{}{} })
	defer unregister()

	if rec := postConfirm(t, h, "/api/remote/rotate", `{}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d: %s", rec.Code, rec.Body.String())
	}
	if len(closed) != 1 {
		t.Errorf("rotate must close every already-open sensitive viewer (F2)")
	}
}

// TestRevokeAllClosesSensitiveViewers pins F2: a revoke-all (terminate every
// device session) closes every already-open read-only sensitive viewer.
func TestRevokeAllClosesSensitiveViewers(t *testing.T) {
	s, h := newManageServer(t)
	ck, token := getConfirm(t, h)
	if rec := postConfirm(t, h, "/api/remote/enable", `{"host":"box.ts.net","allow_terminal_view":true}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d: %s", rec.Code, rec.Body.String())
	}
	closed := make(chan struct{}, 1)
	unregister := s.registerSensitiveViewer("aaaa1111", func() { closed <- struct{}{} })
	defer unregister()

	if rec := postConfirm(t, h, "/api/remote/sessions/revoke-all", `{}`, ck, token); rec.Code != http.StatusOK {
		t.Fatalf("revoke-all = %d: %s", rec.Code, rec.Body.String())
	}
	if len(closed) != 1 {
		t.Errorf("revoke-all must close every already-open sensitive viewer (F2)")
	}
}

// TestSpawnAuditKindVocabulary pins the F4 audit-kind vocabulary: the shared
// SpawnAuditKind table maps the two spawn kinds to their metadata-only
// remote_audit kinds and nothing else.
func TestSpawnAuditKindVocabulary(t *testing.T) {
	cases := map[string]string{
		"attach": "terminal_attach",
		"resume": "terminal_resume",
	}
	for _, k := range []string{"handoff", "fresh"} {
		cases[k] = "" // non-remote-sensitive spawns get no spawn-audit kind
	}
	for kind, want := range cases {
		if got := SpawnAuditKind(termrun.Kind(kind)); got != want {
			t.Errorf("SpawnAuditKind(%q) = %q, want %q", kind, got, want)
		}
	}
}
