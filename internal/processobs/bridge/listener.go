package bridge

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// DefaultListenAddr is the accept-mode listener's default bind. It is
// deliberately its own port — not the proxy (:8820), not the browser ingest
// receiver (:8821), not the dashboard — mirroring the own-port posture of
// every other loopback rail in this repo.
const DefaultListenAddr = "127.0.0.1:8823"

// DefaultHandshakeTimeout bounds the authentication exchange. It applies to
// the handshake ONLY: once a capturer is authenticated its stream is
// long-lived and legitimately idle (a quiet process table produces no frames
// for minutes), so the deadline is cleared before the decode loop starts.
const DefaultHandshakeTimeout = 10 * time.Second

// handshakeMagic prefixes the capturer's opening line, so a port scanner, a
// stray browser, or a misconfigured client is rejected with a legible reason
// instead of being fed into the NDJSON decoder.
const handshakeMagic = "SBO-PROCESS-BRIDGE"

// handshakeOK is the listener's acceptance reply. It exists so a capturer can
// tell "connected and authenticated" from "connected, token refused, socket
// about to close" — without it a bad token looks exactly like success on the
// capturer side and the operator gets no signal at all.
const handshakeOK = "OK"

// maxHandshakeBytes bounds the opening line. A token is ~64 hex chars; 4 KiB
// is generous and stops a hostile local client from making us buffer.
const maxHandshakeBytes = 4096

// streamReadBuffer sizes the connection's read buffer. It matches the
// Decoder's own initial scan buffer (wire.go) so a busy capturer is not read
// 4 KiB at a time.
const streamReadBuffer = 64 * 1024

// takeoverGrace bounds how long a replacing connection waits for the
// connection it superseded to finish draining.
const takeoverGrace = 5 * time.Second

// closeGrace bounds how long Close waits for the accept loop to unwind.
// Teardown is best-effort: a wedged stream must not hang daemon shutdown.
const closeGrace = 5 * time.Second

// Listener sentinel errors.
var (
	// ErrNonLoopback is returned by NewListener when the bind address is not
	// loopback and AllowNonLoopback is false (the network-posture guard,
	// modelled on internal/ingest/browser).
	ErrNonLoopback = errors.New("processobs/bridge: refusing non-loopback bind without AllowNonLoopback")
	// ErrTokenRequired is returned by NewListener when no shared token is
	// configured. UNLIKE the browser receiver — whose empty token disables
	// its gate — this listener has NO unauthenticated mode, in production or
	// in tests. WSL2's localhostForwarding means a WSL-side loopback listener
	// is reachable from ANY process on the Windows host, including other
	// users', so the token is the only thing standing between an arbitrary
	// local process and fabricated process rows. See the plan's §W3
	// "Auth is load-bearing here, not decorative".
	ErrTokenRequired = errors.New("processobs/bridge: a shared token is required (the listener has no unauthenticated mode)")
	// ErrBadHandshake is returned when a connection's opening line is not a
	// well-formed capturer handshake.
	ErrBadHandshake = errors.New("processobs/bridge: malformed handshake")
	// ErrBadToken is returned when a connection presents the wrong token. It
	// never carries the presented value.
	ErrBadToken = errors.New("processobs/bridge: invalid token")
)

// ListenerOptions configure the accept-mode Listener.
type ListenerOptions struct {
	// Addr is the host:port bind. Empty defaults to DefaultListenAddr.
	Addr string
	// AllowNonLoopback permits a non-loopback bind (default false). The
	// escape hatch exists for deployments that genuinely need it; it is an
	// explicit operator decision, never a fallback.
	AllowNonLoopback bool
	// Token is the shared secret every capturer must present. REQUIRED —
	// NewListener fails with ErrTokenRequired when it is empty.
	Token string
	// HandshakeTimeout bounds the auth exchange. 0 = DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
	// Logger receives connection/auth/decode diagnostics. Optional.
	Logger *slog.Logger
	// NetworkAccounting optionally receives the connected capturer's
	// per-process network-byte accounting status. nil is the no-op default
	// and means "this listener does not own the handle" — hand it over only
	// where this is the sole byte-capable source (CLAUDE.md rule 4), exactly
	// as the spawn Backend documents.
	NetworkAccounting *processobs.NetworkAccounting
}

