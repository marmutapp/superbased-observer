package attachsock

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// handshakeTimeout bounds how long the server waits for a client's spawn frame
// before giving up on the connection, so a dead client cannot hold a connection
// open pre-spawn. Long-lived idle connections AFTER spawn are normal (an
// attached agent may sit at a prompt), so no per-frame read deadline applies
// once the session is live.
const handshakeTimeout = 5 * time.Second

// exitPollTimeout bounds how long the output pump waits, after the PTY output
// stream hits EOF, for the child's exit code to become observable via
// Session.ExitCode. The manager records the exit slightly after the pty master
// closes, so a short poll avoids racing a known code into an "unknown" report
// (A4). If it stays unknown past the budget we send exit{known:false} and the
// client reports an honest failure instead of a fabricated 0.
const (
	exitPollTimeout  = 2 * time.Second
	exitPollInterval = 20 * time.Millisecond
)

// shutdownGrace bounds how long Serve waits for in-flight connection handlers to
// drain after ctx cancel before returning, so a wedged handler cannot pin
// shutdown forever (A6).
const shutdownGrace = 3 * time.Second

// SpawnRequest is the server-side view of a client's spawn control frame.
type SpawnRequest struct {
	// Tool is the target tool name (e.g. "claude-code").
	Tool string
	// Subcommand is the observer launcher verb (e.g. "claude").
	Subcommand string
	// Dir is the requested child cwd ("" = launcher default).
	Dir string
	// Rows / Cols are the initial PTY size.
	Rows uint16
	Cols uint16
	// Env is the extra child environment ("KEY=VALUE") the launcher forwards.
	Env []string
	// ExtraArgs are the allow-listed argv tokens appended to the inner
	// `observer <Subcommand>` launcher (routing escape hatch + `--` tool
	// remainder). Explicit + allow-listed by the client (B2/B3).
	ExtraArgs []string
	// AutoResumeCapable is true when the client understands the frameCorrelated
	// announce (resilient-attach Layer 1); the server pushes that frame only
	// then. Absent for a pre-Layer-1 client (decoded false).
	AutoResumeCapable bool
	// ResumeSession is the agent session id this attach resumes (any resume-
	// attach), used by the Host's double-spawn guard. Empty for a non-resume
	// attach.
	ResumeSession string
	// AutoResume marks a daemon-death auto-resume (vs a user-initiated resume),
	// so the Host applies the rediscovered-orphan validation gate only then.
	AutoResume bool
}

// CorrelatedSession is one correlated-session announcement the server relays to
// an AutoResumeCapable client as a frameCorrelated frame. SessionID is always a
// REAL id (the producer abstains on an unresolved correlation — it never emits a
// fabricated one). Source/Confidence carry the provenance the client surfaces.
type CorrelatedSession struct {
	SessionID  string
	Source     string
	Confidence float64
}

// CorrelationSource is an OPTIONAL capability a Session may implement to feed
// the server correlated-session announcements (resilient-attach Layer 1). When a
// Session implements it AND the client advertised AutoResumeCapable, the server
// relays each announcement as a frameCorrelated frame. A Session that does not
// implement it (or whose run never correlates) simply never sends the frame —
// the client then has no auto-resume target and degrades to the resume hint.
type CorrelationSource interface {
	// CorrelatedSessions returns a channel delivering correlation announcements
	// for this session's run. The implementation MUST close the channel when the
	// session ends (Detach), so the server's relay goroutine exits.
	CorrelatedSessions() <-chan CorrelatedSession
}

