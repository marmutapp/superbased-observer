package bridge

import (
	"context"
	"errors"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

const testToken = "5f4dcc3b5aa765d61d8327deb882cf99"

// startTestListener opens a listener on an ephemeral loopback port and starts
// it, returning the listener, its event channel, and the shared status handle.
func startTestListener(t *testing.T, ctx context.Context) (*Listener, <-chan processobs.RawEvent, *processobs.NetworkAccounting) {
	t.Helper()
	status := &processobs.NetworkAccounting{}
	l, err := NewListener(ListenerOptions{
		Addr:              "127.0.0.1:0",
		Token:             testToken,
		HandshakeTimeout:  2 * time.Second,
		NetworkAccounting: status,
	})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	ch, err := l.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, ch, status
}

// dialTestCapturer performs the capturer side of the handshake against l.
func dialTestCapturer(t *testing.T, l *Listener, token string) (net.Conn, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", l.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if herr := Handshake(conn, token, 2*time.Second); herr != nil {
		_ = conn.Close()
		return nil, herr
	}
	return conn, nil
}

func execEvent(pid int) processobs.RawEvent {
	return processobs.RawEvent{
		Type: processobs.EventExec, PID: pid, PPID: 1,
		StartTimeTicks: int64(pid), HasStartTime: true, BootID: "win-boot",
	}
}

// recvEvent waits for one event on ch.
func recvEvent(t *testing.T, ch <-chan processobs.RawEvent) processobs.RawEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed unexpectedly")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return processobs.RawEvent{}
	}
}

// TestNewListenerRequiresToken pins the deliberate divergence from
// internal/ingest/browser, whose empty token DISABLES its gate: this listener
// has no unauthenticated mode at all, because localhostForwarding makes a
// WSL-side loopback bind reachable from the whole Windows host.
func TestNewListenerRequiresToken(t *testing.T) {
	l, err := NewListener(ListenerOptions{Addr: "127.0.0.1:0"})
	if !errors.Is(err, ErrTokenRequired) {
		t.Fatalf("NewListener without a token = %v, want ErrTokenRequired", err)
	}
	if l != nil {
		t.Fatal("a refused listener must not be returned")
	}
}

// TestNewListenerLoopbackGuard pins the network posture: a non-loopback bind
// is refused with the sentinel unless explicitly allowed.
func TestNewListenerLoopbackGuard(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr error
	}{
		{name: "public ip refused", addr: "10.1.2.3:8823", wantErr: ErrNonLoopback},
		{name: "wildcard refused", addr: "0.0.0.0:8823", wantErr: ErrNonLoopback},
		{name: "hostname refused", addr: "example.com:8823", wantErr: ErrNonLoopback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewListener(ListenerOptions{Addr: tt.addr, Token: testToken})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewListener(%q) = %v, want %v", tt.addr, err, tt.wantErr)
			}
		})
	}
}

// TestParseHandshake pins the opening-line grammar, including the deliberate
// refusal of a protocol version we do not speak.
func TestParseHandshake(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantToken string
		wantErr   bool
		wantClass string
	}{
		{name: "well formed", line: "SBO-PROCESS-BRIDGE/1 abc123", wantToken: "abc123"},
		{name: "no space", line: "SBO-PROCESS-BRIDGE/1", wantErr: true, wantClass: processobs.TransportAuthClassMalformed},
		{name: "wrong magic", line: "GET / HTTP/1.1", wantErr: true, wantClass: processobs.TransportAuthClassMalformed},
		{name: "no version", line: "SBO-PROCESS-BRIDGE abc123", wantErr: true, wantClass: processobs.TransportAuthClassMalformed},
		{name: "unparsable version", line: "SBO-PROCESS-BRIDGE/x abc123", wantErr: true, wantClass: processobs.TransportAuthClassProtocolVersion},
		{name: "incompatible version", line: "SBO-PROCESS-BRIDGE/99 abc123", wantErr: true, wantClass: processobs.TransportAuthClassProtocolVersion},
		{name: "empty token", line: "SBO-PROCESS-BRIDGE/1 ", wantErr: true, wantClass: processobs.TransportAuthClassMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, class, err := parseHandshake(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseHandshake(%q) = %q, want an error", tt.line, got)
				}
				if !errors.Is(err, ErrBadHandshake) {
					t.Fatalf("error %v does not wrap ErrBadHandshake", err)
				}
				// Every refusal carries a BOUNDED class decided
				// structurally here, not re-derived downstream by
				// matching on error text that quotes remote input.
				if class != tt.wantClass {
					t.Fatalf("parseHandshake(%q) class = %q, want %q", tt.line, class, tt.wantClass)
				}
				return
			}
			if err != nil || got != tt.wantToken {
				t.Fatalf("parseHandshake(%q) = (%q, %v), want (%q, nil)", tt.line, got, err, tt.wantToken)
			}
		})
	}
}