// Listener is the ACCEPT half of the cross-OS bridge: a processobs.Backend
// that owns a loopback TCP listener and decodes the NDJSON stream of whichever
// capturer dials IN to it.
//
// It is the direction-inverted sibling of Backend, and the inversion is
// measured, not stylistic: WSL cannot reach a Windows-bound listener
// (127.0.0.1 is WSL's own loopback; the default gateway is dropped by Defender
// on the WSL vNIC), while Windows→WSL loopback works via localhostForwarding.
// So the elevated Windows ETW capturer dials out and the WSL daemon accepts —
// no firewall rule, no host-IP discovery. See
// docs/plans/process-obs-etw-windows-parity-plan-2026-07-26.md §0.2.
//
// RETRY SEMANTICS ARE NOT THE SPAWN BACKEND'S. Backend.loop treats a run that
// produced no events as a failure and permanently gives up after
// maxConsecutiveFailures, closing its channel for the daemon's lifetime. Here
// that would be simply wrong: a capturer that has not connected YET is the
// normal boot state — the elevated Scheduled Task may start minutes later, or
// never. The listener therefore waits INDEFINITELY and never gives up;
// "no capturer has ever connected" is a health state (Stats), not a terminal
// condition.
type Listener struct {
	opts ListenerOptions
	ln   net.Listener

	out      chan processobs.RawEvent
	stop     chan struct{}
	stopOnce sync.Once
	// started/exited let Close wait for the accept loop to fully unwind
	// instead of racing its WaitGroup: only the accept loop ever Adds to wg,
	// so it is also the only thing that may Wait on it.
	started atomic.Bool
	exited  chan struct{}

	claim netClaim
	wg    sync.WaitGroup

	// connMu guards active: at most ONE authenticated capturer streams at a
	// time (see takeover).
	connMu sync.Mutex
	active *acceptedConn

	mu    sync.Mutex
	stats ListenerStats
}

// acceptedConn is one authenticated capturer connection plus a done channel a
// superseding connection can wait on.
type acceptedConn struct {
	conn net.Conn
	done chan struct{}
}