// Session is a daemon-owned PTY the server bridges to one attach client. It is
// satisfied by a cmd adapter over the termsession Manager (Subscribe +
// AcquireWriterLocal).
type Session interface {
	// Handle is the opaque PTY handle (also the dashboard join key).
	Handle() string
	// RunID is the run identity minted for this attach launch.
	RunID() string
	// Output returns a replay-then-tail reader over the PTY ring. Its Read
	// returns io.EOF once the child has exited and the ring is drained; any
	// other error (e.g. the viewer was unsubscribed on Detach) means "stop
	// streaming" without implying the child died.
	Output() io.Reader
	// Write forwards raw bytes to the PTY (the client's keystrokes). A write
	// that returns ErrWriterRevoked means the writer lease was fenced out (a
	// dashboard/other seat took over) while the PTY session lives on — the
	// server relays a non-fatal CodeWriterRevoked notice and keeps streaming.
	Write([]byte) (int, error)
	// Resize resizes the PTY.
	Resize(rows, cols uint16) error
	// ExitCode returns the child's exit code once known.
	ExitCode() (int, bool)
	// Detach releases the writer lease and unsubscribes the viewer WITHOUT
	// killing the child — the PTY lives on for the dashboard and other viewers.
	// It must be safe to call more than once.
	Detach()
}

// Host launches daemon-owned PTYs on behalf of attach clients.
type Host interface {
	// LaunchAttachable spawns a PTY for req and returns the live Session, or an
	// error the server relays as a spawn_failed control frame.
	LaunchAttachable(ctx context.Context, req SpawnRequest) (Session, error)
}

// server holds shared state for one Serve loop.
type server struct {
	host   Host
	logger *slog.Logger

	wg    sync.WaitGroup // tracks in-flight connection handlers (A6)
	mu    sync.Mutex
	conns map[*frameConn]net.Conn
}

// Serve accepts attach connections on ln until ctx is canceled or ln fails.
// Each connection is handled in its own goroutine: read the spawn frame, launch
// via host, reply spawned, then bridge PTY output ↔ client stdin/resize until
// the child exits (exit frame) or the client detaches/drops (Session.Detach,
// child lives on). On ctx cancel every live connection gets a best-effort
// daemon_shutdown error frame and is closed. A clean shutdown returns nil.
func Serve(ctx context.Context, ln net.Listener, host Host, logger *slog.Logger) error {
	if host == nil {
		return errors.New("attachsock.Serve: nil host")
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &server{host: host, logger: logger, conns: make(map[*frameConn]net.Conn)}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
		s.broadcastShutdown()
	}()

	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				// Graceful shutdown: the ctx-cancel goroutine has closed the
				// listener and broadcast a shutdown frame + closed every live
				// conn. Wait (bounded) for the handlers to drain so we don't
				// return while goroutines still touch the host/PTY (A6).
				s.waitHandlers(shutdownGrace)
				return nil
			}
			return fmt.Errorf("attachsock.Serve: accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(ctx, nc)
		}()
	}
}

// waitHandlers blocks until every in-flight connection handler returns, or
// grace elapses — whichever comes first. Bounded so a single wedged handler
// cannot pin daemon shutdown (A6).
func (s *server) waitHandlers(grace time.Duration) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		if s.logger != nil {
			s.logger.Warn("attachsock: shutdown grace elapsed with connection handlers still in flight")
		}
	}
}

// track/untrack maintain the live-connection set for shutdown broadcast.
func (s *server) track(fc *frameConn, nc net.Conn) {
	s.mu.Lock()
	s.conns[fc] = nc
	s.mu.Unlock()
}

func (s *server) untrack(fc *frameConn) {
	s.mu.Lock()
	delete(s.conns, fc)
	s.mu.Unlock()
}

// broadcastShutdown sends a daemon_shutdown error to every live connection and
// closes it. Best-effort: send/close errors are ignored.
func (s *server) broadcastShutdown() {
	s.mu.Lock()
	live := make([]struct {
		fc *frameConn
		nc net.Conn
	}, 0, len(s.conns))
	for fc, nc := range s.conns {
		live = append(live, struct {
			fc *frameConn
			nc net.Conn
		}{fc, nc})
	}
	s.mu.Unlock()
	for _, c := range live {
		_ = c.fc.sendError(CodeDaemonShutdown, "observer daemon is shutting down")
		_ = c.nc.Close()
	}
}