// TestListenerStreamsAuthenticatedCapturer is the happy path over real
// loopback TCP: handshake, hello, events out the Backend channel.
func TestListenerStreamsAuthenticatedCapturer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, status := startTestListener(t, ctx)

	// Before anyone connects the status must say so — honestly, and without
	// claiming a failure that has not happened.
	mode, reason := status.Status()
	if mode != processobs.NetworkAccountingUnavailable || !strings.Contains(reason, "none has connected yet") {
		t.Fatalf("pre-connect status = (%q, %q), want unavailable/none-connected-yet", mode, reason)
	}

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()

	enc := NewEncoder(conn)
	if err := enc.Hello(Hello{
		Backend: "poll+etw", BootID: "win-boot", OS: "windows", PID: 42,
		NetworkAccountingMode: processobs.NetworkAccountingTCP,
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := enc.Event(execEvent(100)); err != nil {
		t.Fatalf("event: %v", err)
	}

	if ev := recvEvent(t, ch); ev.PID != 100 || ev.BootID != "win-boot" {
		t.Fatalf("unexpected event %+v", ev)
	}
	// The capturer's own accounting claim must cross the transport.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if m, _ := status.Status(); m == processobs.NetworkAccountingTCP {
			break
		}
		if time.Now().After(deadline) {
			m, r := status.Status()
			t.Fatalf("hello status never applied: (%q, %q)", m, r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := l.Stats(); s.Connections != 1 || s.Events != 1 || s.AuthFailures != 0 {
		t.Fatalf("stats = %+v, want 1 connection / 1 event / 0 auth failures", s)
	}
}

// TestListenerRefusesBadToken pins that the token is enforced, that the
// refusal is visible in health (AuthFailures), and that no event from an
// unauthenticated peer can reach the pipeline.
func TestListenerRefusesBadToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, _ := startTestListener(t, ctx)

	if _, err := dialTestCapturer(t, l, "wrong-token"); err == nil {
		t.Fatal("expected the handshake to fail with a bad token")
	}

	// A rejected peer's frames must never be decoded. Write some anyway on a
	// fresh connection that skips the handshake entirely.
	raw, err := net.DialTimeout("tcp", l.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	_ = NewEncoder(raw).Event(execEvent(666))

	select {
	case ev := <-ch:
		t.Fatalf("an unauthenticated peer's event reached the pipeline: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	if s := l.Stats(); s.Connections != 0 || s.Events != 0 {
		t.Fatalf("stats = %+v, want 0 connections / 0 events", s)
	}
	waitFor(t, 2*time.Second, func() bool { return l.Stats().AuthFailures >= 1 })
}

// TestListenerReconnectAndNeverGivesUp is the semantic that MUST differ from
// the spawn Backend. Backend.loop counts an event-less run as a failure and
// permanently closes its channel after maxConsecutiveFailures; for an
// accept-side listener that would be wrong, because a capturer that has not
// connected (or connected and produced nothing) is the normal boot state.
//
// So: run maxConsecutiveFailures+2 event-less connections, then a real one,
// and assert events still flow.
func TestListenerReconnectAndNeverGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, _ := startTestListener(t, ctx)

	for i := 0; i < maxConsecutiveFailures+2; i++ {
		conn, err := dialTestCapturer(t, l, testToken)
		if err != nil {
			t.Fatalf("connect %d: %v", i, err)
		}
		_ = conn.Close() // zero events, immediate disconnect
	}
	// Every one of those was accepted, none was fatal.
	waitFor(t, 3*time.Second, func() bool { return l.Stats().Connections >= int64(maxConsecutiveFailures+2) })

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("final connect: %v", err)
	}
	defer conn.Close()
	if err := NewEncoder(conn).Event(execEvent(7)); err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev := recvEvent(t, ch); ev.PID != 7 {
		t.Fatalf("unexpected event %+v", ev)
	}
}

// TestListenerWithdrawsStaleNetworkClaimOnDisconnect pins the rule the spawn
// backend already enforces between respawns: a capturer's positive "I am
// counting bytes" claim dies with the connection, because a stale positive
// claim is a lie. A reconnect then re-asserts it.
func TestListenerWithdrawsStaleNetworkClaimOnDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, status := startTestListener(t, ctx)

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	enc := NewEncoder(conn)
	if err := enc.Hello(Hello{Backend: "poll+etw", NetworkAccountingMode: processobs.NetworkAccountingTCP}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := enc.Event(execEvent(1)); err != nil {
		t.Fatalf("event: %v", err)
	}
	recvEvent(t, ch) // the hello has certainly been processed by now
	if m, _ := status.Status(); m != processobs.NetworkAccountingTCP {
		t.Fatalf("status = %q, want tcp", m)
	}

	_ = conn.Close()
	waitFor(t, 3*time.Second, func() bool {
		m, _ := status.Status()
		return m == processobs.NetworkAccountingUnavailable
	})
	if _, reason := status.Status(); !strings.Contains(reason, "disconnected") {
		t.Fatalf("withdrawal reason = %q, want it to name the disconnect", reason)
	}

	// Reconnect: the claim is re-asserted, so a blink does not permanently
	// mark a live capture as unavailable.
	conn2, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("reconnect handshake: %v", err)
	}
	defer conn2.Close()
	enc2 := NewEncoder(conn2)
	if err := enc2.Hello(Hello{Backend: "poll+etw", NetworkAccountingMode: processobs.NetworkAccountingTCP}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := enc2.Event(execEvent(2)); err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev := recvEvent(t, ch); ev.PID != 2 {
		t.Fatalf("unexpected event after reconnect: %+v", ev)
	}
	waitFor(t, 3*time.Second, func() bool {
		m, _ := status.Status()
		return m == processobs.NetworkAccountingTCP
	})
}

// TestListenerCloseClosesChannel pins the Backend lifecycle contract: Close
// stops the listener, drops the live connection, and closes the event channel.
func TestListenerCloseClosesChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, _ := startTestListener(t, ctx)

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close (must be a no-op): %v", err)
	}
	select {
	case _, open := <-ch:
		if open {
			// A buffered event may drain first; the next receive must close.
			select {
			case _, open2 := <-ch:
				if open2 {
					t.Fatal("channel still open after Close")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("channel not closed after Close")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("channel not closed after Close")
	}
}

// TestListenerNameAndCapability pins the two things the daemon wiring reads.
func TestListenerNameAndCapability(t *testing.T) {
	l, err := NewListener(ListenerOptions{Addr: "127.0.0.1:0", Token: testToken})
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	defer l.Close()
	if got := l.Name(); got != "bridge-listen" {
		t.Fatalf("Name() = %q", got)
	}
	if !l.RequiresUnattributedCapture() {
		t.Fatal("cross-OS events cannot be attributed at capture time; want true")
	}
}

// TestListenerTransportStatsLifecycle drives the capability the health
// surface reads (processobs.TransportStatsSource) through the four states an
// operator can actually be in, against a REAL listener rather than a struct
// literal: never connected, refused on a bad token, live, and disconnected.
//
// The never-connected state is the important one: it must report ok=true with
// zero counters — "a transport exists and nobody has dialled it" — because the
// caller distinguishes that from ok=false ("no transport configured") and
// renders them completely differently.
func TestListenerTransportStatsLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, _ := startTestListener(t, ctx)

	ts, ok := l.TransportStats()
	if !ok {
		t.Fatal("the listener IS a transport; want ok=true even before any capturer connects")
	}
	if ts.Addr != l.Addr() || ts.Connections != 0 || ts.AuthFailures != 0 || ts.Connected ||
		!ts.LastConnectAt.IsZero() || !ts.LastDisconnectAt.IsZero() {
		t.Fatalf("never-connected stats = %+v (addr want %q)", ts, l.Addr())
	}

	// Wrong token: AuthFailures climbs, Connections stays 0 — the state that
	// is otherwise invisible.
	if _, err := dialTestCapturer(t, l, "wrong-token"); err == nil {
		t.Fatal("expected the handshake to fail with a bad token")
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.AuthFailures >= 1
	})
	if ts, _ = l.TransportStats(); ts.Connections != 0 || ts.Connected {
		t.Fatalf("a refused capturer must not count as connected: %+v", ts)
	}

	// Right token: live, with a connect timestamp.
	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if werr := NewEncoder(conn).Event(execEvent(4242)); werr != nil {
		t.Fatalf("write event: %v", werr)
	}
	recvEvent(t, ch)
	ts, _ = l.TransportStats()
	if !ts.Connected || ts.Connections != 1 || ts.LastConnectAt.IsZero() {
		t.Fatalf("live stats = %+v", ts)
	}
	if ts.AuthFailures != 1 {
		t.Errorf("the earlier refusal must still be counted, got %d", ts.AuthFailures)
	}

	// Gone: Connected drops, and BOTH timestamps are set — which is how a
	// flapping capturer becomes visible.
	_ = conn.Close()
	waitFor(t, 3*time.Second, func() bool {
		s, _ := l.TransportStats()
		return !s.Connected && !s.LastDisconnectAt.IsZero()
	})
	ts, _ = l.TransportStats()
	if ts.Connections != 1 || ts.LastConnectAt.IsZero() {
		t.Fatalf("disconnected stats lost history: %+v", ts)
	}
}