// ListenerStats is a snapshot of the listener's lifetime counters. It is the
// substrate for the §0.4 health surface: an operator whose elevated capturer
// never starts must be able to SEE that, rather than get silence.
type ListenerStats struct {
	// Addr is the resolved bind address.
	Addr string
	// Connections counts capturers that authenticated successfully.
	Connections int64
	// AuthFailures counts connections refused at the handshake — a non-zero
	// value with Connections == 0 means something is dialling and nothing is
	// getting through, which looks nothing like "the task never started".
	// It counts EVERY authenticate() refusal (bad token, unspoken protocol
	// version, malformed opening line, a port scanner), so it names no cause
	// on its own; LastAuthErr does.
	AuthFailures int64
	// Events counts forwarded process events.
	Events int64
	// DecodeErrs counts malformed lines skipped.
	DecodeErrs int64
	// Connected reports whether a capturer is streaming right now.
	Connected bool
	// LastConnectAt / LastDisconnectAt are zero until they happen.
	LastConnectAt    time.Time
	LastDisconnectAt time.Time
	// LastErr is the most recent connection-level error text, from ANY
	// source: an accept error, a stream error, or a refused handshake.
	//
	// It is DELIBERATELY NOT PUBLISHED to the operator surfaces, and that is
	// a decision rather than an oversight. Its dominant value is the normal
	// end of an accept-mode stream — a capturer that exits, or is killed and
	// restarted, resets the socket, and ECONNRESET / "use of closed network
	// connection" lands here on every clean disconnect. Reporting it beside
	// the transport state would render routine capturer restarts as an error
	// on the health line, and the state that surface exists to make visible
	// (connected / waiting / refused) already carries the same information
	// honestly. LastAuthErr below is the half that IS published, because a
	// refusal is never routine.
	//
	// Its readers are this package's Stats() — the in-process diagnostic
	// snapshot used by the listener's own tests and by anything embedding the
	// listener directly. It is not dead state; it is state with a
	// deliberately narrow, in-process audience.
	LastErr string
	// LastAuthErr is the most recent HANDSHAKE-refusal text specifically,
	// with LastAuthAt as its timestamp. It is separate from LastErr because
	// LastErr is overwritten by ordinary stream ends (a capturer going away
	// is normal), and the health surface must be able to say WHY connections
	// are being refused without a disconnect erasing the answer.
	LastAuthErr string
	// CapturerDecode holds the connected capturer's most recent decoder
	// report, and CapturerDecodeReported is the presence flag that makes it
	// readable. CapturerDecodeAt is stamped HERE, on receipt — never taken
	// from the wire.
	//
	// The report REPLACES rather than accumulates, because the counters are
	// the capturer's own CUMULATIVE totals — the heartbeat re-sends the same
	// running total, so summing successive reports would multiply one drop
	// into hundreds. (A capturer PROCESS that is restarted does begin from
	// zero again; replacement is correct there too, because the new process's
	// totals are the ones its bytes came from.) The value therefore reads as
	// "what the most recent capturer report said", which is exactly what the
	// timestamp beside it qualifies.
	//
	// A disconnect does NOT clear it. Unlike a live byte-measurement claim
	// (which netClaim withdraws because it goes stale the instant the
	// capturer dies), a drop count is a historical fact about a run that
	// happened — and erasing it would hide the one signal that says the
	// payload layout is wrong on this host.
	CapturerDecode         processobs.CapturerDecodeStats
	CapturerDecodeReported bool
	CapturerDecodeAt       time.Time
	// LastAuthClass is LastAuthErr's BOUNDED classification, one of the
	// processobs.TransportAuthClass* constants. It is what the metrics
	// exporter may turn into a label; LastAuthErr is for the text surfaces.
	// See processobs.TransportStats.LastAuthErrorClass for why the two are
	// separate facts.
	LastAuthClass string
	LastAuthAt    time.Time
}

// NewListener validates the options, enforces the loopback posture and the
// mandatory token, then OPENS the listener — so a bind conflict fails fast at
// construction (before the daemon reports itself healthy), exactly as
// internal/ingest/browser.New does. Call Start to begin accepting.
func NewListener(opts ListenerOptions) (*Listener, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultListenAddr
	}
	if opts.Token == "" {
		return nil, ErrTokenRequired
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if err := guardLoopback(opts.Addr, opts.AllowNonLoopback); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("processobs/bridge: listen %s: %w", opts.Addr, err)
	}
	l := &Listener{opts: opts, ln: ln, stop: make(chan struct{}), exited: make(chan struct{})}
	l.stats.Addr = ln.Addr().String()
	return l, nil
}

// Name implements processobs.Backend.
func (l *Listener) Name() string { return "bridge-listen" }

// RequiresUnattributedCapture implements processobs.UnattributedCapturer for
// the same reason the spawn Backend does: events arriving over this transport
// were captured on the OTHER OS, so no pidbridge seed can hit at capture time
// and the deferred CorrelateCrossOS pass must join them later (§5.5).
func (l *Listener) RequiresUnattributedCapture() bool { return true }

// Addr reports the resolved bind address (useful when :0 was requested).
func (l *Listener) Addr() string {
	if l.ln == nil {
		return ""
	}
	return l.ln.Addr().String()
}

