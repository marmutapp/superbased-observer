package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// withTerminalPingBudget lowers the consecutive-failure budget for a test and
// restores the production default on cleanup (companion to withFastTerminalPing).
func withTerminalPingBudget(t *testing.T, budget int64) {
	t.Helper()
	old := terminalPingFailureBudget.Load()
	terminalPingFailureBudget.Store(budget)
	t.Cleanup(func() { terminalPingFailureBudget.Store(old) })
}

// TestTerminalBridgeToleratesTemporarilyFrozenPeer is the mobile-continuity half
// of the liveness contract. A backgrounded mobile tab is SUSPENDED by the OS: it
// stops reading, so coder/websocket stops auto-replying pongs, and the previous
// one-strike rule tore the bridge down inside ~40 seconds — which is what the
// user saw as "reconnect to the terminal" after stepping out to their mail app.
//
// Here the client stops reading for several ping intervals (the freeze) and then
// resumes (the return). With a failure budget > the missed pings, the bridge
// must still be up. The companion negative below proves a peer that NEVER comes
// back is still reaped, so this is a change in latency, not the removal of a
// check.
func TestTerminalBridgeToleratesTemporarilyFrozenPeer(t *testing.T) {
	withFastTerminalPing(t, 40*time.Millisecond, 50*time.Millisecond)
	withTerminalPingBudget(t, 8)

	lm := &fakeLaunchManager{sub: newFakeSubscription()}
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := dialLaunchWS(t, ctx, ts.URL)
	defer c.CloseNow()

	// FREEZE: do not read at all for ~5 ping intervals. Every ping in this
	// window times out waiting for a pong (budget 8 > 5, so no teardown).
	time.Sleep(250 * time.Millisecond)

	// RETURN: start reading again. The pongs flow, the failure streak resets,
	// and the connection must stay up across many more intervals.
	readErr := make(chan error, 1)
	go func() {
		for {
			if _, _, rerr := c.Read(ctx); rerr != nil {
				readErr <- rerr
				return
			}
		}
	}()
	select {
	case rerr := <-readErr:
		t.Fatalf("a briefly-frozen (but returning) peer was evicted: %v", rerr)
	case <-time.After(500 * time.Millisecond): // ~12 further ping intervals
	}
}

// TestTerminalBridgeStillReapsPermanentlyDeadPeer is the companion negative: the
// grace window must not become "never reap". A peer that never reads again
// exhausts the budget and the bridge is torn down.
func TestTerminalBridgeStillReapsPermanentlyDeadPeer(t *testing.T) {
	withFastTerminalPing(t, 30*time.Millisecond, 40*time.Millisecond)
	withTerminalPingBudget(t, 3)

	lm := &fakeLaunchManager{sub: newFakeSubscription()}
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := dialLaunchWS(t, ctx, ts.URL)
	defer c.CloseNow()

	// Never respond: 3 consecutive missed pongs (~210ms) exhausts the budget.
	time.Sleep(500 * time.Millisecond)

	// The assertion must be that the SERVER tore the connection down — not
	// merely that this goroutine stopped waiting. rctx is a LOCAL deadline, so
	// accepting any Read error made the control vacuous: with liveness disabled
	// entirely, Read simply blocks until rctx fires and returns
	// context.DeadlineExceeded, which the old form scored as a successful reap.
	// (Proven by mutation on 2026-07-25: neutering the reap left the old test
	// GREEN and this one RED.) So a deadline-exceeded error whose cause is our
	// own expired context is an explicit FAILURE here; only a server-originated
	// close/EOF/reset counts.
	rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
	defer rcancel()
	for {
		_, _, rerr := c.Read(rctx)
		if rerr == nil {
			continue // a frame from a live server says nothing either way
		}
		if errors.Is(rerr, context.DeadlineExceeded) && rctx.Err() != nil {
			t.Fatalf("a permanently dead peer was NOT torn down within the read window — "+
				"the grace budget must not disable the check (read err: %v)", rerr)
		}
		return // server-originated failure: the dead peer was reaped
	}
}

// expiringWriter is a recordingWriter that additionally reports RevokeIsExpiry,
// the additive optional interface the remote bridge consults.
type expiringWriter struct {
	*recordingWriter
	expiry bool
}

func (w *expiringWriter) RevokeIsExpiry() bool { return w.expiry }

// expiryLaunchManager reuses the recording manager for every seam except the
// remote writer acquire, which hands back the expiry-reporting writer above.
type expiryLaunchManager struct {
	*recordingLaunchManager
	writer LaunchWriter
}

func (m *expiryLaunchManager) AcquireWriterRemote(RemoteWriterRequest) (LaunchWriter, error) {
	return m.writer, nil
}

