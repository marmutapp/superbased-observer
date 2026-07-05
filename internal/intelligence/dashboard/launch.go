package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// Embedded web-terminal launch surface (docs/session-handoff.md launch
// section). Two endpoints, both inheriting the dashboard's loopback +
// browserGuard trust boundary:
//
//   - POST /api/session/<id>/launch mints an opaque session handle after
//     validating the target tool against the launchable capability set. The
//     argv is built SERVER-SIDE by internal/termsession from the validated
//     Spec; the client never supplies argv or paths. This POST is
//     Origin-checked by browserGuard (CSRF), so a malicious page can't mint
//     a handle.
//   - GET /ws/launch/<handle> upgrades to a websocket bridging the PTY to
//     the browser. coder/websocket's Accept rejects cross-origin upgrades by
//     default (Origin host must equal the request Host), which — together
//     with the opaque handle minted only by the Origin-checked POST — is the
//     CSWSH defense. No browserGuard relaxation is made for the GET.
//
// The whole surface is gated by Options.LaunchManager != nil; cmd wires the
// manager only when [handoff].allow_dashboard_launch is true, so a nil seam
// (503) is the honest "disabled" state.

// LaunchManager is the dashboard's seam onto internal/termsession (the
// embedded web-terminal PTY registry). It is DEFINED here, not imported, so
// the dashboard package carries no dependency on termsession's concrete
// types — the same injected-seam discipline as BuildHandoff/DemoSeeder. cmd
// wires a thin adapter that injects the observer BinPath and translates
// types. Dispatch is on capability shape (the LaunchSpec set), never tool
// name.
type LaunchManager interface {
	// Create spawns a PTY-backed launcher and returns its opaque handle.
	Create(spec LaunchSpec) (handle string, err error)
	// Attach claims the session for a single client.
	Attach(handle string) (LaunchSession, error)
	// Detach releases a client's claim.
	Detach(handle string)
	// Resize sets the terminal window size.
	Resize(handle string, rows, cols uint16) error
	// Close reaps the session's process tree and removes it.
	Close(handle string)
	// Snapshot lists live sessions (Phase 3 running-session list).
	Snapshot() []LaunchInfo
}

// LaunchSession is one attached terminal session. *termsession.Session
// satisfies it structurally, so the cmd adapter returns it directly.
type LaunchSession interface {
	io.Reader
	io.Writer
	Resize(rows, cols uint16) error
	Done() <-chan struct{}
	Exited() (bool, int)
}

// LaunchSpec is the dashboard's server-derived launch request. It carries no
// BinPath — the cmd adapter injects os.Executable() so a client can never
// influence which binary runs.
type LaunchSpec struct {
	Subcommand  string
	SessionID   string
	Carry       string
	FromMessage int
	Rows        uint16
	Cols        uint16
}

// LaunchInfo is a running-session snapshot row (Phase 3 list).
type LaunchInfo struct {
	ID         string    `json:"token"`
	Subcommand string    `json:"subcommand"`
	SessionID  string    `json:"session_id"`
	CreatedAt  time.Time `json:"created_at"`
	Attached   bool      `json:"attached"`
	Exited     bool      `json:"exited"`
	ExitCode   int       `json:"exit_code"`
}

// Errors the cmd adapter maps termsession errors onto so the handler can
// pick an honest HTTP status without importing termsession.
var (
	// ErrLaunchTooMany signals the concurrent-session cap was hit (429).
	ErrLaunchTooMany = errors.New("too many concurrent terminal sessions")
	// ErrLaunchUnsupported signals the platform can't host a PTY (501) —
	// a native-Windows daemon (run under WSL instead).
	ErrLaunchUnsupported = errors.New("embedded terminal is not supported on this platform")
	// ErrLaunchAlreadyAttached signals a second client tried to attach.
	ErrLaunchAlreadyAttached = errors.New("session already has an attached client")
)

// validCarryModes is the accepted --carry set; "" defers to the launcher's
// configured default.
var validCarryModes = map[string]bool{
	"":               true,
	"metadata":       true,
	"distilled":      true,
	"distilled_tail": true,
	"full":           true,
}

// launchSubcommand resolves a target tool name to its observer launcher
// verb, or ("", false) when the tool is not launchable in the embedded
// terminal. Branches on the Launch capability shape, never the tool name.
func launchSubcommand(tool string) (string, bool) {
	cap, ok := integration.For(tool)
	if !ok || !cap.Handoff.Launchable() {
		return "", false
	}
	return cap.Handoff.Launch.Subcommand, true
}

type launchRequest struct {
	To          string `json:"to"`
	Carry       string `json:"carry"`
	ForkMessage int    `json:"fork_message"`
}

type launchResponse struct {
	Token      string `json:"token"`
	Subcommand string `json:"subcommand"`
	SessionID  string `json:"session_id"`
}