// Start begins accepting capturer connections and returns the event channel.
// It cannot fail: the bind already happened in NewListener.
func (l *Listener) Start(ctx context.Context) (<-chan processobs.RawEvent, error) {
	l.out = make(chan processobs.RawEvent, 1024)
	// Honest opening state. Nothing is measuring bytes yet because nobody has
	// connected yet — which is a REASON, not a failure, and is exactly what an
	// operator whose elevated task never started needs to read.
	l.opts.NetworkAccounting.Set(processobs.NetworkAccountingUnavailable,
		"waiting for an elevated cross-OS process capturer to connect to "+l.Addr()+" — none has connected yet")
	l.started.Store(true)
	go l.acceptLoop(ctx)
	return l.out, nil
}

// Close stops accepting, drops any live capturer connection, and waits for the
// accept loop to unwind (bounded — teardown is best-effort and must never hang
// daemon shutdown). Idempotent; safe before Start.
func (l *Listener) Close() error {
	l.stopOnce.Do(func() { close(l.stop) })
	if l.ln != nil {
		_ = l.ln.Close()
	}
	l.closeActive()
	if l.started.Load() {
		select {
		case <-l.exited:
		case <-time.After(closeGrace):
		}
	}
	return nil
}

// acceptLoop accepts forever (no give-up cap — see the type doc), handing each
// connection to a goroutine. It owns closing l.out.
func (l *Listener) acceptLoop(ctx context.Context) {
	// LIFO: exited closes LAST, so a Close that observes it knows the channel
	// is closed and every stream goroutine is done.
	defer close(l.exited)
	defer close(l.out)
	// Whatever ends this loop, no capturer is streaming afterwards, so a
	// positive network-accounting claim must not outlive it.
	defer l.claim.invalidate(l.opts.NetworkAccounting,
		"the cross-OS process capturer listener stopped — per-process network bytes are no longer being measured")

	// Unblock Accept (and drop any live connection) when ctx is cancelled.
	// Close covers the l.stop path itself; this goroutine covers ctx, which
	// Close does not observe.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.ln.Close()
			l.closeActive()
		case <-l.stop:
		case <-watchDone:
		}
	}()

	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if l.done(ctx) {
				break
			}
			// A transient accept error (EMFILE, a half-open peer) must not
			// end capture: pause briefly and keep waiting. There is no
			// failure cap here BY DESIGN.
			l.setLastErr(err.Error())
			l.warn("process bridge listener: accept failed — still listening", "err", err)
			if !sleepCtx(ctx, l.stop, 100*time.Millisecond) {
				break
			}
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handleConn(ctx, conn)
		}()
	}
	l.wg.Wait()
}

// handleConn authenticates one connection and, if it passes, streams it.
func (l *Listener) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close() //nolint:errcheck // best-effort close of a finished conn

	br := bufio.NewReaderSize(conn, streamReadBuffer)
	if class, err := l.authenticate(conn, br); err != nil {
		l.countAuthFailure(class, err)
		// The remote address is logged; the presented token never is.
		l.warn("process bridge listener: connection refused", "remote", remoteAddr(conn), "err", err)
		return
	}

	me := l.takeover(conn)
	defer l.release(me)
	l.markConnected()
	l.info("process bridge listener: capturer connected", "remote", remoteAddr(conn))

	// Same NDJSON stream as the spawn transport, byte for byte (stream.go).
	// A terminal transport error is RECORDED, not warned about: a capturer
	// going away mid-read (ECONNRESET, "use of closed network connection") is
	// the normal end of an accept-mode stream, not an incident.
	if _, _, serr := consumeFrames(ctx, br, l); serr != nil {
		l.setLastErr(serr.Error())
	}

	// The capturer is gone. Anything it claimed about live byte counting is
	// now stale, and stale is a lie — withdraw it, exactly as spawn mode does
	// between respawns. The listener itself keeps waiting.
	l.claim.invalidate(l.opts.NetworkAccounting,
		"the cross-OS process capturer disconnected — per-process network bytes are not being measured until it reconnects")
	l.markDisconnected()
}