// TestListenerTransportStatsAuthReason is the empirical proof that
// AuthFailures conflates causes and that only LastAuthError names one.
//
// It drives a REAL listener with three handshakes that are NOT token
// problems, plus one that is, and pins that each publishes its own verbatim
// reason. Before this field existed the surfaces read only the counter and
// asserted "shared-token mismatch" for all four — sending an operator whose
// token is already correct to fix the token.
func TestListenerTransportStatsAuthReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, _, _ := startTestListener(t, ctx)

	// rawHandshake sends one opening line verbatim and lets the listener
	// refuse it — the capturer-side Handshake helper cannot express a wrong
	// protocol version or a port scanner.
	rawHandshake := func(line string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", l.Addr(), 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close()
		if _, werr := conn.Write([]byte(line + "\n")); werr != nil {
			t.Fatalf("write handshake: %v", werr)
		}
	}

	cases := []struct {
		name       string
		send       func()
		wantSubstr string
		// wantAbsent is the whole point: a non-token failure must not carry
		// token vocabulary into the operator's diagnosis.
		wantAbsent []string
	}{
		{
			name:       "protocol version this daemon does not speak",
			send:       func() { rawHandshake(handshakeMagic + "/" + strconv.Itoa(WireVersion+1) + " " + testToken) },
			wantSubstr: "speaks protocol v" + strconv.Itoa(WireVersion+1),
			wantAbsent: []string{"token"},
		},
		{
			name:       "a port scanner is not a capturer",
			send:       func() { rawHandshake("GET / HTTP/1.1") },
			wantSubstr: "not a " + handshakeMagic + " client",
			wantAbsent: []string{"token"},
		},
		{
			name:       "an unparsable version says so",
			send:       func() { rawHandshake(handshakeMagic + "/vNEXT " + testToken) },
			wantSubstr: `unparsable protocol version "vNEXT"`,
			wantAbsent: []string{"invalid token"},
		},
		{
			name:       "and a real token mismatch says THAT",
			send:       func() { _, _ = dialTestCapturer(t, l, "wrong-token") },
			wantSubstr: "invalid token",
		},
	}

	var seen int64
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.send()
			seen++
			waitFor(t, 2*time.Second, func() bool {
				s, _ := l.TransportStats()
				return s.AuthFailures >= seen && s.LastAuthError != ""
			})
			ts, _ := l.TransportStats()
			if !strings.Contains(ts.LastAuthError, tc.wantSubstr) {
				t.Fatalf("LastAuthError = %q, want it to contain %q", ts.LastAuthError, tc.wantSubstr)
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(ts.LastAuthError, absent) {
					t.Errorf("LastAuthError %q names %q — this refusal was not a token problem", ts.LastAuthError, absent)
				}
			}
			if ts.LastAuthFailureAt.IsZero() {
				t.Error("the refusal reason must be stamped so an aggregate can pick the most recent")
			}
			if ts.Connections != 0 {
				t.Errorf("a refused connection must not count as connected: %+v", ts)
			}
		})
	}

	// A refusal reason must survive a later stream-level error: LastErr is
	// overwritten by ordinary disconnects, LastAuthErr is not.
	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = conn.Close()
	waitFor(t, 3*time.Second, func() bool {
		s, _ := l.TransportStats()
		return !s.Connected && s.Connections == 1
	})
	if ts, _ := l.TransportStats(); !strings.Contains(ts.LastAuthError, "invalid token") {
		t.Errorf("a disconnect erased the refusal reason: %q", ts.LastAuthError)
	}
}

