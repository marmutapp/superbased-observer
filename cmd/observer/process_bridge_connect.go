package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
)

// processBridgeTokenEnv is the environment variable the capturer accepts the
// shared token in when --token-file is not used.
//
// There is deliberately NO --token flag. argv is world-readable — /proc on
// Linux, ToolHelp/WMI on Windows — and this very tool captures argv into the
// observer DB, so a token on the command line would be a secret we leak
// through our own feature. A file (0600) or the environment is the least-bad
// channel.
const processBridgeTokenEnv = "OBSERVER_PROCESS_BRIDGE_TOKEN" //nolint:gosec // G101: env var NAME, not a credential.

// Reconnect backoff bounds for --connect. The daemon may legitimately not be
// up yet (an elevated Scheduled Task can start before it), so a failed dial is
// the NORMAL boot state and is retried forever — the capturer never gives up,
// mirroring the listener's own no-give-up posture.
const (
	connectMinBackoff  = 1 * time.Second
	connectMaxBackoff  = 30 * time.Second
	connectDialTimeout = 10 * time.Second
)

// runProcessBridgeConnect is the capturer's accept-mode entry point: it dials
// the daemon's loopback listener, authenticates, and streams NDJSON frames
// over the socket instead of stdout — reconnecting forever.
//
// The direction is measured, not stylistic (plan §0.2): WSL cannot reach a
// Windows-bound listener, but Windows CAN reach a WSL-bound one via
// localhostForwarding. So the elevated capturer dials OUT and the daemon
// accepts.
//
// streamProcessBridgeWith already takes a plain io.Writer, so the only change
// on this side is WHICH writer: a net.Conn instead of os.Stdout. The stream
// contract, the hello frame and the wire are untouched.
func runProcessBridgeConnect(ctx context.Context, addr, tokenFile string, opts processBridgeOptions, errOut io.Writer) error {
	// ONE ETW capture, held across reconnects. This matters for the §0.1
	// parity contract: the capture's per-pid accumulator is CUMULATIVE, and
	// every consumer differentiates consecutive samples to get a rate. Start a
	// fresh capture on each reconnect and those totals restart at zero, so the
	// next differentiation is negative — a wrong number with no error
	// anywhere, which is the exact failure mode that contract exists to
	// prevent. The poll backend is restarted (its initial snapshot is
	// idempotent — rows upsert on process_key), the byte accumulator is not.
	shared := &sharedETWCapture{start: opts.startETW}
	defer shared.close()
	opts.startETW = shared.get

	backoff := connectMinBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		// RE-READ the token on EVERY attempt, never once at start. The
		// capturer's production trigger is a logon Scheduled Task, which can
		// fire minutes before WSL (and therefore the daemon that GENERATES
		// this file) exists — reading it once would turn the normal boot
		// ordering into a permanent exit. Re-reading also picks up a token the
		// daemon regenerated after the file was removed, instead of retrying
		// forever with a stale secret.
		token, terr := resolveProcessBridgeToken(tokenFile)
		if terr != nil {
			if tokenFile == "" {
				// Nothing to wait FOR: no --token-file was given and the
				// environment carries no token, so this is a usage error and
				// no amount of retrying changes it.
				return terr
			}
			fmt.Fprintf(errOut, "process-bridge: %v — retrying in %s\n", terr, backoff)
			if !sleepProcessBridge(ctx, backoff) {
				return nil
			}
			backoff = nextConnectBackoff(backoff)
			continue
		}
		conn, derr := dialProcessBridge(ctx, addr, token)
		if derr != nil {
			fmt.Fprintf(errOut, "process-bridge: connect to %s failed: %v — retrying in %s\n", addr, derr, backoff)
			if !sleepProcessBridge(ctx, backoff) {
				return nil
			}
			backoff = nextConnectBackoff(backoff)
			continue
		}
		backoff = connectMinBackoff

		// Notice a departed daemon PROMPTLY. streamProcessBridgeWith only
		// learns the socket is dead when its next Encoder write fails, so
		// without this the capturer would keep streaming into a closed pipe
		// for up to a whole poll interval (and on a quiet host, longer). The
		// daemon says nothing after its handshake ack, so ANY read return
		// means it is gone: that is the cheapest possible liveness signal and
		// it needs no wire change.
		streamCtx, streamCancel := context.WithCancel(ctx)
		go watchDaemonHangup(conn, streamCancel)

		serr := streamProcessBridgeWith(streamCtx, conn, opts)
		streamCancel()
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil
		}
		if serr != nil {
			// A backend Start failure is not a transport problem and
			// reconnecting cannot fix it — surface it instead of spinning.
			return serr
		}
		fmt.Fprintf(errOut, "process-bridge: the daemon closed the stream — reconnecting to %s in %s\n", addr, backoff)
		if !sleepProcessBridge(ctx, backoff) {
			return nil
		}
	}
}

