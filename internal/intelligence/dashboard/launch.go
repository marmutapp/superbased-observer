package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
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
	// CreateFresh spawns a FRESH agent (no --continue-from) after the
	// application service validates the operator's fresh-launch opt-in
	// ([terminal.launch]: allow_fresh_agent + allowed_tools +
	// allowed_project_roots). It returns the opaque handle. F1.
	CreateFresh(spec FreshLaunchSpec) (handle string, err error)
	// CreateSetup spawns a fixed, server-derived local operator SETUP command
	// (SetupSpec.Argv — e.g. the one-time Tailscale operator grant) in a PTY,
	// bypassing the AI-launch policy / terminal_run identity / OOB channel. The
	// resulting session is LOCAL-WRITER-ONLY (a remote principal can never
	// acquire its writer lease). It returns the opaque handle. The caller MUST
	// build Argv from a server-side helper, never from request input.
	CreateSetup(spec SetupSpec) (handle string, err error)
	// Subscribe registers a read-only viewer (§4.α.1 output fan-out). Every
	// attach — local or remote — starts as a viewer; input requires a separate
	// writer lease. The caller MUST Unsubscribe. This is the OWNER-LOCAL path.
	Subscribe(handle string) (LaunchSubscription, error)
	// SubscribeRemote is the REMOTE-principal viewer path. It refuses a SpecSetup
	// (privileged, local-only) session — whose output can echo a typed sudo
	// password / login URL — mirroring AcquireWriterRemote's write-side pin at
	// the READ seam; every other session subscribes normally. Used by the
	// remote-exposed /ws/launch bridge so a paired remote device can never read a
	// setup PTY.
	SubscribeRemote(handle string) (LaunchSubscription, error)
	// IsSetupSession reports whether a handle is a SpecSetup (privileged,
	// local-only) session. The dashboard redacts these from remotely-visible
	// snapshots and refuses their remote termination. Unknown handle ⇒ false.
	IsSetupSession(handle string) bool
	// Unsubscribe releases a viewer's subscription (child survives for
	// reconnect).
	Unsubscribe(sub LaunchSubscription)
	// AcquireWriterLocal grants the owner-local exclusive writer lease — the
	// loopback path (CapabilityLocal provenance), never refused, always takes
	// over an incumbent remote writer (§4.α.2a/§4.α.3).
	AcquireWriterLocal(handle string) (LaunchWriter, error)
	// AcquireWriterRemote grants a remote writer lease after the cmd adapter
	// runs the single §4.δ authorization conjunction over the request-derived
	// inputs and mints the unforgeable grant. The dashboard passes plain
	// strings; the grant type never crosses this seam.
	AcquireWriterRemote(req RemoteWriterRequest) (LaunchWriter, error)
	// Close reaps the session's process tree and removes it.
	Close(handle string)
	// Snapshot lists live sessions (Phase 3 running-session list).
	Snapshot() []LaunchInfo
	// RevokeAllRemoteWriters revokes every live session's REMOTE-held writer
	// lease (leaving any owner-LOCAL loopback writer untouched) through the ONE
	// termsession revocation funnel — the manager-level global kill the admin
	// transitions (remote disable / rotate / allow_terminal→false) drive so a
	// live remote writer is actually terminated, not merely blocked for future
	// acquires. Returns the count revoked. The cmd adapter delegates to
	// termsession.Manager.RevokeAllRemoteWriters; the dashboard never imports it.
	RevokeAllRemoteWriters(reason string) int
	// RevokeRemoteWriterByHolder revokes the REMOTE writer lease held by a
	// specific device fingerprint (the unified sha256(device-session)[:8] the
	// /api/remote/sessions list surfaces), if any, through the same funnel — the
	// single-device kill a device-session revoke drives. The owner-LOCAL writer
	// is never matched. Returns whether a lease was revoked.
	RevokeRemoteWriterByHolder(fingerprint, reason string) bool
}

// LaunchSubscription is one read-only viewer of a terminal session. It streams
// PTY output (replay then live tail); its input never reaches the PTY.
// *termsession.Subscription satisfies it structurally.
type LaunchSubscription interface {
	io.Reader
	Done() <-chan struct{}
	Exited() (bool, int)
	// Lost is the bytes this viewer missed to the drop-oldest ring (a gap).
	Lost() int64
}