// handleSessionLaunch serves POST /api/session/<id>/launch. It validates the
// target against the launchable capability set, builds a server-derived
// LaunchSpec, and returns the opaque session handle. The websocket bridge is
// a separate GET route the client opens with the returned handle.
func (s *Server) handleSessionLaunch(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.LaunchManager == nil {
		http.Error(w, "launch unavailable — this dashboard runs without the embedded-terminal launcher (set [handoff].allow_dashboard_launch and run via `observer start`)", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(sessionID) == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	var body launchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sub, ok := launchSubcommand(body.To)
	if !ok {
		http.Error(w, "tool "+body.To+" is not launchable in the embedded terminal", http.StatusBadRequest)
		return
	}
	if !validCarryModes[body.Carry] {
		http.Error(w, "invalid carry mode", http.StatusBadRequest)
		return
	}
	if body.ForkMessage < 0 {
		http.Error(w, "fork_message must be >= 0", http.StatusBadRequest)
		return
	}

	handle, err := s.opts.LaunchManager.Create(LaunchSpec{
		Subcommand:  sub,
		SessionID:   sessionID,
		Carry:       body.Carry,
		FromMessage: body.ForkMessage,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrLaunchTooMany):
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		case errors.Is(err, ErrLaunchUnsupported):
			http.Error(w, err.Error(), http.StatusNotImplemented)
		default:
			http.Error(w, "launch failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, launchResponse{Token: handle, Subcommand: sub, SessionID: sessionID})
}

// handleLaunchAdmin serves the running-session admin routes under
// /api/launch/: GET /api/launch/sessions lists live terminal sessions
// (metadata only — no content), and DELETE /api/launch/<handle> reaps one.
// Gated by the LaunchManager seam; DELETE is Origin-checked by browserGuard.
func (s *Server) handleLaunchAdmin(w http.ResponseWriter, r *http.Request) {
	if s.opts.LaunchManager == nil {
		http.Error(w, "launch unavailable", http.StatusServiceUnavailable)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/launch/")
	if rest == "sessions" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"sessions": s.opts.LaunchManager.Snapshot()})
		return
	}
	// DELETE /api/launch/<handle> — reap one session.
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if rest == "" || strings.Contains(rest, "/") {
		http.NotFound(w, r)
		return
	}
	s.opts.LaunchManager.Close(rest)
	w.WriteHeader(http.StatusNoContent)
}

// wsControl is the text-frame control protocol between the browser and the
// PTY bridge. Terminal I/O rides BINARY frames (keystrokes in, output out);
// TEXT frames are JSON control messages. The client sends {"t":"resize",…};
// the server emits {"t":"exit","code":…} when the process ends.
type wsControl struct {
	T    string `json:"t"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Code int    `json:"code,omitempty"`
}

// handleLaunchWS serves GET /ws/launch/<handle>. It upgrades to a websocket
// (Accept rejects cross-origin by default) and bridges the PTY to the browser
// until either side closes. On disconnect the client DETACHES — the child
// process keeps running (an always-on pump drains its PTY into a replay ring),
// so a tab-close/refresh survives and a reconnecting client replays recent
// output then tails live. The session is reaped only by an explicit "Stop &
// close" (DELETE /api/launch/<handle>) or the idle/exit-linger reaper.
func (s *Server) handleLaunchWS(w http.ResponseWriter, r *http.Request) {
	if s.opts.LaunchManager == nil {
		http.NotFound(w, r)
		return
	}
	handle := strings.TrimPrefix(r.URL.Path, "/ws/launch/")
	if handle == "" || strings.Contains(handle, "/") {
		http.NotFound(w, r)
		return
	}

	// Accept FIRST so a cross-origin upgrade is rejected before any session
	// state is touched. On rejection Accept has already written the response.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20) // allow large pastes; terminal input is otherwise tiny
	defer func() { _ = c.CloseNow() }()

	sess, err := s.opts.LaunchManager.Attach(handle)
	if err != nil {
		reason := "session not found"
		if errors.Is(err, ErrLaunchAlreadyAttached) {
			reason = "session already attached"
		}
		_ = c.Close(websocket.StatusPolicyViolation, reason)
		return
	}
	defer s.opts.LaunchManager.Detach(handle) // detach on disconnect; child survives for reconnect

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// PTY output → client (binary frames). Runs until the PTY closes or a ws
	// write fails, then signals readerDone. It does NOT cancel on PTY EOF —
	// that ownership belongs to the exit notifier below, so the exit control
	// frame is emitted AFTER all output has flushed (otherwise a cancel here
	// races the notifier's write and the client only sees a raw socket EOF).
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := sess.Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// When the process exits AND its output is fully flushed, emit the exit
	// control frame and unblock the read loop. On a client-side disconnect
	// (ctx already cancelled) it returns without writing.
	go func() {
		select {
		case <-sess.Done():
		case <-ctx.Done():
			return
		}
		<-readerDone // let the reader flush the final bytes first
		_, code := sess.Exited()
		_ = wsjson.Write(ctx, c, wsControl{T: "exit", Code: code})
		cancel()
	}()

	// Client → PTY (main loop). Binary = keystrokes; text = control frames.
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			break
		}
		if typ == websocket.MessageText {
			var ctrl wsControl
			if err := json.Unmarshal(data, &ctrl); err == nil && ctrl.T == "resize" {
				_ = sess.Resize(ctrl.Rows, ctrl.Cols)
			}
			continue
		}
		if _, werr := sess.Write(data); werr != nil {
			break
		}
	}
	cancel()
}