// dialLaunchWS opens a same-origin /ws/launch bridge against a test server.
func dialLaunchWS(t *testing.T, ctx context.Context, base string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws/launch/HANDLE-abc",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {base}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c
}

// TestLeaseExpiryDemotesRemoteBridgeWhileRevokeCloses is the socket-teardown
// half of the arc. closeOnHardRevoke is TRUE on the remote bridge, so any
// non-takeover revocation used to close the websocket — including a writer lease
// that had merely aged out. Closing the socket for an expiry is what forced a
// remote user to re-establish the terminal (and, with it, re-issue credentials)
// every 30 minutes even though the device was never in doubt.
//
// An EXPIRY must demote to a read-only viewer with the socket intact (the client
// can then silently re-acquire — instantly with a standing secret). A revoke —
// admin disable, rotate, device revoke, allow_terminal→false — must still close.
func TestLeaseExpiryDemotesRemoteBridgeWhileRevokeCloses(t *testing.T) {
	tests := []struct {
		name          string
		expiry        bool
		wantStillOpen bool
	}{
		{name: "aged-out lease demotes and keeps the socket", expiry: true, wantStillOpen: true},
		{name: "trust-withdrawing revoke still closes the socket", expiry: false, wantStillOpen: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rw := newRecordingWriter()
			w := &expiringWriter{recordingWriter: rw, expiry: tc.expiry}
			base := newRecordingLaunchManager(nil)
			lm := &expiryLaunchManager{recordingLaunchManager: base, writer: w}
			t.Cleanup(func() { close(base.sub.release) })
			s := newLaunchTestServer(t, lm)
			ts := remoteExposedWSServer(t, s)
			t.Cleanup(ts.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/launch/HANDLE-abc",
				&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {ts.URL}}})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer c.CloseNow()

			if werr := c.Write(ctx, websocket.MessageText,
				[]byte(`{"t":"acquire-writer","cap":"x","confirm":"y"}`)); werr != nil {
				t.Fatalf("write acquire: %v", werr)
			}
			if !waitForControl(t, ctx, c, "control_granted") {
				t.Fatal("expected control_granted")
			}

			// Terminate the lease the way the reaper does (or the way an admin
			// revoke does, per the table row).
			close(rw.revoked)

			// readControlFrame t.Fatals if the frame never arrives, which IS
			// the "the client must always be told it lost control" assertion.
			revoked := readControlFrame(t, c, "control_revoked")
			// The client must be able to tell an AGE-OUT from a takeover /
			// trust-withdrawing revoke. Both arrive as control_revoked with
			// by:"" for a reaper-driven end, so without this discriminator the
			// "the client can silently re-acquire" justification for demoting
			// instead of closing had nothing to act on (review B3): the
			// on-open standing auto-present never re-fires on a socket that
			// deliberately stayed open.
			if revoked.Expiry != tc.expiry {
				t.Fatalf("control_revoked expiry = %v, want %v — an expiry demote and a real revoke must be distinguishable on the wire",
					revoked.Expiry, tc.expiry)
			}

			// Now the discriminating assertion: is the SOCKET still alive?
			rctx, rcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
			defer rcancel()
			var readErr error
			for {
				if _, _, rerr := c.Read(rctx); rerr != nil {
					readErr = rerr
					break
				}
			}
			stillOpen := errors.Is(readErr, context.DeadlineExceeded)
			if stillOpen != tc.wantStillOpen {
				t.Fatalf("socket still open = %v (read err %v), want %v", stillOpen, readErr, tc.wantStillOpen)
			}
		})
	}
}

// countingToucher records TouchDeviceSession calls and can report the session
// dead after a set number of them.
type countingToucher struct {
	calls    atomic.Int32
	deadFrom int32 // 0 ⇒ always live
}

func (c *countingToucher) TouchDeviceSession(string) bool {
	n := c.calls.Add(1)
	return c.deadFrom == 0 || n < c.deadFrom
}