// serveConn handles a single attach connection end to end.
func (s *server) serveConn(ctx context.Context, nc net.Conn) {
	fc := newFrameConn(nc)
	s.track(fc, nc)
	defer s.untrack(fc)
	defer func() { _ = nc.Close() }()

	// Handshake: bounded wait for the spawn frame so a dead pre-spawn client
	// cannot pin the connection.
	_ = nc.SetReadDeadline(time.Now().Add(handshakeTimeout))
	sm, err := s.readSpawn(fc)
	if err != nil {
		if errors.Is(err, ErrProtocol) {
			_ = fc.sendError(CodeProtocol, err.Error())
		}
		return
	}
	_ = nc.SetReadDeadline(time.Time{}) // clear; idle live connections are fine

	sess, err := s.host.LaunchAttachable(ctx, SpawnRequest{
		Tool:              sm.Tool,
		Subcommand:        sm.Subcommand,
		Dir:               sm.Dir,
		Rows:              sm.Rows,
		Cols:              sm.Cols,
		Env:               sm.Env,
		ExtraArgs:         sm.ExtraArgs,
		AutoResumeCapable: sm.AutoResumeCapable,
		ResumeSession:     sm.ResumeSession,
		AutoResume:        sm.AutoResume,
	})
	if err != nil {
		// A resume-guard refusal maps to its own code so the client prints why
		// and does NOT spawn a duplicate / loop; every other launch failure is a
		// generic spawn_failed (Layer-1 double-spawn guard + orphan validation).
		_ = fc.sendError(spawnErrorCode(err), err.Error())
		return
	}

	// Detach exactly once, whether the child exits or the client drops. Detach
	// never kills the child — it lives on for the dashboard/other viewers.
	var detachOnce sync.Once
	detach := func() { detachOnce.Do(sess.Detach) }
	defer detach()

	if err := fc.sendControl(spawnedMsg{Op: opSpawned, Handle: sess.Handle(), RunID: sess.RunID()}); err != nil {
		return
	}

	// Correlation relay (resilient-attach Layer 1): when the client advertised
	// AutoResumeCapable AND the Session can source correlation announcements,
	// push each REAL correlated-session id as a frameCorrelated frame so the
	// client retains an auto-resume target. Gated on the capability so a pre-
	// Layer-1 client never receives an unknown frame. Tracked in the shutdown
	// WaitGroup like the output pump. The relay ends when the Session's channel
	// closes (Detach) or a frame write fails.
	if sm.AutoResumeCapable {
		if cs, ok := sess.(CorrelationSource); ok {
			if ch := cs.CorrelatedSessions(); ch != nil {
				s.wg.Add(1)
				go func() {
					defer s.wg.Done()
					s.relayCorrelations(fc, ch)
				}()
			}
		}
	}

	// Output pump: PTY ring → frameOutput frames. io.EOF ⇒ child exit ⇒ send
	// exit frame then close the connection to unblock the read loop. Tracked in
	// the shutdown WaitGroup so a graceful shutdown waits for it too, not just
	// the read-loop handler (A2-5).
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pumpOutput(fc, nc, sess)
	}()

	// Read loop: client stdin/resize/detach → PTY. Ends on client drop, detach,
	// child exit (conn closed by the pump), or a protocol violation.
	s.readLoop(fc, sess)
}

// readSpawn reads and validates the initial spawn control frame.
func (s *server) readSpawn(fc *frameConn) (spawnMsg, error) {
	t, payload, err := fc.readFrame()
	if err != nil {
		return spawnMsg{}, err
	}
	if t != frameControl {
		return spawnMsg{}, fmt.Errorf("%w: expected a control spawn frame, got type %d", ErrProtocol, t)
	}
	op, err := peekOp(payload)
	if err != nil {
		return spawnMsg{}, err
	}
	if op != opSpawn {
		return spawnMsg{}, fmt.Errorf("%w: expected op %q, got %q", ErrProtocol, opSpawn, op)
	}
	var sm spawnMsg
	if err := unmarshalControl(payload, &sm); err != nil {
		return spawnMsg{}, err
	}
	if sm.V != ProtocolVersion {
		return spawnMsg{}, fmt.Errorf("%w: unsupported protocol version %d (want %d)", ErrProtocol, sm.V, ProtocolVersion)
	}
	if sm.Tool == "" || sm.Subcommand == "" {
		return spawnMsg{}, fmt.Errorf("%w: spawn requires tool and subcommand", ErrProtocol)
	}
	return sm, nil
}