// TestClampAuthErr pins the one bound on the published reason. The bind is
// reachable from any process on the Windows host under WSL localhostForwarding
// and two refusal strings quote a remote-supplied fragment, so an unbounded
// reason would flow into a doctor line and a Prometheus label.
func TestClampRemoteText(t *testing.T) {
	t.Parallel()
	short := "processobs/bridge: invalid token"
	if got := clampRemoteText(short); got != short {
		t.Errorf("a real reason must survive whole: %q", got)
	}
	long := strings.Repeat("x", maxPublishedRemoteText*3)
	got := clampRemoteText(long)
	if len(got) >= len(long) || !strings.HasSuffix(got, "(truncated)") {
		t.Errorf("a hostile-length reason must be cut and MARKED as cut, got %d bytes", len(got))
	}
	// The tail is remote-supplied bytes, so the cut must land on a rune
	// boundary — a half rune would travel into a JSON record and a metrics
	// label as invalid UTF-8.
	multibyte := strings.Repeat("é", maxPublishedRemoteText)
	if cut := clampRemoteText(multibyte); !utf8.ValidString(cut) {
		t.Errorf("clamped reason is not valid UTF-8: %q", cut)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}

// TestNetClaimBoundsRemoteHelloFields is the M3 boundary proof: the two
// hello fields a capturer supplies are REMOTE INPUT and must be bounded and
// vocabulary-checked before they reach NetworkAccounting — which is exactly
// the value that becomes a doctor line and a Prometheus label.
//
// Authentication does not change this. The token is a file on the same
// Windows host whose every process can dial this bind, so it buys trust in
// the sender, never in the shape of what the sender sends.
func TestNetClaimBoundsRemoteHelloFields(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("A", 200000)
	tests := []struct {
		name          string
		hello         Hello
		wantMode      string
		wantMeasuring bool
		check         func(t *testing.T, reason string)
	}{
		{
			name:     "a known live mode is adopted as-is",
			hello:    Hello{NetworkAccountingMode: processobs.NetworkAccountingTCP, NetworkAccountingReason: "attached"},
			wantMode: processobs.NetworkAccountingTCP, wantMeasuring: true,
			check: func(t *testing.T, reason string) {
				if reason != "attached" {
					t.Errorf("a short, well-formed reason must survive whole: %q", reason)
				}
			},
		},
		{
			name: "an INVENTED mode cannot assert a positive accounting claim",
			// Neither "off" nor "unavailable", so IsMeasuringNetworkMode
			// alone would read it as live measurement — under a name nothing
			// in this build has ever seen, and with an embedded quote headed
			// for a metrics label.
			hello:    Hello{NetworkAccountingMode: "tcp\"injected"},
			wantMode: processobs.NetworkAccountingUnavailable, wantMeasuring: false,
			check: func(t *testing.T, reason string) {
				if !strings.Contains(reason, "unrecognised network-accounting mode") {
					t.Errorf("the rejection must SAY it rejected something, not go silent: %q", reason)
				}
			},
		},
		{
			name:     "a 200 000-byte reason is clamped",
			hello:    Hello{NetworkAccountingMode: processobs.NetworkAccountingUnavailable, NetworkAccountingReason: huge},
			wantMode: processobs.NetworkAccountingUnavailable, wantMeasuring: false,
			check: func(t *testing.T, reason string) {
				if len(reason) > maxPublishedRemoteText+len("… (truncated)") {
					t.Errorf("reason is %d bytes; the only cap must not be the 1 MiB NDJSON line budget", len(reason))
				}
				if !strings.HasSuffix(reason, "(truncated)") {
					t.Errorf("a cut reason must be MARKED as cut: %q", reason)
				}
			},
		},
		{
			name:     "an invented mode's own bytes are clamped where they are quoted",
			hello:    Hello{NetworkAccountingMode: huge},
			wantMode: processobs.NetworkAccountingUnavailable, wantMeasuring: false,
			check: func(t *testing.T, reason string) {
				if len(reason) > 2*maxPublishedRemoteText {
					t.Errorf("quoting the offending mode reintroduced the unbounded field: %d bytes", len(reason))
				}
			},
		},
		{
			name:     "an omitted mode still writes nothing",
			hello:    Hello{NetworkAccountingReason: "ignored without a mode"},
			wantMode: processobs.NetworkAccountingOff, wantMeasuring: false,
			check: func(t *testing.T, reason string) {
				if reason != "" {
					t.Errorf("silence from a pre-W2 capturer must not become a claim: %q", reason)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var c netClaim
			handle := &processobs.NetworkAccounting{}
			c.apply(handle, tt.hello)
			mode, reason := handle.Status()
			if mode != tt.wantMode {
				t.Fatalf("mode = %q, want %q", mode, tt.wantMode)
			}
			if !processobs.KnownNetworkAccountingMode(mode) {
				t.Fatalf("a mode outside this build's vocabulary reached the handle: %q", mode)
			}
			if got := IsMeasuringNetworkMode(mode); got != tt.wantMeasuring {
				t.Fatalf("IsMeasuringNetworkMode(%q) = %v, want %v", mode, got, tt.wantMeasuring)
			}
			// The standing-claim bit must agree with the adopted mode, or a
			// disconnect would withdraw a claim that was never made (or
			// leave one that was).
			c.mu.Lock()
			measuring := c.measuring
			c.mu.Unlock()
			if measuring != tt.wantMeasuring {
				t.Fatalf("netClaim.measuring = %v, want %v", measuring, tt.wantMeasuring)
			}
			if !utf8.ValidString(reason) {
				t.Fatalf("reason is not valid UTF-8: %q", reason)
			}
			tt.check(t, reason)
		})
	}
}

// TestListenerBoundsHostileHelloOverTheWire is the same M3 property proven
// end-to-end over real loopback TCP by a capturer that HAS authenticated —
// the exact position the reviewer's live listener reproduced it from.
func TestListenerBoundsHostileHelloOverTheWire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, status := startTestListener(t, ctx)

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()

	if err := NewEncoder(conn).Hello(Hello{
		Backend: "poll+etw", BootID: "win-boot", OS: "windows", PID: 42,
		NetworkAccountingMode:   "tcp\"injected",
		NetworkAccountingReason: strings.Repeat("B", 200000),
	}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	// An event proves the frame was consumed, so the hello ahead of it was
	// too — no sleeping on a hope.
	if err := NewEncoder(conn).Event(execEvent(11)); err != nil {
		t.Fatalf("event: %v", err)
	}
	if ev := recvEvent(t, ch); ev.PID != 11 {
		t.Fatalf("unexpected event %+v", ev)
	}

	mode, reason := status.Status()
	if mode != processobs.NetworkAccountingUnavailable {
		t.Fatalf("an invented mode was adopted over the wire: %q", mode)
	}
	if len(reason) > 2*maxPublishedRemoteText {
		t.Fatalf("the hello reason reached the health handle unbounded: %d bytes", len(reason))
	}
	if strings.Contains(reason, strings.Repeat("B", maxPublishedRemoteText+1)) {
		t.Fatalf("the 200 000-byte reason survived verbatim: %d bytes", len(reason))
	}
}

// TestListenerClassifiesAuthFailures pins the BOUNDED half of a refusal (M4):
// every handshake refusal carries a class drawn from a closed vocabulary this
// build owns, decided structurally at the refusal site. The metrics exporter
// labels on the class precisely because the verbatim reason quotes whatever a
// remote sent, and Prometheus keeps one series per distinct label value
// forever.
func TestListenerClassifiesAuthFailures(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantClass string
	}{
		{
			name:      "a wrong shared secret",
			line:      "SBO-PROCESS-BRIDGE/" + strconv.Itoa(WireVersion) + " definitely-not-the-token\n",
			wantClass: processobs.TransportAuthClassTokenMismatch,
		},
		{
			name:      "an upgrade-skewed capturer",
			line:      "SBO-PROCESS-BRIDGE/" + strconv.Itoa(WireVersion+1) + " " + testToken + "\n",
			wantClass: processobs.TransportAuthClassProtocolVersion,
		},
		{
			name:      "a remote-chosen version fragment",
			line:      "SBO-PROCESS-BRIDGE/" + strings.Repeat("z", 64) + " " + testToken + "\n",
			wantClass: processobs.TransportAuthClassProtocolVersion,
		},
		{
			name:      "something that is not a capturer at all",
			line:      "GET / HTTP/1.1\n",
			wantClass: processobs.TransportAuthClassMalformed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			l, _, _ := startTestListener(t, ctx)

			conn, err := net.DialTimeout("tcp", l.Addr(), 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			if _, err := conn.Write([]byte(tt.line)); err != nil {
				t.Fatalf("write: %v", err)
			}
			waitFor(t, 2*time.Second, func() bool { return l.Stats().AuthFailures >= 1 })

			ts, ok := l.TransportStats()
			if !ok {
				t.Fatal("the listener must always report stats")
			}
			if ts.LastAuthErrorClass != tt.wantClass {
				t.Fatalf("class = %q, want %q (reason %q)", ts.LastAuthErrorClass, tt.wantClass, ts.LastAuthError)
			}
			// The class is bounded; the reason still carries the detail.
			if got := processobs.NormalizeTransportAuthClass(ts.LastAuthErrorClass); got != tt.wantClass {
				t.Fatalf("the published class is outside the closed vocabulary: %q", ts.LastAuthErrorClass)
			}
			if ts.LastAuthError == "" {
				t.Fatal("classifying a refusal must not cost the verbatim reason")
			}
		})
	}
}

// TestListenerCarriesCapturerDecodeStats pins the whole E6 path across the
// transport, including the two silences that make it honest.
//
// The reported/never-reported distinction is the point of the surface: a
// capturer with no running network decoder — every non-elevated run — sends no
// stats frame at all, and "0 events refused" is the OPPOSITE claim from
// "nothing was decoded". Absence must therefore stay absence, and a reported
// zero must stay a real measurement.
func TestListenerCarriesCapturerDecodeStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, _ := startTestListener(t, ctx)

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()

	enc := NewEncoder(conn)
	if err := enc.Hello(Hello{Backend: "poll+etw", BootID: "win-boot", OS: "windows", PID: 42}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := enc.Event(execEvent(100)); err != nil {
		t.Fatalf("event: %v", err)
	}
	recvEvent(t, ch)

	// A connected capturer that has not reported must leave the flag FALSE.
	// Zeroed counters beside a true flag would say the payload-length
	// assumptions were exercised and held.
	ts, ok := l.TransportStats()
	if !ok {
		t.Fatal("TransportStats reported no transport on a live listener")
	}
	if ts.CapturerDecodeReported {
		t.Fatalf("CapturerDecodeReported is true before any stats frame arrived: %+v", ts)
	}
	if !ts.CapturerDecodeAt.IsZero() {
		t.Fatalf("CapturerDecodeAt stamped before any report: %v", ts.CapturerDecodeAt)
	}

	// A genuine zero: the decoder ran and refused nothing.
	if err := enc.Stats(CapturerStats{}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.CapturerDecodeReported
	})
	ts, _ = l.TransportStats()
	if ts.CapturerDecode.Any() {
		t.Fatalf("a zero report registered as a failure: %+v", ts.CapturerDecode)
	}
	if ts.CapturerDecodeAt.IsZero() {
		t.Fatal("CapturerDecodeAt was not stamped on receipt")
	}

	// A later report REPLACES rather than accumulates: the counters are the
	// capturer's own cumulative totals and the heartbeat re-sends the running
	// total, so summing would multiply one drop into many.
	if err := enc.Stats(CapturerStats{NetworkDecodeDropped: 5, NetworkDecodeUnsupportedVersion: 1}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.CapturerDecode.NetworkDropped == 5
	})
	if err := enc.Stats(CapturerStats{NetworkDecodeDropped: 6, NetworkDecodeUnsupportedVersion: 1}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.CapturerDecode.NetworkDropped == 6
	})
	ts, _ = l.TransportStats()
	if ts.CapturerDecode.NetworkUnsupportedVersion != 1 {
		t.Fatalf("counters accumulated instead of replacing: %+v", ts.CapturerDecode)
	}
}

