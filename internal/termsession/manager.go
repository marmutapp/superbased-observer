package termsession

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Errors returned by the Manager.
var (
	// ErrInvalidSpec is returned by Create when a required Spec field is
	// missing (BinPath / Subcommand / SessionID).
	ErrInvalidSpec = errors.New("termsession: invalid spec (BinPath, Subcommand and SessionID are required)")
	// ErrTooManySessions is returned when MaxConcurrent live sessions
	// already exist. The dashboard surfaces it honestly (429-shaped).
	ErrTooManySessions = errors.New("termsession: too many concurrent terminal sessions")
	// ErrNotFound is returned by Attach/Resize/Close for an unknown handle.
	ErrNotFound = errors.New("termsession: session not found")
	// ErrAlreadyAttached is returned when a second client tries to attach
	// to a session that already has a live attachment.
	ErrAlreadyAttached = errors.New("termsession: session already has an attached client")
	// errDetached unblocks a session Read when its client detaches while the
	// process is still alive (Detach closes the subscription). The websocket
	// bridge treats any read error as "stop reading", so it is unexported.
	errDetached = errors.New("termsession: client detached")
)

// Default lifecycle bounds. All overridable via Options.
const (
	defaultMaxConcurrent = 4
	defaultIdleTimeout   = 30 * time.Minute
	defaultExitLinger    = 30 * time.Second
	defaultReapInterval  = 10 * time.Second
)

// Options configures a Manager. The zero value is usable (OS spawner,
// sane bounds, wall-clock).
type Options struct {
	// Spawner creates the PTY-backed process. Nil defaults to the OS
	// spawner (creack/pty on unix; unsupported on native Windows).
	Spawner Spawner
	// MaxConcurrent caps live sessions (default 4). Create errors past it.
	MaxConcurrent int
	// IdleTimeout reaps a session whose PTY has seen no I/O for this long
	// (default 30m). Any output or keystroke resets the clock.
	IdleTimeout time.Duration
	// ExitLinger keeps an exited session in the registry this long after
	// the process ends, so a still-attached (or reconnecting) client can
	// read the final output + exit code before it is swept (default 30s).
	ExitLinger time.Duration
	// ReapInterval is the background reaper tick (default 10s).
	ReapInterval time.Duration
	// RingBytes bounds each session's replay ring (default 256 KiB). The
	// always-on pump drains the PTY into this ring so a reconnecting client
	// can replay recent output; older bytes are dropped once it is full.
	RingBytes int
	// Now is the clock (test hook). Nil defaults to time.Now.
	Now func() time.Time
	// Logger receives operational messages. Nil defaults to a discard
	// logger.
	Logger *slog.Logger
}