// LaunchWriter is the exclusive right to drive a session's PTY. Its Write /
// Resize reach the PTY only while the lease is live; a revoked/taken-over lease
// returns an error (the server-side input-frame drop, §4.β).
// *termsession.WriterLease satisfies it structurally.
type LaunchWriter interface {
	io.Writer
	Resize(rows, cols uint16) error
	// Revoked is closed when the lease is revoked/taken-over/expired so the WS
	// bridge can tell the (remote) writer it lost control.
	Revoked() <-chan struct{}
	// Release voluntarily yields the lease (local yield / disconnect).
	Release()
	// Holder is the lease holder identity ("local" or a device fingerprint).
	Holder() string
	// RevokeIsTakeover reports (meaningfully only AFTER Revoked() has closed)
	// whether the lease ended because a LOCAL takeover superseded it. On the
	// remote bridge a takeover only DEMOTES the client to a viewer (the device
	// session is still valid); any other termination — an admin/device revoke,
	// an expiry, or teardown — CLOSES the socket. *termsession.WriterLease
	// satisfies it structurally.
	RevokeIsTakeover() bool
}

// RemoteWriterRequest carries the request-derived inputs for a remote writer
// acquire. The cmd adapter runs termlease.Authorize over these (device session +
// allow_terminal + launch policy + single-use capability + bound confirm) and
// mints the unforgeable grant; the dashboard never sees the grant type.
type RemoteWriterRequest struct {
	Handle          string
	DeviceSessionID string
	CapabilityToken string
	Confirm         string
	// RemoteExposed marks that the request arrived on a remotely-exposed
	// execute-classified route (listener provenance, §4.4).
	RemoteExposed bool
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

// FreshLaunchSpec is the dashboard's server-derived FRESH-launch request (F1).
// Tool is the client-chosen tool NAME (validated against the operator's
// allow-list by the application service); Subcommand is resolved server-side
// from the capability registry; ProjectRoot is the only client-influenced path
// and is canonicalized + allow-list-checked before spawn. No BinPath, no argv.
type FreshLaunchSpec struct {
	Tool        string
	Subcommand  string
	ProjectRoot string
	Rows        uint16
	Cols        uint16
}

// SetupSpec is a server-derived local operator setup command run in a PTY (the
// dashboard-tailnet-guided-setup plan §B). Argv is the complete argv built by
// the handler from a fixed helper (tailnet.OperatorGrantArgv) — NEVER request
// input; Label is a short human tag for logs. The spawned session is
// local-writer-only, launches no agent, and mints no terminal_run.
type SetupSpec struct {
	Argv  []string
	Label string
	Rows  uint16
	Cols  uint16
}

// LaunchInfo is a running-session snapshot row (Phase 3 list).
type LaunchInfo struct {
	ID         string `json:"token"`
	Subcommand string `json:"subcommand"`
	SessionID  string `json:"session_id"`
	// RunID is the durable terminal_run identity this PTY handle belongs to
	// (F1); empty when the session predates the run-identity wiring.
	RunID     string    `json:"run_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Attached is retained for wire compatibility: true when at least one viewer
	// is subscribed. Viewers is the exact read-only subscriber count and
	// WriterHolder identifies the controlling writer ("local" or a remote device
	// fingerprint, empty when none) — the §4.α controller display.
	Attached     bool   `json:"attached"`
	Viewers      int    `json:"viewers"`
	WriterHolder string `json:"writer_holder,omitempty"`
	// Setup marks a SpecSetup (privileged, local-only) session. It is redacted
	// from snapshots served to REMOTE callers (visibleSnapshot), so a remote
	// device never even learns the handle exists.
	Setup    bool `json:"setup,omitempty"`
	Exited   bool `json:"exited"`
	ExitCode int  `json:"exit_code"`
}

// Errors the cmd adapter maps termsession errors onto so the handler can
// pick an honest HTTP status without importing termsession.
var (
	// ErrLaunchTooMany signals the concurrent-session cap was hit (429).
	ErrLaunchTooMany = errors.New("too many concurrent terminal sessions")
	// ErrLaunchUnsupported signals the platform can't host a PTY (501) —
	// a native-Windows daemon (run under WSL instead).
	ErrLaunchUnsupported = errors.New("embedded terminal is not supported on this platform")
	// ErrLaunchAlreadyAttached signals a second client tried to attach. Retained
	// for callers that still map it; the output fan-out (§4.α) no longer rejects
	// a second viewer.
	ErrLaunchAlreadyAttached = errors.New("session already has an attached client")
	// ErrLaunchExecuteUnavailable signals the remote writer-lease path is not
	// wired (the §4.γ/§4.δ authorizer is nil) — a fail-closed 503-shaped state.
	ErrLaunchExecuteUnavailable = errors.New("remote terminal writer is not configured")
	// ErrLaunchFreshDisabled signals [terminal.launch].allow_fresh_agent is
	// off (403). Fresh-agent launch is a conscious opt-in, never migrated on.
	ErrLaunchFreshDisabled = errors.New("fresh-agent launch is disabled (set [terminal.launch].allow_fresh_agent)")
	// ErrLaunchToolNotAllowed signals the tool is not in the operator's
	// [terminal.launch].allowed_tools allow-list (403).
	ErrLaunchToolNotAllowed = errors.New("tool is not in the fresh-launch allow-list")
	// ErrLaunchProjectRootDenied signals the requested project_root failed the
	// allow-list / canonicalization check (400).
	ErrLaunchProjectRootDenied = errors.New("project root not permitted")
	// ErrLaunchSetupInFlight signals a setup session of the same kind
	// (operator-grant / login / install) is already starting — the setup
	// single-flight refusal (409). Prevents a POST-spam from spawning many
	// privileged PTYs.
	ErrLaunchSetupInFlight = errors.New("a setup session of this kind is already starting")
)

// visibleSnapshot returns the live launch-session list filtered for the request
// principal: on the owner-trusted loopback listener (not remote-exposed) it is
// the full snapshot; for a REMOTE-exposed caller every SpecSetup handle is
// redacted so a paired remote device cannot even see that a privileged setup PTY
// exists (defence in depth over the SubscribeRemote refusal). Branches on the
// boundary-resolved local-vs-remote signal (remoteExposedFromContext), never on
// tool/route name.
func (s *Server) visibleSnapshot(ctx context.Context) []LaunchInfo {
	all := s.opts.LaunchManager.Snapshot()
	if !remoteExposedFromContext(ctx) {
		return all
	}
	out := make([]LaunchInfo, 0, len(all))
	for _, info := range all {
		if info.Setup {
			continue // redact privileged setup handles from remote callers
		}
		out = append(out, info)
	}
	return out
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

// launchableTools returns every tool NAME launchable in the embedded terminal,
// sorted, resolved from the capability registry (dispatch on capability shape,
// never tool name). The fresh-launch dialog uses it to populate its picker
// honestly; the operator's [terminal.launch].allowed_tools allow-list still
// governs which of these actually launch (enforced server-side).
func launchableTools() []string {
	var out []string
	for _, c := range integration.Capabilities() {
		if c.Handoff.Launchable() {
			out = append(out, c.Tool)
		}
	}
	sort.Strings(out)
	return out
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
	// "" defers to the launcher's configured default; otherwise defer to
	// handoff.ValidCarry (the single owner of the carry vocabulary) so a new
	// mode never needs a second allow-list here.
	if body.Carry != "" && !handoff.ValidCarry(handoff.CarryMode(body.Carry)) {
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

// terminalLaunchRequest is the POST /api/terminal/launch body (F1). The only
// client inputs are the tool NAME (allow-listed server-side) and an optional
// project_root (canonicalized + allow-list-checked server-side). No argv, no
// session id (fresh launch), no BinPath.
type terminalLaunchRequest struct {
	Tool        string `json:"tool"`
	ProjectRoot string `json:"project_root"`
}

// terminalLaunchResponse mirrors launchResponse plus the tool label so the
// client can render the terminal header without re-deriving it.
type terminalLaunchResponse struct {
	Token      string `json:"token"`
	Tool       string `json:"tool"`
	Subcommand string `json:"subcommand"`
}

// handleTerminalLaunch serves POST /api/terminal/launch — the F1 fresh-agent
// launch. It is classified EXECUTE in the route-capability registry (§9): a
// fresh launch starts a new process, the privilege-expansion feature of this
// plan. The tool is resolved to its launcher subcommand server-side from the
// capability registry; the application service enforces the operator's
// [terminal.launch] opt-in (allow_fresh_agent + allowed_tools +
// allowed_project_roots) and canonicalizes project_root before spawn.
func (s *Server) handleTerminalLaunch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.LaunchManager == nil {
		http.Error(w, "launch unavailable — this dashboard runs without the embedded-terminal launcher (run via `observer start`)", http.StatusServiceUnavailable)
		return
	}
	var body terminalLaunchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	sub, ok := launchSubcommand(body.Tool)
	if !ok {
		http.Error(w, "tool "+body.Tool+" is not launchable in the embedded terminal", http.StatusBadRequest)
		return
	}
	handle, err := s.opts.LaunchManager.CreateFresh(FreshLaunchSpec{
		Tool:        body.Tool,
		Subcommand:  sub,
		ProjectRoot: body.ProjectRoot,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrLaunchFreshDisabled):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrLaunchToolNotAllowed):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrLaunchProjectRootDenied):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, ErrLaunchTooMany):
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		case errors.Is(err, ErrLaunchUnsupported):
			http.Error(w, err.Error(), http.StatusNotImplemented)
		default:
			http.Error(w, "launch failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, terminalLaunchResponse{Token: handle, Tool: body.Tool, Subcommand: sub})
}

// handleTerminalSessions serves GET /api/terminal/sessions — the live
// terminal-session list (metadata only, no content). Classified VIEW (§9).
// It generalizes /api/launch/sessions under the terminal route namespace.
func (s *Server) handleTerminalSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.LaunchManager == nil {
		http.Error(w, "launch unavailable", http.StatusServiceUnavailable)
		return
	}
	// Surface the operator's canonicalized [terminal.launch].allowed_project_roots
	// so the "New terminal" dialog can honestly mark which known-project roots a
	// fresh launch will actually accept (empty = only the launcher's own default
	// cwd is permitted). The SPA reuses these canonical strings verbatim — it never
	// re-canonicalizes a path — so its permitted/not-permitted marking matches the
	// server's spawn-time ValidateProjectRoot check.
	var allowedRoots []string
	if cfg, err := loadConfigForDashboard(s.opts.ConfigPath); err == nil {
		allowedRoots = resolveAllowedProjectRoots(cfg.Terminal.Launch.AllowedProjectRoots)
	}
	writeJSON(w, map[string]any{
		"sessions":              s.visibleSnapshot(r.Context()),
		"launchable_tools":      launchableTools(),
		"allowed_project_roots": allowedRoots,
	})
}

// resolveAllowedProjectRoots canonicalizes the configured
// [terminal.launch].allowed_project_roots through the SAME internal/termsvc
// validator the spawn path re-runs (real filesystem identity, symlinks
// resolved, UNC/relative rejected). A stale or now-invalid entry is skipped
// rather than failing the whole list, so the read surface degrades gracefully.
// The returned slice holds the exact canonical strings the server matches a
// requested project_root against — the SPA marks known roots permitted/not by
// comparing against these, never by re-canonicalizing client-side. Returns a
// non-nil empty slice when nothing is allow-listed (deny-all: only the default
// cwd is permitted) so the JSON field is always a real array.
func resolveAllowedProjectRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, raw := range roots {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		canonical, err := termsvc.ValidateProjectRoot(entry, []string{entry})
		if err != nil {
			continue
		}
		out = append(out, canonical)
	}
	return out
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
		writeJSON(w, map[string]any{"sessions": s.visibleSnapshot(r.Context())})
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
	// A privileged SpecSetup PTY (sudo login / install / operator-grant) is
	// LOCAL-ONLY for its whole lifecycle: a remote-exposed principal may not
	// terminate it either. Return 404 so the remote caller cannot even confirm
	// the handle exists (same confidentiality posture as the snapshot redaction
	// + SubscribeRemote refusal). The owner-local loopback listener is never
	// remote-exposed, so the local owner's Cancel still reaps it.
	if remoteExposedFromContext(r.Context()) && s.opts.LaunchManager.IsSetupSession(rest) {
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
	// Cap + Confirm ride the remote writer-acquire control frame
	// ({"t":"acquire-writer","cap":…,"confirm":…}) — the single-use terminal-
	// control capability + its bound confirm (§4.γ). They are accepted ONLY in
	// this TEXT frame body over the already-Origin-checked, cookie-authenticated
	// websocket, never a URL/subprotocol/query (§8.1 #5).
	Cap     string `json:"cap,omitempty"`
	Confirm string `json:"confirm,omitempty"`
}

// handleLaunchWS serves GET /ws/launch/<handle> — the owner-local terminal
// bridge on the direct loopback listener. It upgrades to a websocket (Accept
// rejects cross-origin by default), SUBSCRIBES a read-only output viewer, and
// acquires the owner-local writer lease (§4.α: this loopback path is
// CapabilityLocal provenance, so it takes the writer without a grant and can
// never be refused — a second local tab takes over). On disconnect the viewer
// unsubscribes and the writer lease is released; the child process keeps running
// (an always-on pump drains its PTY into a replay ring), so a tab-close/refresh
// survives and a reconnecting client replays recent output then tails live.
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

	// Listener provenance (§4.4), resolved at the boundary — NOT from any client
	// field. A remote-exposed request subscribes through SubscribeRemote, which
	// REFUSES a SpecSetup (privileged, local-only) session: its output can echo a
	// typed sudo password / login URL, so a paired remote principal must not even
	// READ it. The owner-trusted loopback path uses Subscribe (setup PTYs are
	// driven and watched locally, so the in-dashboard xterm embed still works).
	remoteExposed := remoteExposedFromContext(r.Context())
	var sub LaunchSubscription
	if remoteExposed {
		sub, err = s.opts.LaunchManager.SubscribeRemote(handle)
	} else {
		sub, err = s.opts.LaunchManager.Subscribe(handle)
	}
	if err != nil {
		// A setup-session refusal and a genuine not-found are both closed the
		// same way so a remote caller cannot distinguish "refused" from "absent".
		_ = c.Close(websocket.StatusPolicyViolation, "session not found")
		return
	}
	defer s.opts.LaunchManager.Unsubscribe(sub)

	// A remote-exposed request may NEVER take the owner-local writer (that would
	// hand a remote principal an ungated PTY); it starts read-only and must
	// acquire a writer through the §4.δ conjunction (AcquireWriterRemote) via a
	// writer-acquire control frame. The owner-trusted loopback path keeps the
	// CapabilityLocal writer (never refused).
	if remoteExposed {
		device := sessionCookie(r)
		deviceFP := deviceFingerprint(device)
		peer := hostnameOnly(r.RemoteAddr)
		// Writer-acquire lifecycle audit (plan §8.1), metadata only. The single-
		// use capability + confirm are NEVER recorded — only the request, the
		// coarse outcome, and the device/handle correlation. termlease.Authorize
		// is atomic (claim+confirm+consume in one call), so the claim and confirm
		// legs are not separately observable without leaking secret-adjacent
		// detail; they are collapsed into request + a single coarse denial on any
		// failed leg (never which leg) + consume on success.
		acquire := func(capTok, confirm string) (LaunchWriter, error) {
			s.auditTerminalControl("terminal_control_request", deviceFP, handle, peer, "request", "acquire_writer")
			wl, err := s.opts.LaunchManager.AcquireWriterRemote(RemoteWriterRequest{
				Handle:          handle,
				DeviceSessionID: device,
				CapabilityToken: capTok,
				Confirm:         confirm,
				RemoteExposed:   true, // provenance, resolved at the boundary
			})
			if err != nil || wl == nil {
				// Coarse denial: never which leg failed, never the secret (§8.1 #6).
				s.auditTerminalControl("terminal_control_denied", deviceFP, handle, peer, "deny", "authorize_rejected")
				return wl, err
			}
			s.auditTerminalControl("terminal_control_capability_consume", deviceFP, handle, peer, "allow", "")
			return wl, err
		}
		// Denied-frame coalescer: a viewer with no writer lease flooding forged
		// input/control frames (§4.β drop) must not amplify the audit log — its
		// drops are batched into bounded rows carrying a coalesced count.
		denied := s.newDeniedFrameCoalescer(deviceFP, handle, peer)
		// closeOnHardRevoke=true: on the REMOTE bridge an admin/device revoke of
		// the writer lease closes the socket (the device is no longer trusted); a
		// local takeover only demotes. The owner-local bridge below passes false
		// so its revoke behaviour stays byte-identical (always demote).
		s.bridgeTerminalWS(r.Context(), c, sub, nil, acquire, denied, true)
		return
	}

	// Owner-local writer lease (loopback path). If the session vanished between
	// Subscribe and here we degrade to a read-only viewer rather than failing.
	writer, werr := s.opts.LaunchManager.AcquireWriterLocal(handle)
	if werr == nil {
		defer writer.Release()
	}

	// The owner-local loopback path has no forged-viewer amplification surface
	// (it holds the writer), so no denied-frame coalescer is wired.
	// closeOnHardRevoke=false: the local loopback path always DEMOTES on a
	// revoke (byte-identical to prior behaviour) — the socket-closing branch is
	// remote-only.
	s.bridgeTerminalWS(r.Context(), c, sub, writer, nil, nil, false)
}

// terminalPingInterval / terminalPingTimeout drive the WS liveness probe. Every
// interval the bridge pings the peer and waits up to the timeout for the pong
// (coder/websocket auto-replies to the client's pings and reads the pong on the
// main read loop). This detects a dead / half-open TCP peer — NOT an idle one:
// a terminal viewer legitimately sends nothing for long stretches while
// watching, so we never impose a read-idle deadline (that would evict a
// legitimately-idle watcher). Applies to BOTH the local and remote bridge —
// liveness is a transport property, not a security branch.
// terminalPingIntervalNs / terminalPingTimeoutNs hold the ping cadence in
// nanoseconds. They are atomics (not plain consts) solely so a test can lower
// the cadence race-free while a bridge goroutine reads it concurrently; the
// production defaults (set in init) are unchanged.
var (
	terminalPingIntervalNs atomic.Int64
	terminalPingTimeoutNs  atomic.Int64
)

func init() {
	terminalPingIntervalNs.Store(int64(30 * time.Second))
	terminalPingTimeoutNs.Store(int64(10 * time.Second))
}

// bridgeTerminalWS bridges a terminal Subscription (output) and an optional
// WriterLease (input) to a websocket. A nil writer is a read-only viewer: its
// keystroke + resize frames are DROPPED (never forwarded) — the client-side
// half of the §4.β server-side drop (the manager refuses a write without a live
// lease regardless). When the writer's lease is revoked mid-session (local
// takeover / allow_terminal→false / device revoke), a control frame tells the
// client it lost control and input stops flowing.
//
// acquire, when non-nil (the remote-exposed path), lets the client request the
// writer role mid-session via an {"t":"acquire-writer",…} control frame; it runs
// the §4.δ conjunction (AcquireWriterRemote). Until it succeeds — and if it never
// does — the client is a pure viewer whose frames never reach the PTY. A writer
// acquired here is Release()d when the bridge exits.
//
// closeOnHardRevoke distinguishes the two revoke outcomes (§4.α): when true (the
// REMOTE bridge), a writer-lease revocation that is NOT a local takeover — an
// admin disable/rotate/device-revoke/allow_terminal→false, i.e. the device is no
// longer trusted — CLOSES the socket; a local takeover only demotes the client to
// a read-only viewer. When false (the owner-local bridge) every revoke merely
// demotes, keeping that path byte-identical. A normal PTY-exit teardown never
// reaches this branch: the exit notifier cancels the bridge context first, so
// watchRevoke returns via ctx.Done rather than the revoked channel.
func (s *Server) bridgeTerminalWS(parent context.Context, c *websocket.Conn, sub LaunchSubscription, writer LaunchWriter, acquire func(capTok, confirm string) (LaunchWriter, error), denied *deniedFrameCoalescer, closeOnHardRevoke bool) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Flush the coalesced tail of dropped-frame drops on teardown so a viewer
	// that floods then disconnects still yields a bounded final count (nil-safe:
	// the owner-local path wires no coalescer).
	defer denied.flush()

	// A writer acquired remotely inside this bridge is owned here — release it on
	// exit (the owner-local loopback writer is released by handleLaunchWS's defer).
	var acquired LaunchWriter
	defer func() {
		if acquired != nil {
			acquired.Release()
		}
	}()

	// watchRevoke tells the client it lost control when a writer's lease is
	// revoked (local takeover / allow_terminal→false / device revoke). Started
	// once per writer (the initial loopback writer, and again after a remote
	// acquire installs a new one).
	watchRevoke := func(wl LaunchWriter) {
		go func() {
			select {
			case <-wl.Revoked():
				// Always tell the client it lost control. On the remote bridge, a
				// revocation that is NOT a local takeover means the device is no
				// longer trusted (admin disable/rotate/device-revoke/allow_terminal
				// →false) — cancel the bridge so the deferred CloseNow tears the
				// socket down. A local takeover (device still valid) only demotes.
				_ = wsjson.Write(ctx, c, wsControl{T: "control_revoked"})
				if closeOnHardRevoke && !wl.RevokeIsTakeover() {
					cancel()
				}
			case <-ctx.Done():
			}
		}()
	}

	// PTY output → client (binary frames). Runs until the PTY closes or a ws
	// write fails, then signals readerDone. It does NOT cancel on PTY EOF —
	// that ownership belongs to the exit notifier below, so the exit control
	// frame is emitted AFTER all output has flushed.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := sub.Read(buf)
			if n > 0 {
				if wErr := c.Write(ctx, websocket.MessageBinary, buf[:n]); wErr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// When the process exits AND its output is fully flushed, emit the exit
	// control frame and unblock the read loop.
	go func() {
		select {
		case <-sub.Done():
		case <-ctx.Done():
			return
		}
		<-readerDone // let the reader flush the final bytes first
		_, code := sub.Exited()
		_ = wsjson.Write(ctx, c, wsControl{T: "exit", Code: code})
		cancel()
	}()

	// Liveness probe: ping the peer periodically and cancel the bridge if a ping
	// fails (dead / half-open connection). This is NOT a read-idle timeout — a
	// viewer that sends nothing for a long time while watching stays connected;
	// only an unresponsive TRANSPORT tears down. Runs identically on the local and
	// remote bridge.
	go pingLoop(ctx, c, cancel)

	// Writer-lease revocation → tell the client it lost control (§4.α.3). Input
	// writes error out on their own once the lease is gone; this surfaces it.
	if writer != nil {
		watchRevoke(writer)
	}

	// Client → PTY (main loop). Binary = keystrokes; text = control frames.
	// Without a live writer lease every input/control frame is DROPPED at this
	// boundary (§4.β): a viewer's forged frames never reach Write/Resize. The
	// ONLY frame that can establish a writer is the acquire-writer control frame,
	// and only through the §4.δ conjunction (acquire) — never a client "oob" or
	// any other forged frame type.
	for {
		typ, data, rerr := c.Read(ctx)
		if rerr != nil {
			break
		}
		if typ == websocket.MessageText {
			var ctrl wsControl
			if err := json.Unmarshal(data, &ctrl); err != nil {
				if writer == nil {
					denied.note() // malformed control frame from a lease-less viewer
				}
				continue
			}
			switch {
			case ctrl.T == "acquire-writer" && acquire != nil && writer == nil:
				wl, aerr := acquire(ctrl.Cap, ctrl.Confirm)
				if aerr != nil || wl == nil {
					_ = wsjson.Write(ctx, c, wsControl{T: "control_denied"})
					continue
				}
				writer = wl
				acquired = wl
				watchRevoke(wl)
				_ = wsjson.Write(ctx, c, wsControl{T: "control_granted"})
			case ctrl.T == "resize" && writer != nil:
				_ = writer.Resize(ctrl.Rows, ctrl.Cols)
			default:
				if writer == nil {
					denied.note() // forged/dropped control frame with no live lease (§4.β)
				}
			}
			continue
		}
		if writer == nil {
			denied.note() // viewer keystroke dropped server-side (§4.β)
			continue
		}
		if _, wErr := writer.Write(data); wErr != nil {
			// Lease lost (revoked/taken over/expired) or PTY gone: stop driving
			// and degrade to a viewer, still watching output until the socket or
			// the exit notifier closes the bridge. A remote client may re-acquire.
			writer = nil
			continue
		}
	}
	cancel()
}

// pingLoop probes the websocket peer every terminalPingInterval and cancels the
// bridge (via cancel) if a ping fails or times out — detecting a dead/half-open
// connection without evicting a legitimately-idle-but-alive viewer. It relies on
// the bridge's concurrent main read loop to read the pong (coder/websocket's
// Ping requires a concurrent Reader). Returns when the bridge context is done.
func pingLoop(ctx context.Context, c *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(time.Duration(terminalPingIntervalNs.Load()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, pcancel := context.WithTimeout(ctx, time.Duration(terminalPingTimeoutNs.Load()))
			err := c.Ping(pctx)
			pcancel()
			if err != nil {
				cancel() // dead peer: tear the socket down
				return
			}
		}
	}
}

// auditTerminalControl emits one metadata-only execute-tier control event
// through the injected RemoteAudit sink (never a secret). device is the short
// device fingerprint (hash[:8]); handle is the opaque terminal handle carried in
// the route column so every terminal event correlates on it. Nil sink (the
// default) is a no-op. detail is a coarse, non-sensitive descriptor.
func (s *Server) auditTerminalControl(kind, device, handle, peer, decision, detail string) {
	if s.opts.RemoteAudit == nil {
		return
	}
	s.opts.RemoteAudit(RemoteAuditRecord{
		Kind:       kind,
		SessionID:  device,
		Principal:  "execute",
		RemoteAddr: peer,
		Route:      handle,
		Decision:   decision,
		Detail:     detail,
	})
}

// deniedFrameBatch bounds how many coalesced drops accumulate before a
// terminal_denied_frame row is emitted mid-flood (in addition to the first-drop
// row and the teardown flush), so a long-lived flooding viewer still surfaces
// periodically while the row count stays ~count/batch — never one row per frame.
const deniedFrameBatch = 128

// deniedFrameCoalescer bounds terminal_denied_frame audit rows so a viewer with
// no writer lease flooding forged input/control frames (§4.β) cannot amplify the
// audit log (plan §8.1 #): it emits the FIRST drop immediately (count 1),
// batches subsequent drops into one row every deniedFrameBatch, and flushes the
// remainder once at bridge teardown. The coalesced count rides the row detail;
// the counts across all rows sum to the exact number of dropped frames. It is
// touched only from the single WS read-loop goroutine (note) and that
// goroutine's deferred teardown (flush), so no lock is needed; all methods are
// nil-safe (the owner-local path wires no coalescer).
type deniedFrameCoalescer struct {
	emit    func(count int)
	pending int
	rows    int
}

// newDeniedFrameCoalescer builds a coalescer whose rows carry the (device,
// handle, peer) correlation. Nil audit sink still returns a live coalescer (its
// emit is a no-op) so the drop-counting path is uniform.
func (s *Server) newDeniedFrameCoalescer(device, handle, peer string) *deniedFrameCoalescer {
	return &deniedFrameCoalescer{emit: func(count int) {
		if s.opts.RemoteAudit == nil {
			return
		}
		s.opts.RemoteAudit(RemoteAuditRecord{
			Kind:       "terminal_denied_frame",
			SessionID:  device,
			Principal:  "view",
			RemoteAddr: peer,
			Route:      handle,
			Decision:   "deny",
			Detail:     "dropped=" + strconv.Itoa(count),
		})
	}}
}

// note records one dropped frame, emitting a bounded audit row on the first drop
// and once per deniedFrameBatch thereafter.
func (d *deniedFrameCoalescer) note() {
	if d == nil {
		return
	}
	d.pending++
	if d.rows == 0 || d.pending >= deniedFrameBatch {
		n := d.pending
		d.pending = 0
		d.rows++
		d.emit(n)
	}
}

// flush emits the coalesced remainder (if any) — called once at bridge teardown.
func (d *deniedFrameCoalescer) flush() {
	if d == nil || d.pending == 0 {
		return
	}
	n := d.pending
	d.pending = 0
	d.rows++
	d.emit(n)
}