// TestCapturerStatsValidate pins the untrusted-input rule for the numeric half
// of the wire. The listener's bind is reachable from every process on the
// Windows host under WSL localhostForwarding, so an authenticated peer is still
// a peer: a count of events cannot run backwards.
//
// A broken report is REFUSED, not floored to zero. Flooring would turn "this
// report is nonsense" into "the decoder ran and refused nothing", which is the
// one claim this whole surface exists to make trustworthy.
func TestCapturerStatsValidate(t *testing.T) {
	t.Parallel()

	for _, bad := range []CapturerStats{
		{NetworkDecodeDropped: -9},
		{NetworkDecodeUnsupportedVersion: -1},
		{NetworkDecodeDropped: -1, NetworkDecodeUnsupportedVersion: -1},
		// The classification counters join the SAME gate. A negative here is
		// as impossible as a negative drop count, and a report carrying one
		// says nothing about which of its other numbers can be trusted — so
		// the whole frame fails rather than reaching a surface half-credible.
		{NetworkDecodeIgnored: -1},
		{NetworkDecodeDecoded: -1},
		{NetworkDecodeDropped: 3, NetworkDecodeIgnored: -7},
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("validate(%+v) = nil, want a refusal", bad)
		}
	}
	for _, ok := range []CapturerStats{
		{},
		{NetworkDecodeDropped: 3, NetworkDecodeUnsupportedVersion: 4},
		{NetworkDecodeDropped: math.MaxInt64},
		// No upper bound on the ignore count either, and this one matters
		// most: a renumbered provider produces a HUGE ignored count, so a
		// ceiling would refuse exactly the report the operator needs.
		{NetworkDecodeIgnored: math.MaxInt64},
		{NetworkDecodeIgnored: 1_000_000, NetworkDecodeDecoded: 4321},
	} {
		if err := ok.validate(); err != nil {
			t.Errorf("validate(%+v) = %v, want nil — there is no knowable ceiling on a refusal count", ok, err)
		}
	}
}