// authenticate reads and verifies the capturer's opening line, then replies
// with the acceptance line. The deadline covers the handshake ONLY and is
// cleared on success: an authenticated stream is legitimately idle for long
// stretches.
//
// It returns the refusal's BOUNDED class alongside the verbatim error, so the
// caller never has to re-derive a cause by matching on error TEXT — the text
// is the part that quotes remote input, and pattern-matching it would put a
// remote in charge of which class a refusal lands in. class is empty on
// success.
func (l *Listener) authenticate(conn net.Conn, br *bufio.Reader) (class string, err error) {
	if err := conn.SetDeadline(time.Now().Add(l.opts.HandshakeTimeout)); err != nil {
		return processobs.TransportAuthClassTransport,
			fmt.Errorf("processobs/bridge: set handshake deadline: %w", err)
	}
	line, err := readHandshakeLine(br)
	if err != nil {
		// A read that never produced a line is a dead/idle socket, not a
		// statement about a client — except when the line itself was
		// over-long, which readHandshakeLine reports as a malformed
		// handshake.
		if errors.Is(err, ErrBadHandshake) {
			return processobs.TransportAuthClassMalformed, err
		}
		return processobs.TransportAuthClassTransport, err
	}
	token, class, err := parseHandshake(line)
	if err != nil {
		return class, err
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(l.opts.Token)) != 1 {
		return processobs.TransportAuthClassTokenMismatch, ErrBadToken
	}
	if _, err := io.WriteString(conn, handshakeMagic+"/"+strconv.Itoa(WireVersion)+" "+handshakeOK+"\n"); err != nil {
		return processobs.TransportAuthClassTransport,
			fmt.Errorf("processobs/bridge: write handshake ack: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return processobs.TransportAuthClassTransport,
			fmt.Errorf("processobs/bridge: clear handshake deadline: %w", err)
	}
	return "", nil
}

// takeover installs conn as THE active capturer, closing and draining whatever
// was active before.
//
// Why replace rather than refuse: an elevated task that is killed and restarted
// can leave a half-open connection on our side that will not EOF for minutes
// (TCP keepalive), and refusing the new connection would make reconnect
// unreliable exactly when it matters. Replacement is only reachable AFTER
// authentication, so it is not a lever an unauthenticated local process has.
func (l *Listener) takeover(conn net.Conn) *acceptedConn {
	me := &acceptedConn{conn: conn, done: make(chan struct{})}
	l.connMu.Lock()
	prev := l.active
	l.active = me
	l.connMu.Unlock()
	if prev == nil {
		return me
	}
	_ = prev.conn.Close()
	select {
	case <-prev.done:
	case <-time.After(takeoverGrace):
		l.warn("process bridge listener: superseded capturer connection did not drain within the grace window")
	}
	return me
}

// release clears the active slot (when it is still ours) and unblocks anyone
// waiting on this connection in takeover.
func (l *Listener) release(me *acceptedConn) {
	l.connMu.Lock()
	if l.active == me {
		l.active = nil
	}
	l.connMu.Unlock()
	close(me.done)
}

// closeActive drops the live capturer connection, if any.
func (l *Listener) closeActive() {
	l.connMu.Lock()
	a := l.active
	l.connMu.Unlock()
	if a != nil {
		_ = a.conn.Close()
	}
}

// --- frameSink (stream.go): the accept transport's half of the shared decode
// loop. Identical rules to the spawn Backend's, by construction.

func (l *Listener) onHello(h Hello) { l.claim.apply(l.opts.NetworkAccounting, h) }

func (l *Listener) onDecodeErr(err error) {
	l.mu.Lock()
	l.stats.DecodeErrs++
	l.mu.Unlock()
	l.warn("process bridge listener: decode error (line skipped)", "err", err)
}

func (l *Listener) onWireMismatch(got, want int) {
	l.warn("process bridge listener: capturer wire-version mismatch", "got", got, "want", want)
}

func (l *Listener) onErrorFrame(msg string) {
	l.warn("process bridge listener: capturer reported error", "err", msg)
}

// onCapturerStats records the capturer's latest decoder report. See
// ListenerStats.CapturerDecode for why it replaces rather than accumulates and
// why a disconnect does not clear it.
//
// A drop is warned about IN THE LOG as well as counted, because it is the one
// capturer-side condition that makes the byte totals silently wrong rather
// than merely absent.
func (l *Listener) onCapturerStats(s CapturerStats) {
	decode := s.DecodeStats()
	l.mu.Lock()
	prev := l.stats.CapturerDecode
	reported := l.stats.CapturerDecodeReported
	l.stats.CapturerDecode = decode
	l.stats.CapturerDecodeReported = true
	l.stats.CapturerDecodeAt = time.Now()
	l.mu.Unlock()
	// Only on a CHANGE, so a 30s heartbeat carrying a standing non-zero count
	// does not turn one real problem into a log every half minute.
	if !reported || decode != prev {
		switch {
		case decode.Any():
			l.warn("process bridge listener: the capturer's network-telemetry decoder is REFUSING events — "+
				"per-process byte totals from this host may be wrong, not merely missing",
				"dropped", decode.NetworkDropped,
				"unsupported_version", decode.NetworkUnsupportedVersion)
		case decode.NothingClassified():
			// The E6b shape: nothing was refused, so the loud branch above
			// stays silent — and the byte totals are zero anyway. Logged
			// because a decoder that accepts nothing is not a decoder, and
			// the refusal counters cannot say so.
			l.warn("process bridge listener: the capturer's network-telemetry decoder has classified NO data events — "+
				"it refused nothing, but it accepted nothing either, so it is measuring no bytes at all; "+
				"if this host was moving TCP traffic, the provider's event ids no longer match this build's layout table",
				"ignored", decode.NetworkIgnored,
				"decoded", decode.NetworkDecoded)
		}
	}
}

func (l *Listener) onEvent(ctx context.Context, ev processobs.RawEvent) bool {
	l.mu.Lock()
	l.stats.Events++
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return false
	case <-l.stop:
		return false
	case l.out <- ev:
		return true
	}
}