// Manager is the one-owner registry of live PTY terminal sessions. It is
// safe for concurrent use; the dashboard holds exactly one per process.
type Manager struct {
	spawner       Spawner
	maxConcurrent int
	idleTimeout   time.Duration
	exitLinger    time.Duration
	ringBytes     int
	now           func() time.Time
	logger        *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// NewManager builds a Manager and starts its background reaper. Call
// Shutdown to stop the reaper and kill every live session.
func NewManager(opts Options) *Manager {
	m := &Manager{
		spawner:       opts.Spawner,
		maxConcurrent: opts.MaxConcurrent,
		idleTimeout:   opts.IdleTimeout,
		exitLinger:    opts.ExitLinger,
		ringBytes:     opts.RingBytes,
		now:           opts.Now,
		logger:        opts.Logger,
		sessions:      make(map[string]*Session),
		stop:          make(chan struct{}),
	}
	if m.spawner == nil {
		m.spawner = NewOSSpawner()
	}
	if m.maxConcurrent <= 0 {
		m.maxConcurrent = defaultMaxConcurrent
	}
	if m.idleTimeout <= 0 {
		m.idleTimeout = defaultIdleTimeout
	}
	if m.exitLinger <= 0 {
		m.exitLinger = defaultExitLinger
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.logger == nil {
		m.logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	interval := opts.ReapInterval
	if interval <= 0 {
		interval = defaultReapInterval
	}
	m.wg.Add(1)
	go m.reapLoop(interval)
	return m
}

// Session is one live PTY-backed launcher process. An always-on pump drains
// the PTY into out (a bounded replay ring) so the child never blocks on a
// clientless PTY; Write/Resize proxy to the PTY. Read serves the attached
// client from out, replaying the ring from the start of the attachment then
// tailing live. Done fires when the process exits. The websocket handler holds
// one *Session for the duration of an attachment.
type Session struct {
	handle    string
	spec      Spec
	pty       PTY
	out       *outBuf
	createdAt time.Time
	now       func() time.Time

	lastAct  atomic.Int64 // unixnano of the last PTY I/O
	doneAt   atomic.Int64 // unixnano the process exited (0 = still running)
	exitCode atomic.Int32
	attached atomic.Bool

	// Subscription state for the single attached client. subOff is the
	// reader's absolute cursor into out and is owned solely by that client's
	// Read goroutine (set by beginAttach before the reader starts). subCancel
	// is closed by Detach to unblock a caught-up Read without killing the PTY;
	// subMu guards the field itself against the next Attach's reassignment.
	subMu     sync.Mutex
	subCancel chan struct{}
	subOff    int64

	done     chan struct{}
	doneOnce sync.Once
}

// beginAttach (re)initializes the subscription for a fresh client: replay from
// the start of the ring, new cancel channel. Called by Attach under the
// single-client CAS, before the client's Read goroutine starts.
func (s *Session) beginAttach() {
	s.subMu.Lock()
	s.subOff = 0
	s.subCancel = make(chan struct{})
	s.subMu.Unlock()
}

// cancelSub closes the current subscription's cancel channel, unblocking a
// waiting Read. Called exactly once per attachment (Detach gates it on the
// attached CAS), so it never double-closes.
func (s *Session) cancelSub() {
	s.subMu.Lock()
	if s.subCancel != nil {
		close(s.subCancel)
	}
	s.subMu.Unlock()
}

// subCancelCh returns the current subscription cancel channel (stable for the
// duration of an attachment).
func (s *Session) subCancelCh() <-chan struct{} {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return s.subCancel
}

// Token is the opaque session identifier.
func (s *Session) Token() string { return s.handle }

// Subcommand is the observer launcher verb driving this session.
func (s *Session) Subcommand() string { return s.spec.Subcommand }

// SessionID is the source session being continued.
func (s *Session) SessionID() string { return s.spec.SessionID }

// CreatedAt is when the session was spawned.
func (s *Session) CreatedAt() time.Time { return s.createdAt }

// Done is closed when the underlying process exits.
func (s *Session) Done() <-chan struct{} { return s.done }

// Exited reports whether the process has ended, and its exit code.
func (s *Session) Exited() (bool, int) {
	if s.doneAt.Load() == 0 {
		return false, 0
	}
	return true, int(s.exitCode.Load())
}

func (s *Session) touch() { s.lastAct.Store(s.now().UnixNano()) }

// Read streams terminal output to the attached client from the replay ring:
// it drains buffered bytes first (so a reconnecting client repaints recent
// output), then tails live output, blocking until more arrives. It returns
// io.EOF once the process has exited and the ring is fully drained, and
// errDetached when the client detaches (Detach closes the subscription). The
// idle clock is refreshed by the pump on output, not here.
func (s *Session) Read(p []byte) (int, error) {
	cancel := s.subCancelCh()
	for {
		n, wait, closed := s.out.read(&s.subOff, p)
		if n > 0 {
			return n, nil
		}
		if closed {
			return 0, io.EOF
		}
		select {
		case <-cancel:
			return 0, errDetached
		case <-wait:
		}
	}
}

// Write feeds client keystrokes to the terminal; it refreshes the idle
// clock.
func (s *Session) Write(p []byte) (int, error) {
	n, err := s.pty.Write(p)
	if n > 0 {
		s.touch()
	}
	return n, err
}

// Resize sets the terminal window size and refreshes the idle clock.
func (s *Session) Resize(rows, cols uint16) error {
	s.touch()
	return s.pty.Resize(rows, cols)
}

func (s *Session) markDone(code int) {
	s.doneOnce.Do(func() {
		s.exitCode.Store(int32(code))
		s.doneAt.Store(s.now().UnixNano())
		close(s.done)
	})
}

// Create validates the spec, spawns the PTY process, registers the
// session, and returns its opaque handle. A Wait goroutine records the exit
// and starts the linger clock; the reaper (or Close) removes it.
func (m *Manager) Create(spec Spec) (string, error) {
	if spec.BinPath == "" || spec.Subcommand == "" || spec.SessionID == "" {
		return "", ErrInvalidSpec
	}

	m.mu.Lock()
	if len(m.sessions) >= m.maxConcurrent {
		m.mu.Unlock()
		return "", ErrTooManySessions
	}
	m.mu.Unlock()

	p, err := m.spawner.Spawn(spec)
	if err != nil {
		return "", err
	}

	handle, err := newToken()
	if err != nil {
		_ = p.Kill()
		return "", err
	}

	s := &Session{
		spec:      spec,
		pty:       p,
		out:       newOutBuf(m.ringBytes),
		createdAt: m.now(),
		now:       m.now,
		done:      make(chan struct{}),
	}
	s.handle = handle
	s.touch()

	m.mu.Lock()
	m.sessions[handle] = s
	m.mu.Unlock()

	m.wg.Add(2)
	go m.waitExit(s)
	go m.pump(s)

	m.logger.Info("termsession: created", "subcommand", spec.Subcommand, "session", spec.SessionID)
	return handle, nil
}

// waitExit blocks on the process and records its exit. The session lingers
// in the registry (ExitLinger) so an attached client sees the final bytes.
func (m *Manager) waitExit(s *Session) {
	defer m.wg.Done()
	code, _ := s.pty.Wait()
	s.markDone(code)
}

// pump is the always-on PTY drainer: it continuously copies PTY output into
// the session's replay ring (refreshing the idle clock) whether or not a
// client is attached, so a clientless PTY never blocks and a reconnecting
// client can replay recent output. It ends when the PTY read errors (process
// exited or Kill closed the master), closing the ring so a caught-up Read
// returns io.EOF.
func (m *Manager) pump(s *Session) {
	defer m.wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.out.write(buf[:n])
			s.touch()
		}
		if err != nil {
			s.out.close()
			return
		}
	}
}

// Attach claims the session for a single client and returns it. The client's
// Read replays the ring from the start (recent scrollback) then tails live.
// The caller MUST call Detach when the attachment ends. A second concurrent
// attach fails with ErrAlreadyAttached.
func (m *Manager) Attach(handle string) (*Session, error) {
	m.mu.Lock()
	s := m.sessions[handle]
	m.mu.Unlock()
	if s == nil {
		return nil, ErrNotFound
	}
	if !s.attached.CompareAndSwap(false, true) {
		return nil, ErrAlreadyAttached
	}
	s.beginAttach()
	return s, nil
}

// Detach releases a client's claim and unblocks its Read (errDetached) WITHOUT
// killing the PTY — the session stays live so the client can reconnect (a
// tab-close/refresh detaches, it does not reap). Cleanup is the idle reaper's
// job (or an explicit Close/DELETE). The attached CAS gates cancelSub so it
// fires exactly once per attachment.
func (m *Manager) Detach(handle string) {
	m.mu.Lock()
	s := m.sessions[handle]
	m.mu.Unlock()
	if s != nil && s.attached.CompareAndSwap(true, false) {
		s.cancelSub()
	}
}

// Resize sets the window size of a live session.
func (m *Manager) Resize(handle string, rows, cols uint16) error {
	m.mu.Lock()
	s := m.sessions[handle]
	m.mu.Unlock()
	if s == nil {
		return ErrNotFound
	}
	return s.Resize(rows, cols)
}

// Close kills a session's process tree and removes it from the registry.
// Idempotent: closing an already-gone handle is a no-op.
func (m *Manager) Close(handle string) {
	m.terminate(handle)
}

// terminate is the single idempotent kill+remove funnel every teardown
// path routes through (Close, reaper, Shutdown).
func (m *Manager) terminate(handle string) {
	m.mu.Lock()
	s := m.sessions[handle]
	delete(m.sessions, handle)
	m.mu.Unlock()
	if s == nil {
		return
	}
	_ = s.pty.Kill()
	s.markDone(-1) // no-op if the process already recorded a real code
}

// Shutdown stops the reaper and kills every live session. Safe to call
// multiple times.
func (m *Manager) Shutdown() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.Lock()
	handles := make([]string, 0, len(m.sessions))
	for h := range m.sessions {
		handles = append(handles, h)
	}
	m.mu.Unlock()
	for _, h := range handles {
		m.terminate(h)
	}
	m.wg.Wait()
}