// TestViewerSessionHeartbeat pins the "an attached live viewer touches its
// device session" path. Before it, an open terminal viewer produced no HTTP
// requests at all, so nothing refreshed the idle clock and a phone left watching
// a terminal had its device session expire underneath it.
func TestViewerSessionHeartbeat(t *testing.T) {
	t.Run("touches while the viewer stays attached", func(t *testing.T) {
		tr := &countingToucher{}
		done := make(chan struct{})
		go viewerSessionHeartbeat("raw", tr, done, 10*time.Millisecond)
		deadline := time.Now().Add(3 * time.Second)
		for tr.calls.Load() < 3 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		close(done)
		if got := tr.calls.Load(); got < 3 {
			t.Fatalf("heartbeat fired %d times, want >= 3", got)
		}
	})

	t.Run("stops when the viewer disconnects", func(t *testing.T) {
		tr := &countingToucher{}
		done := make(chan struct{})
		go viewerSessionHeartbeat("raw", tr, done, 10*time.Millisecond)
		time.Sleep(50 * time.Millisecond)
		close(done)
		time.Sleep(60 * time.Millisecond)
		settled := tr.calls.Load()
		time.Sleep(120 * time.Millisecond)
		if got := tr.calls.Load(); got != settled {
			t.Fatalf("heartbeat kept firing after disconnect (%d → %d)", settled, got)
		}
	})

	t.Run("stops once the session is no longer live", func(t *testing.T) {
		tr := &countingToucher{deadFrom: 2} // the 2nd touch reports dead
		done := make(chan struct{})
		defer close(done)
		go viewerSessionHeartbeat("raw", tr, done, 10*time.Millisecond)
		time.Sleep(200 * time.Millisecond)
		if got := tr.calls.Load(); got != 2 {
			t.Fatalf("heartbeat fired %d times, want exactly 2 (it must never try to resurrect a dead session)", got)
		}
	})

	t.Run("a non-positive interval is inert", func(t *testing.T) {
		tr := &countingToucher{}
		done := make(chan struct{})
		defer close(done)
		viewerSessionHeartbeat("raw", tr, done, 0) // returns immediately
		if got := tr.calls.Load(); got != 0 {
			t.Fatalf("heartbeat with a 0 interval fired %d times", got)
		}
	})
}

// TestSetTerminalPingPolicy pins the config seam: positive values apply, and
// non-positive ones leave the existing value alone (so a config that omits a key
// — or seeds 0 — never installs a degenerate bound).
func TestSetTerminalPingPolicy(t *testing.T) {
	oi, ot, ob := terminalPingIntervalNs.Load(), terminalPingTimeoutNs.Load(), terminalPingFailureBudget.Load()
	t.Cleanup(func() {
		terminalPingIntervalNs.Store(oi)
		terminalPingTimeoutNs.Store(ot)
		terminalPingFailureBudget.Store(ob)
	})

	SetTerminalPingPolicy(45*time.Second, 7*time.Second, 9)
	if got := terminalPingIntervalNs.Load(); got != int64(45*time.Second) {
		t.Fatalf("interval = %v, want 45s", time.Duration(got))
	}
	if got := terminalPingTimeoutNs.Load(); got != int64(7*time.Second) {
		t.Fatalf("timeout = %v, want 7s", time.Duration(got))
	}
	if got := terminalPingFailureBudget.Load(); got != 9 {
		t.Fatalf("budget = %d, want 9", got)
	}

	// Zeroes are no-ops, not resets.
	SetTerminalPingPolicy(0, 0, 0)
	if terminalPingIntervalNs.Load() != int64(45*time.Second) ||
		terminalPingTimeoutNs.Load() != int64(7*time.Second) ||
		terminalPingFailureBudget.Load() != 9 {
		t.Fatal("a zero-valued policy overwrote the live bounds")
	}
}

// TestAuthTransientIsADistinctWireReason pins that the transient credential
// refusal is NOT reported as "auth". The device UI destroys a saved standing
// secret on an "auth" denial, so a rate-limited or momentarily-disabled acquire
// reporting "auth" wiped a valid secret and forced the operator to mint a new
// one. The two must stay distinguishable on the wire.
func TestAuthTransientIsADistinctWireReason(t *testing.T) {
	if ControlDenialAuthTransient == ControlDenialAuth {
		t.Fatal("the transient refusal must not collapse into the credential rejection")
	}
	if ControlDenialAuthTransient != "auth_transient" {
		t.Fatalf("wire value = %q, want auth_transient", ControlDenialAuthTransient)
	}

	lm := newRecordingLaunchManager(nil)
	lm.remoteErr = NewControlDeniedError(ControlDenialAuthTransient, false, errors.New("rate limited"))
	t.Cleanup(func() { close(lm.sub.release) })
	s := newLaunchTestServer(t, lm)
	ts := remoteExposedWSServer(t, s)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/ws/launch/HANDLE-abc",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {ts.URL}}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	if werr := c.Write(ctx, websocket.MessageText,
		[]byte(`{"t":"acquire-writer","cap":"standing.x","confirm":""}`)); werr != nil {
		t.Fatalf("write acquire: %v", werr)
	}
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			t.Fatalf("read control_denied: %v", rerr)
		}
		if typ != websocket.MessageText {
			continue
		}
		var ctrl wsControl
		if json.Unmarshal(data, &ctrl) == nil && ctrl.T == "control_denied" {
			if ctrl.Reason != ControlDenialAuthTransient {
				t.Fatalf("reason = %q, want %q", ctrl.Reason, ControlDenialAuthTransient)
			}
			return
		}
	}
}