// pumpOutput copies the PTY ring to the client, terminating the session on
// child exit.
func (s *server) pumpOutput(fc *frameConn, nc net.Conn, sess Session) {
	buf := make([]byte, dataFrameMax)
	out := sess.Output()
	for {
		n, err := out.Read(buf)
		if n > 0 {
			if werr := fc.writeData(frameOutput, buf[:n]); werr != nil {
				_ = nc.Close()
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// EOF on the output ring means the child's PTY closed. The
				// manager records the exit code slightly after the master
				// closes, so poll briefly (A4) rather than reporting whatever
				// ExitCode returns this instant — a not-yet-known code must be
				// reported as unknown, never fabricated as 0.
				code, known := s.awaitExitCode(sess)
				_ = fc.sendControl(exitMsg{Op: opExit, Code: code, Known: known})
			}
			// Either the child exited (exit frame sent above) or the viewer was
			// unsubscribed on Detach / the conn broke — in every case stop and
			// unblock the read loop.
			_ = nc.Close()
			return
		}
	}
}

// relayCorrelations forwards correlated-session announcements to the client as
// frameCorrelated frames until the channel closes (session ended) or a write
// fails (the conn is gone — the read/output loops handle the teardown). It
// forwards only announcements carrying a REAL session id; an empty id is dropped
// (defensive — the producer already abstains). A write error simply ends the
// relay; it never tears the session down (the output pump owns exit).
func (s *server) relayCorrelations(fc *frameConn, ch <-chan CorrelatedSession) {
	for c := range ch {
		if c.SessionID == "" {
			continue
		}
		if err := fc.sendCorrelated(correlatedMsg{
			SessionID:  c.SessionID,
			Source:     c.Source,
			Confidence: c.Confidence,
		}); err != nil {
			return
		}
	}
}

// spawnErrorCode maps a Host launch error to the wire error code the client
// keys off. A resume-guard refusal (double-spawn / not-a-daemon-death-orphan)
// gets its own code so the client prints why and neither loops nor spawns a
// duplicate; anything else is a generic spawn_failed.
func spawnErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrResumeConflict):
		return CodeResumeConflict
	case errors.Is(err, ErrResumeNotResumable):
		return CodeResumeNotResumable
	default:
		return CodeSpawnFailed
	}
}

// awaitExitCode polls Session.ExitCode until it becomes known or exitPollTimeout
// elapses. Returns (code, known); known=false means the daemon could not
// determine the exit within the budget (A4).
func (s *server) awaitExitCode(sess Session) (int, bool) {
	deadline := time.Now().Add(exitPollTimeout)
	for {
		if code, ok := sess.ExitCode(); ok {
			return code, true
		}
		if time.Now().After(deadline) {
			return 0, false
		}
		time.Sleep(exitPollInterval)
	}
}