// TestListenerRefusesBrokenCapturerStats is the wired half: a negative report
// must NOT set the presence flag, because a surface that reads
// CapturerDecodeReported=true with zeroed counters reports a PASS. It is
// counted as a refused line instead, and the stream stays live.
func TestListenerRefusesBrokenCapturerStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, ch, _ := startTestListener(t, ctx)

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()
	enc := NewEncoder(conn)
	if err := enc.Hello(Hello{Backend: "poll+etw", BootID: "win-boot", OS: "windows", PID: 42}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	if err := enc.Stats(CapturerStats{NetworkDecodeDropped: -5}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool { return l.Stats().DecodeErrs >= 1 })
	ts, _ := l.TransportStats()
	if ts.CapturerDecodeReported {
		t.Fatalf("a broken report registered as a decoder measurement: %+v", ts)
	}
	if ts.CapturerDecode.Any() {
		t.Fatalf("a refused report leaked into the counters: %+v", ts.CapturerDecode)
	}

	// The stream is still live: the next heartbeat is accepted normally.
	if err := enc.Event(execEvent(101)); err != nil {
		t.Fatalf("event: %v", err)
	}
	recvEvent(t, ch)
	if err := enc.Stats(CapturerStats{NetworkDecodeDropped: 2}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.CapturerDecodeReported && s.CapturerDecode.NetworkDropped == 2
	})
}