// Info is a read-only snapshot of a session (Phase 3 session list). ID is
// the opaque session handle.
type Info struct {
	ID         string
	Subcommand string
	SessionID  string
	CreatedAt  time.Time
	Attached   bool
	Exited     bool
	ExitCode   int
}

// Snapshot returns the current live sessions.
func (m *Manager) Snapshot() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		exited, code := s.Exited()
		out = append(out, Info{
			ID:         s.handle,
			Subcommand: s.spec.Subcommand,
			SessionID:  s.spec.SessionID,
			CreatedAt:  s.createdAt,
			Attached:   s.attached.Load(),
			Exited:     exited,
			ExitCode:   code,
		})
	}
	return out
}

func (m *Manager) reapLoop(interval time.Duration) {
	defer m.wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			m.reapOnce(m.now())
		}
	}
}

// reapOnce terminates sessions that are idle past IdleTimeout or exited
// past ExitLinger. Split from the ticker so tests drive it deterministically.
func (m *Manager) reapOnce(now time.Time) {
	m.mu.Lock()
	var kill []string
	for h, s := range m.sessions {
		if da := s.doneAt.Load(); da != 0 {
			if now.Sub(time.Unix(0, da)) > m.exitLinger {
				kill = append(kill, h)
			}
			continue
		}
		if now.Sub(time.Unix(0, s.lastAct.Load())) > m.idleTimeout {
			kill = append(kill, h)
		}
	}
	m.mu.Unlock()
	for _, h := range kill {
		m.logger.Info("termsession: reaped", "session", h[:min(8, len(h))])
		m.terminate(h)
	}
}

// discard is an io.Writer sink for the default no-op logger.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