// Stats returns a snapshot of the lifetime counters.
func (l *Listener) Stats() ListenerStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats
}

// TransportStats implements processobs.TransportStatsSource: it maps this
// listener's own counters onto the backend-agnostic shape the health surface
// re-publishes, so `observer doctor` and /metrics can report "no capturer has
// ever connected" and "the capturer's token is wrong" as the distinct states
// they are.
//
// ok is always true: this listener IS a dial-in transport, so its stats are
// always meaningful — including (and especially) when every counter is zero,
// which is exactly the never-connected state the surface exists to show.
func (l *Listener) TransportStats() (processobs.TransportStats, bool) {
	s := l.Stats()
	return processobs.TransportStats{
		Addr:               s.Addr,
		Connections:        s.Connections,
		AuthFailures:       s.AuthFailures,
		LastAuthError:      clampRemoteText(s.LastAuthErr),
		LastAuthErrorClass: s.LastAuthClass,
		LastAuthFailureAt:  s.LastAuthAt,
		Connected:          s.Connected,
		LastConnectAt:      s.LastConnectAt,
		LastDisconnectAt:   s.LastDisconnectAt,
		// Carried WITH its presence flag, never as bare counters: a capturer
		// that never reported must not scrape as "zero events dropped".
		CapturerDecode:         s.CapturerDecode,
		CapturerDecodeReported: s.CapturerDecodeReported,
		CapturerDecodeAt:       s.CapturerDecodeAt,
	}, true
}