// TestDecoderRejectsOutOfRangeCounters records what the JSON layer does with a
// counter that cannot be an int64 — MEASURED, not assumed, because "the
// decoding is bounded" is exactly the kind of claim that is usually inferred.
//
// An over-large, fractional or non-numeric counter fails the WHOLE frame, so
// the report is dropped rather than truncated or wrapped. Nothing accumulates
// these numbers (each report replaces the last), so no overflow is reachable
// downstream either.
func TestDecoderRejectsOutOfRangeCounters(t *testing.T) {
	t.Parallel()

	for _, line := range []string{
		`{"v":1,"kind":"stats","stats":{"network_decode_dropped":9223372036854775808}}`,
		`{"v":1,"kind":"stats","stats":{"network_decode_dropped":1e30}}`,
		`{"v":1,"kind":"stats","stats":{"network_decode_dropped":1.5}}`,
		`{"v":1,"kind":"stats","stats":{"network_decode_dropped":"12"}}`,
	} {
		dec := NewDecoder(strings.NewReader(line + "\n"))
		if _, err := dec.Next(); err == nil {
			t.Errorf("decoded %s without error; an out-of-range counter must fail the frame", line)
		}
	}
	// The largest representable value is NOT an error — it is merely a very
	// broken host, and refusing it would hide the report that matters most.
	dec := NewDecoder(strings.NewReader(`{"v":1,"kind":"stats","stats":{"network_decode_dropped":9223372036854775807}}` + "\n"))
	f, err := dec.Next()
	if err != nil || f.Stats == nil || f.Stats.NetworkDecodeDropped != math.MaxInt64 {
		t.Fatalf("Next() = %+v, %v; want the max int64 carried through", f, err)
	}
}