// watchDaemonHangup blocks until the connection produces anything at all —
// EOF, a reset, or (impossibly) a byte — and then cancels the stream. The
// protocol is one-way after the handshake ack, so a read that returns is by
// definition the daemon going away. The goroutine unblocks when the caller
// closes the conn.
func watchDaemonHangup(conn net.Conn, cancel context.CancelFunc) {
	defer cancel()
	var buf [1]byte
	_, _ = conn.Read(buf[:])
}

// dialProcessBridge opens one authenticated connection to the daemon.
func dialProcessBridge(ctx context.Context, addr, token string) (net.Conn, error) {
	d := net.Dialer{Timeout: connectDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if err := bridge.Handshake(conn, token, 0); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// resolveProcessBridgeToken reads the shared token from --token-file, else
// from the environment. It never accepts one from argv (see
// processBridgeTokenEnv).
func resolveProcessBridgeToken(path string) (string, error) {
	if path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied path, by design.
		if err != nil {
			return "", fmt.Errorf("process-bridge: read --token-file: %w", err)
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			return "", fmt.Errorf("process-bridge: --token-file %s is empty", path)
		}
		return tok, nil
	}
	if tok := strings.TrimSpace(os.Getenv(processBridgeTokenEnv)); tok != "" {
		return tok, nil
	}
	return "", fmt.Errorf("process-bridge: --connect requires the daemon's shared token — pass --token-file <path> or set %s "+
		"(there is deliberately no --token flag: argv is world-readable, and this tool captures argv)", processBridgeTokenEnv)
}

// sharedETWCapture memoizes ONE etwNetworkCapture across reconnects and hides
// its Close from the per-connection stream, which would otherwise stop the
// trace session (and reset the cumulative accumulator) every time the daemon
// blinks. The real Close runs once, when the connect loop exits.
type sharedETWCapture struct {
	// start is the underlying platform starter; nil means the real one.
	start func() (etwNetworkCapture, error)

	once    sync.Once
	capture etwNetworkCapture
	err     error
}

// get is a drop-in for processBridgeOptions.startETW: it starts the capture on
// first use and hands out a non-closing view of it thereafter. A start FAILURE
// is memoized too — a non-elevated capturer gets ERROR_ACCESS_DENIED on every
// attempt, so retrying it per reconnect would only add noise.
func (s *sharedETWCapture) get() (etwNetworkCapture, error) {
	s.once.Do(func() {
		start := s.start
		if start == nil {
			start = startETWNetworkCapture
		}
		s.capture, s.err = start()
	})
	if s.err != nil {
		return nil, s.err
	}
	return nopCloseCapture{s.capture}, nil
}

// close stops the underlying capture, if one ever started.
func (s *sharedETWCapture) close() {
	if s.capture != nil {
		_ = s.capture.Close()
	}
}

// nopCloseCapture is an etwNetworkCapture whose Close is a no-op, so a stream
// that ends does not tear down a capture the next stream will reuse.
type nopCloseCapture struct{ etwNetworkCapture }

// Close is deliberately a no-op — sharedETWCapture owns the real lifecycle.
func (nopCloseCapture) Close() error { return nil }

// nextConnectBackoff doubles up to the ceiling.
func nextConnectBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > connectMaxBackoff {
		return connectMaxBackoff
	}
	return d
}

// sleepProcessBridge waits d, returning false if ctx fires first.
func sleepProcessBridge(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