// maxPublishedRemoteText bounds every string this package publishes to the
// health surface that a REMOTE may have influenced.
//
// Two such strings exist and they arrive on opposite sides of the
// authentication gate. Pre-auth: the handshake-refusal reason, always one of
// THIS package's own error strings and never carrying a presented token — but
// two of them quote a remote-supplied fragment (the unparsable protocol
// version). Post-auth: the connected capturer's network-accounting reason,
// taken verbatim from its hello frame and bounded only by the 1 MiB NDJSON
// line budget. This bind is reachable from any process on the Windows host
// under WSL's localhostForwarding, so BOTH would otherwise flow, unbounded,
// into a doctor line and a Prometheus label. One clamp, applied at both
// boundaries — a second implementation is a second place to forget.
//
// The cap is generous enough that every real string either side produces fits
// whole; only a hostile-length one is cut, and the cut is marked rather than
// silent.
const maxPublishedRemoteText = 240

func clampRemoteText(s string) string {
	if len(s) <= maxPublishedRemoteText {
		return s
	}
	// Cut on a rune boundary: the tail may be attacker-supplied bytes, and a
	// half rune would travel into a JSON record and a metrics label as
	// invalid UTF-8.
	cut := maxPublishedRemoteText
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + "… (truncated)"
}

// Compile-time proof that the listener satisfies the capability the health
// surface probes for — a signature drift would otherwise degrade silently to
// "no transport configured" (rendered as silence) rather than fail the build.
var _ processobs.TransportStatsSource = (*Listener)(nil)

func (l *Listener) markConnected() {
	l.mu.Lock()
	l.stats.Connections++
	l.stats.Connected = true
	l.stats.LastConnectAt = time.Now()
	l.mu.Unlock()
}

func (l *Listener) markDisconnected() {
	l.mu.Lock()
	l.stats.Connected = false
	l.stats.LastDisconnectAt = time.Now()
	l.mu.Unlock()
}

// countAuthFailure records one refused handshake. class is the bounded
// classification the authenticate path produced; anything outside the closed
// vocabulary is normalised rather than stored, so the field's value set is a
// property of this build and never of what a client sent.
func (l *Listener) countAuthFailure(class string, err error) {
	l.mu.Lock()
	l.stats.AuthFailures++
	if err != nil {
		l.stats.LastErr = err.Error()
		l.stats.LastAuthErr = err.Error()
		l.stats.LastAuthClass = processobs.NormalizeTransportAuthClass(class)
		l.stats.LastAuthAt = time.Now()
	}
	l.mu.Unlock()
}

func (l *Listener) setLastErr(s string) {
	l.mu.Lock()
	l.stats.LastErr = s
	l.mu.Unlock()
}

func (l *Listener) warn(msg string, args ...any) {
	if l.opts.Logger != nil {
		l.opts.Logger.Warn(msg, args...)
	}
}

func (l *Listener) info(msg string, args ...any) {
	if l.opts.Logger != nil {
		l.opts.Logger.Info(msg, args...)
	}
}

// done reports whether the listener should stop (ctx cancelled or Close).
func (l *Listener) done(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	select {
	case <-l.stop:
		return true
	default:
		return false
	}
}

// Handshake performs the CAPTURER side of the accept-mode handshake over conn:
// it presents the shared token as its first act, then waits for the daemon's
// acceptance line. A refused token surfaces here as an error rather than as a
// silently-dropped stream.
//
// It is exported because the capturer lives in cmd/observer; the wire format
// stays owned by this package so the two halves cannot drift.
func Handshake(conn net.Conn, token string, timeout time.Duration) error {
	if token == "" {
		return ErrTokenRequired
	}
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("processobs/bridge: set handshake deadline: %w", err)
	}
	if _, err := io.WriteString(conn, handshakeMagic+"/"+strconv.Itoa(WireVersion)+" "+token+"\n"); err != nil {
		return fmt.Errorf("processobs/bridge: write handshake: %w", err)
	}
	line, err := readHandshakeLine(bufio.NewReaderSize(conn, maxHandshakeBytes))
	if err != nil {
		return fmt.Errorf("processobs/bridge: no acceptance from the daemon (wrong token, or the listener closed): %w", err)
	}
	want := handshakeMagic + "/" + strconv.Itoa(WireVersion) + " " + handshakeOK
	if line != want {
		return fmt.Errorf("%w: unexpected acceptance line", ErrBadHandshake)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("processobs/bridge: clear handshake deadline: %w", err)
	}
	return nil
}