// TestListenerCarriesNothingClassifiedSignature is the E6b end-to-end on the
// transport that actually carries the elevated capturer: a report whose
// refusal counters are clean but which classified NO data events must reach
// the daemon's published TransportStats intact, and read as the suspicion it
// is rather than as the PASS every refusal-shaped check would call it.
func TestListenerCarriesNothingClassifiedSignature(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, _, _ := startTestListener(t, ctx)

	conn, err := dialTestCapturer(t, l, testToken)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()
	enc := NewEncoder(conn)
	if err := enc.Hello(Hello{Backend: "poll+etw", BootID: "win-boot", OS: "windows", PID: 42}); err != nil {
		t.Fatalf("hello: %v", err)
	}

	// The renumbered-provider shape: everything landed in ClassIgnored.
	if err := enc.Stats(CapturerStats{NetworkDecodeIgnored: 48_211}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.CapturerDecodeReported
	})
	ts, _ := l.TransportStats()
	if ts.CapturerDecode.NetworkIgnored != 48_211 || ts.CapturerDecode.NetworkDecoded != 0 {
		t.Fatalf("the classification counters did not survive the wire: %+v", ts.CapturerDecode)
	}
	if ts.CapturerDecode.Any() {
		t.Fatalf("an ignored-only report registered as a decode FAULT: %+v", ts.CapturerDecode)
	}
	if !ts.CapturerDecode.NothingClassified() {
		t.Fatalf("the renumbered-provider signature did not reach the daemon: %+v", ts.CapturerDecode)
	}

	// A capture that decodes data events clears it, on the same link.
	if err := enc.Stats(CapturerStats{NetworkDecodeIgnored: 48_500, NetworkDecodeDecoded: 12}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		s, _ := l.TransportStats()
		return s.CapturerDecode.NetworkDecoded == 12
	})
	ts, _ = l.TransportStats()
	if ts.CapturerDecode.NothingClassified() {
		t.Fatalf("a capture that decoded data events still reads as classifying nothing: %+v", ts.CapturerDecode)
	}
}