// readLoop consumes client frames until the connection ends or a protocol
// violation occurs. It never kills the child; the deferred detach in serveConn
// releases the writer + viewer.
func (s *server) readLoop(fc *frameConn, sess Session) {
	for {
		t, payload, err := fc.readFrame()
		if err != nil {
			if errors.Is(err, ErrProtocol) {
				_ = fc.sendError(CodeProtocol, err.Error())
			}
			return
		}
		switch t {
		case frameStdin:
			if _, werr := sess.Write(payload); werr != nil {
				if errors.Is(werr, ErrWriterRevoked) {
					// Non-fatal: the lease was fenced out but the session lives
					// on. Tell the client once (it dedupes) and keep the loop
					// running so output still streams (A5).
					_ = fc.sendError(CodeWriterRevoked, "writer control was taken over — your keystrokes are not reaching the session")
				} else if s.logger != nil {
					// Any other write error is worth a server-side line rather
					// than a silent swallow (A5).
					s.logger.Warn("attachsock: session write failed", "err", werr)
				}
			}
		case frameControl:
			op, perr := peekOp(payload)
			if perr != nil {
				_ = fc.sendError(CodeProtocol, perr.Error())
				return
			}
			switch op {
			case opResize:
				var rm resizeMsg
				if err := unmarshalControl(payload, &rm); err != nil {
					_ = fc.sendError(CodeProtocol, err.Error())
					return
				}
				if rerr := sess.Resize(rm.Rows, rm.Cols); rerr != nil && s.logger != nil {
					s.logger.Warn("attachsock: session resize failed", "err", rerr)
				}
			case opDetach:
				return
			default:
				// Forward-compat (review finding M7): an UNKNOWN control op from a
				// newer client is fully consumed (peekOp already parsed the whole
				// payload) and SKIPPED, not fatal — mirroring the unknown-FRAME-type
				// tolerance below and the client's own tolerant read loop, so the
				// control channel is bidirectionally forward-compatible. Only
				// MALFORMED frames (peekOp/unmarshal errors, length/junk) stay fatal
				// protocol errors; op-code unknownness alone is tolerated.
				if s.logger != nil {
					s.logger.Debug("attachsock: ignoring unknown control op", "op", op)
				}
				continue
			}
		default:
			// Forward-compat: a size-bounded unknown frame type from a newer
			// client is SKIPPED, not fatal (the stream stays synced via the
			// length prefix). frameStdin/frameControl are the only types a client
			// legitimately sends today; a future one is ignored here rather than
			// poisoning the session.
			continue
		}
	}
}

// ErrSocketLiveDaemon is returned by ListenSocket when the socket path is
// already being served by a live daemon — detected primarily by a HELD listen
// lock (A3-5) and secondarily by a successful probe-dial (A9). We refuse to
// steal the path from a running server.
var ErrSocketLiveDaemon = errors.New("attachsock: attach socket already served by a live daemon")

// errLockHeld is returned by acquireListenLock (unix) when another process
// already holds the attach listen lock — a live daemon. ListenSocket maps it to
// ErrSocketLiveDaemon. Defined here (platform-neutral) so both the unix
// acquireListenLock and this mapping can reference it.
var errLockHeld = errors.New("attachsock: attach listen lock is held by another process")

// lockedListener wraps a net.Listener whose Close ALSO owns the socket-path
// unlink AND releases the held listen lock, so the flock lives exactly as long
// as the listener (A3-5) and the path is removed by — and only by — the holder
// that bound it (F3).
type lockedListener struct {
	net.Listener
	lock      *listenLock
	path      string      // the bound socket path (owned unlink target)
	boundInfo os.FileInfo // stat of the socket we bound, for a SameFile unlink guard
	closeOnce sync.Once
}

// Close closes the underlying listener, unlinks OUR socket path, then releases
// the held listen lock — strictly in that order, exactly once. The ordering is
// load-bearing (F3): releasing the flock BEFORE the unlink would let a
// replacement daemon acquire the lock and bind a NEW socket at the same path,
// which our unlink would then destroy. Removing the path while still holding
// the lock closes that window. The os.SameFile guard is belt-and-braces —
// even under the held lock we only remove the path if it still refers to the
// exact inode we bound (the stdlib's own unlink-on-close is disabled in
// ListenSocket so this is the single unlink site).
func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() {
		if l.path != "" && l.boundInfo != nil {
			if cur, statErr := os.Stat(l.path); statErr == nil && os.SameFile(l.boundInfo, cur) {
				_ = os.Remove(l.path)
			}
		}
		l.lock.release()
	})
	return err
}