// readHandshakeLine reads one bounded, newline-terminated line and trims the
// CR/LF. An over-long line is a rejection, never an unbounded buffer — both
// because the reader itself is fixed-size and because the line is explicitly
// capped at maxHandshakeBytes regardless of how big that reader is.
func readHandshakeLine(br *bufio.Reader) (string, error) {
	raw, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", fmt.Errorf("%w: opening line exceeds the read buffer", ErrBadHandshake)
	}
	if err != nil {
		return "", fmt.Errorf("%w: read opening line: %w", ErrBadHandshake, err)
	}
	if len(raw) > maxHandshakeBytes {
		return "", fmt.Errorf("%w: opening line exceeds %d bytes", ErrBadHandshake, maxHandshakeBytes)
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}

// parseHandshake validates the "SBO-PROCESS-BRIDGE/<v> <token>" opening line
// and returns the presented token.
//
// A protocol version we do not speak is REFUSED rather than warned about:
// WireVersion moves only on an INCOMPATIBLE change (wire.go), so continuing
// would mean decoding frames whose semantics we have already been told differ.
// That is the opposite call from the hello-frame check, which only warns —
// deliberately: by then the connection is already authenticated and streaming,
// and dropping it would lose events we can still read.
//
// It returns the refusal's bounded class (processobs.TransportAuthClass*)
// alongside the verbatim error. The class is decided HERE, at the point the
// case is known structurally — never later by matching on the error text,
// which is precisely the string that quotes remote input.
func parseHandshake(line string) (token, class string, err error) {
	head, rest, ok := strings.Cut(line, " ")
	if !ok {
		return "", processobs.TransportAuthClassMalformed,
			fmt.Errorf("%w: expected %q, got a single field", ErrBadHandshake, handshakeMagic+"/<v> <token>")
	}
	magic, version, ok := strings.Cut(head, "/")
	if !ok || magic != handshakeMagic {
		return "", processobs.TransportAuthClassMalformed,
			fmt.Errorf("%w: not a %s client", ErrBadHandshake, handshakeMagic)
	}
	v, cerr := strconv.Atoi(version)
	if cerr != nil {
		return "", processobs.TransportAuthClassProtocolVersion,
			fmt.Errorf("%w: unparsable protocol version %q", ErrBadHandshake, version)
	}
	if v != WireVersion {
		return "", processobs.TransportAuthClassProtocolVersion,
			fmt.Errorf("%w: capturer speaks protocol v%d, this daemon speaks v%d", ErrBadHandshake, v, WireVersion)
	}
	token = strings.TrimSpace(rest)
	if token == "" {
		return "", processobs.TransportAuthClassMalformed,
			fmt.Errorf("%w: empty token", ErrBadHandshake)
	}
	return token, "", nil
}

// guardLoopback enforces the network posture: a bind address must resolve to
// loopback unless the operator explicitly allowed otherwise. Mirrors
// internal/ingest/browser.guardLoopback.
func guardLoopback(addr string, allow bool) error {
	if allow {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("processobs/bridge: bad addr %q: %w", addr, err)
	}
	if host == "" || host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%w: %q", ErrNonLoopback, addr)
	}
	return nil
}

// remoteAddr is a nil-safe RemoteAddr string for diagnostics.
func remoteAddr(conn net.Conn) string {
	if a := conn.RemoteAddr(); a != nil {
		return a.String()
	}
	return "unknown"
}