// ListenSocket creates the owner-only attach socket at path. Security model
// (A1): the socket's PARENT DIRECTORY is created 0700 (owner rwx only), so
// connect(2) — which requires execute (search) permission on the parent dir —
// is OS-enforced owner-only with NO race window, regardless of the socket
// file's own mode or the process umask. We additionally chmod the socket 0600
// (belt-and-braces). Before binding we refuse to steal a path a LIVE daemon is
// serving (A9): an existing socket is probe-dialed and only unlinked when the
// dial fails (stale). A non-socket file at the path is never removed.
func ListenSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	// Create the parent 0700. MkdirAll won't tighten an existing looser dir, so
	// we stat + chmod it below to guarantee owner-only search permission.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("attachsock.ListenSocket: create attach dir %q: %w", dir, err)
	}
	dfi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("attachsock.ListenSocket: stat attach dir %q: %w", dir, err)
	}
	if !dfi.IsDir() {
		return nil, fmt.Errorf("attachsock.ListenSocket: attach path parent %q is not a directory", dir)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		// A pre-existing dir may be looser (e.g. the DB dir shared with other
		// state). Tighten it to owner-only so connect() is owner-enforced.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("attachsock.ListenSocket: tighten attach dir %q perms (%o→0700): %w", dir, perm, err)
		}
	}

	// A3-5/A6/A9: acquire the listen lock and HOLD it for the listener's
	// lifetime. A NON-BLOCKING flock is the primary live-daemon detector — if
	// another daemon holds it, we bail with ErrSocketLiveDaemon BEFORE any
	// probe/unlink, removing the probe-false-negative steal window (a daemon
	// whose accept loop is momentarily wedged would fail the probe-dial yet
	// still hold the lock). Once acquired the lock also serializes the
	// stat→probe→remove→listen sequence against a second local daemon. On
	// platforms without flock (attach is Linux/WSL-only in v1) it is a no-op.
	lock, lerr := acquireListenLock(filepath.Join(dir, "attach.lock"))
	if lerr != nil {
		if errors.Is(lerr, errLockHeld) {
			return nil, fmt.Errorf("attachsock.ListenSocket: %w at %q", ErrSocketLiveDaemon, path)
		}
		return nil, fmt.Errorf("attachsock.ListenSocket: acquire listen lock: %w", lerr)
	}
	// The lock is HELD from here. Release it on every error path below; on
	// success the returned lockedListener's Close releases it (A3-5).
	listening := false
	defer func() {
		if !listening {
			lock.release()
		}
	}()

	if fi, err := os.Stat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("attachsock.ListenSocket: refusing to remove non-socket file %q", path)
		}
		// Secondary stale check (A9): the held lock already excludes a live
		// local daemon, but a probe-dial that unexpectedly succeeds means
		// something else is serving the path — bail rather than steal. Only
		// unlink when the dial fails (stale socket from a crashed daemon).
		if c, derr := net.DialTimeout("unix", path, 500*time.Millisecond); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("attachsock.ListenSocket: %w at %q", ErrSocketLiveDaemon, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("attachsock.ListenSocket: remove stale socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("attachsock.ListenSocket: stat %q: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("attachsock.ListenSocket: listen: %w", err)
	}
	// Take ownership of the unlink: the stdlib's default unlink-on-close would
	// remove whatever file sits at the path at Close time, UNORDERED against
	// our flock release — a replacement daemon's freshly-bound socket could be
	// the casualty (F3). lockedListener.Close does the unlink itself, ordered
	// before the lock release and inode-guarded.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("attachsock.ListenSocket: chmod: %w", err)
	}
	// Record the bound socket's identity for the SameFile unlink guard in Close.
	boundInfo, _ := os.Stat(path)
	listening = true
	return &lockedListener{Listener: ln, lock: lock, path: path, boundInfo: boundInfo}, nil
}
