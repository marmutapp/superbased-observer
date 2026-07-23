package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termrun"
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
	// CreateResume spawns a NATIVE resume of a CLOSED session (session-attach
	// design Phase 3): a fresh dashboard-owned PTY running the tool's own resume
	// mechanism (claude `--resume <id>`, codex `resume <id>`) so the real prior
	// transcript reopens — NOT a distilled fork. The application service enforces
	// the SAME fresh-launch opt-in as CreateFresh (a dashboard-initiated Execute
	// respects the allow-lists, unlike the owner-only CLI attach socket). It
	// returns the opaque handle AND the durable run id so the response carries
	// both. P3.
	CreateResume(spec ResumeLaunchSpec) (handle string, runID string, err error)
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
	// IsRemoteSensitiveSession reports whether a handle is a remote-VIEW
	// sensitive run: an external `observer <tool> --attach` session (KindAttach)
	// OR a native resume of a real closed transcript (KindResume) — both bind a
	// daemon-owned PTY to a REAL external transcript whose TUI can echo
	// secrets/customer data. Whether the dashboard exposes such a handle to a
	// remote-exposed caller (its snapshot row via visibleSnapshot and its
	// websocket subscription via handleLaunchWS) is governed by the
	// [remote].allow_terminal_view toggle, which now DEFAULTS TRUE (§3.2) — set
	// it false to restore the deny-read posture. Branches on run KIND (the shared
	// termrun.IsRemoteSensitiveKind table), never a tool name. Unknown handle ⇒
	// false.
	IsRemoteSensitiveSession(handle string) bool
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
	// RevokeRemoteWriterByHolder revokes the REMOTE writer lease whose holder key
	// equals the passed key — the FULL device-session hash (deviceSessionKey /
	// SessionInfo.ID / grant.HolderKey()), NOT the 8-char display fingerprint —
	// if any, through the same funnel. It is the single-device kill a device-
	// session revoke drives; the caller resolves the device to its full session
	// hash first, so the match hits exactly one lease with no 8-char-prefix
	// over-revoke. The owner-LOCAL writer is never matched. Returns whether a
	// lease was revoked.
	RevokeRemoteWriterByHolder(holderKey, reason string) bool
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
	// whether the lease ended because another granted writer superseded it. On the
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

// ResumeLaunchSpec is the dashboard's server-derived NATIVE-resume request
// (P3). Tool is the client-chosen tool NAME (validated against the fresh-launch
// allow-list by the application service); Subcommand is resolved server-side
// from the capability registry; SessionID is the closed session being resumed;
// ExtraArgs is the resume tail composed server-side via integration.ResumeArgs
// (uniformly `--resume <id>`); ProjectRoot is canonicalized + allow-list-checked
// before spawn. No BinPath, no raw argv.
type ResumeLaunchSpec struct {
	Tool        string
	Subcommand  string
	SessionID   string
	ProjectRoot string
	ExtraArgs   []string
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
	// Kind is the terminal_run kind this PTY handle belongs to ("attach" /
	// "handoff" / "fresh"), resolved from the run identity (session-attach
	// design Phase 2). Empty when the session predates run-identity wiring. It
	// drives the dashboard's "Jump in" gating: only Kind=="attach" sessions are
	// daemon-owned externals a dashboard tab can join (design §4 — the sole
	// class with exact daemon-owned liveness).
	Kind string `json:"kind,omitempty"`
	// Tool is the target tool NAME (e.g. "claude-code") the run launched, as
	// distinct from Subcommand (the observer launcher verb). Empty when unknown.
	Tool string `json:"tool,omitempty"`
	// RunID is the durable terminal_run identity this PTY handle belongs to
	// (F1); empty when the session predates the run-identity wiring.
	RunID string `json:"run_id,omitempty"`
	// HasProjectRoot reports whether this run has a resolvable project root
	// (Arc A). It lets the dashboard enable/disable the per-terminal Files/Git
	// panel buttons straight from the /api/launch/sessions rehydrate with no
	// extra round-trip. The raw path is DELIBERATELY not carried here (this
	// struct flows to remote viewers); the path itself is served only by the
	// gated project-panel endpoint.
	HasProjectRoot bool      `json:"has_project_root"`
	CreatedAt      time.Time `json:"created_at"`
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
	// PTY geometry (Feature 2): InitialRows/InitialCols are the spawn size (or
	// the first resize when the launch Spec was 0×0); Rows/Cols are the current
	// live size. Zero means not yet known. Surfaced so REST + the terminal UI can
	// report / restore the session's real dimensions.
	InitialRows uint16 `json:"initial_rows,omitempty"`
	InitialCols uint16 `json:"initial_cols,omitempty"`
	Rows        uint16 `json:"rows,omitempty"`
	Cols        uint16 `json:"cols,omitempty"`
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

// ControlDenialReason is the stable wire taxonomy for a refused remote writer
// acquire. Only ControlDenialAuth means a credential was genuinely rejected;
// callers must not clear a standing secret for any other reason.
type ControlDenialReason string

const (
	// ControlDenialAuth means the capability/confirm or standing secret failed
	// its credential check.
	ControlDenialAuth ControlDenialReason = "auth"
	// ControlDenialHeldLocally means a local/native writer holds control and the
	// authenticated-remote takeover policy is disabled.
	ControlDenialHeldLocally ControlDenialReason = "held_locally"
	// ControlDenialHeldByRemote means another remote writer holds control and the
	// authenticated-remote takeover policy is disabled.
	ControlDenialHeldByRemote ControlDenialReason = "held_by_remote"
	// ControlDenialTerminalDisabled means [remote].allow_terminal is off.
	ControlDenialTerminalDisabled ControlDenialReason = "terminal_disabled"
	// ControlDenialSessionInvalid means the paired device session is no longer valid.
	ControlDenialSessionInvalid ControlDenialReason = "session_invalid"
	// ControlDenialPolicyDenied means listener/launch/session policy refused the target.
	ControlDenialPolicyDenied ControlDenialReason = "policy_denied"
	// ControlDenialNotFound means the terminal session no longer exists.
	ControlDenialNotFound ControlDenialReason = "not_found"
	// ControlDenialUnavailable means the writer path or a raced lifecycle state
	// was unavailable without proving any credential defect.
	ControlDenialUnavailable ControlDenialReason = "unavailable"
)

// ControlDeniedError carries a typed acquire refusal across the cmd adapter
// boundary without importing internal/termsession into dashboard. Cause remains
// unwrap-able for backend tests and diagnostics. CapabilityConsumed is true
// only when a valid single-use capability was burned before a later lease-policy
// refusal, allowing the audit/UI to request a fresh approval honestly.
type ControlDeniedError struct {
	Reason             ControlDenialReason
	CapabilityConsumed bool
	Cause              error
}

// Error implements error without exposing secret-adjacent details.
func (e *ControlDeniedError) Error() string {
	if e == nil {
		return "terminal control denied"
	}
	return "terminal control denied: " + string(e.Reason)
}

// Unwrap preserves the adapter's underlying sentinel for internal callers.
func (e *ControlDeniedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewControlDeniedError constructs a typed dashboard-boundary denial.
func NewControlDeniedError(reason ControlDenialReason, capabilityConsumed bool, cause error) error {
	return &ControlDeniedError{Reason: reason, CapabilityConsumed: capabilityConsumed, Cause: cause}
}

// visibleSnapshot returns the live launch-session list filtered for the request
// principal: on the owner-trusted loopback listener (not remote-exposed) it is
// the full snapshot; for a REMOTE-exposed caller every SpecSetup handle is
// redacted, and every remote-sensitive (attach OR resume) handle is redacted
// UNLESS the [remote].allow_terminal_view READ opt-in is on (defence in depth
// over the SubscribeRemote / WS-gate refusals). Branches on the boundary-resolved
// local-vs-remote signal (remoteExposedFromContext) + the run KIND carried on the
// row (the shared termrun.IsRemoteSensitiveKind table), never on tool/route name.
//
// The remote-VIEW gate `[remote].allow_terminal_view` now DEFAULTS TRUE (§3.2),
// so a paired remote device SEES attach/resume rows by default and may subscribe
// read-only past the WS gate. This is acceptable because remote access is already
// multi-lever gated (armed rail + authenticated paired device over the tailnet)
// and the remote WRITE/drive path is UNCHANGED — driving still requires
// allow_terminal plus the full writer-acquire conjunction. The toggle still
// exists: allow_terminal_view = false restores the deny-read posture, redacting
// these rows uniformly across every remotely-visible snapshot
// (/api/attach/sessions, /api/terminal/sessions, /api/launch/sessions all route
// through here). SpecSetup handles are ALWAYS redacted — no view opt-in relaxes
// them.
func (s *Server) visibleSnapshot(ctx context.Context) []LaunchInfo {
	all := s.opts.LaunchManager.Snapshot()
	if !remoteExposedFromContext(ctx) {
		return all
	}
	// The remote-VIEW opt-in ([remote].allow_terminal_view, default TRUE) lets a
	// remote caller SEE attach/resume rows; when explicitly turned OFF they are
	// redacted. Setup handles are ALWAYS redacted (local-only; no view opt-in
	// relaxes them).
	viewSensitive := s.allowTerminalView()
	out := make([]LaunchInfo, 0, len(all))
	for _, info := range all {
		if info.Setup {
			continue // redact privileged setup handles from remote callers
		}
		if !viewSensitive && termrun.IsRemoteSensitiveKind(termrun.Kind(info.Kind)) {
			continue // allow_terminal_view is OFF — hide this attach/resume PTY (§3.2; default is now ON)
		}
		out = append(out, info)
	}
	return out
}

// allowTerminalView reports whether the LIVE [remote].allow_terminal_view READ
// opt-in is enabled (session-attach design §3.2). It is the independent
// remote-VIEW gate for attach/resume rows — strictly weaker than allow_terminal
// (write) — and now DEFAULTS TRUE in config, so a remote-exposed node exposes
// these rows read-only unless the operator sets allow_terminal_view = false.
// Read via a type assertion off the injected RemoteController (additive,
// CLAUDE.md #6): a nil / non-implementing controller (loopback-only, no remote
// exposure) yields false, so the redaction still holds on any non-remote path
// and every existing test path is unchanged. Mirrors how the live allow_terminal
// gate is read.
func (s *Server) allowTerminalView() bool {
	v, ok := s.opts.Remote.(allowTerminalViewer)
	return ok && v.AllowTerminalView()
}

// SpawnAuditKind maps a spawned run KIND to its metadata-only remote_audit
// event kind (session-attach design §3.5). Table-driven per CLAUDE.md #5 — a new
// spawnable kind is one row, never a nested branch — and the SINGLE vocabulary
// both spawn sites use (the dashboard resume handler + the cmd-side attach host)
// instead of bespoke inline strings. Empty for a kind with no spawn-audit event.
func SpawnAuditKind(k termrun.Kind) string {
	switch k {
	case termrun.KindAttach:
		return "terminal_attach"
	case termrun.KindResume:
		return "terminal_resume"
	default:
		return ""
	}
}

// sensitiveViewer is one registered remote-exposed read-only viewer of a
// remote-sensitive (attach/resume) session admitted under allow_terminal_view.
// It carries the bridge-scoped cancel AND the viewer's FULL device-session key
// (its sha256 hash, NOT the 32-bit display fingerprint) so a per-device
// revocation can target exactly that device's open views without a prefix
// collision reaching another device's viewer (F2).
type sensitiveViewer struct {
	deviceKey string
	cancel    context.CancelFunc
}

// registerSensitiveViewer records the bridge-scoped cancel of a LIVE
// remote-exposed read-only viewer of a remote-sensitive (attach/resume) session,
// admitted under allow_terminal_view. deviceKey is the viewer's FULL
// device-session key (deviceSessionKey — the sha256 hash of its device-session,
// never the 32-bit display fingerprint), so a per-device revoke can close
// exactly this viewer with no prefix-collision hole (F2). It returns a deregister
// func the caller MUST defer so a normally-disconnecting viewer leaves no entry
// behind. On an allow_terminal_view→false flip, closeRemoteSensitiveViewers
// cancels every registered entry, tearing the open view down at once (§3.2
// read-side revoke).
func (s *Server) registerSensitiveViewer(deviceKey string, cancel context.CancelFunc) func() {
	s.sensitiveViewerMu.Lock()
	if s.sensitiveViewers == nil {
		s.sensitiveViewers = make(map[uint64]sensitiveViewer)
	}
	id := s.sensitiveViewerSeq
	s.sensitiveViewerSeq++
	s.sensitiveViewers[id] = sensitiveViewer{deviceKey: deviceKey, cancel: cancel}
	s.sensitiveViewerMu.Unlock()
	return func() {
		s.sensitiveViewerMu.Lock()
		delete(s.sensitiveViewers, id)
		s.sensitiveViewerMu.Unlock()
	}
}

// closeRemoteSensitiveViewers cancels every LIVE remote-exposed read-only viewer
// of a remote-sensitive session admitted under allow_terminal_view — the READ
// analogue of revokeRemoteWriters. Called when the owner flips
// allow_terminal_view→false so an already-open remote view of a secret-bearing
// attach/resume TUI is torn down NOW, not merely blocked for future subscribes.
// The owner-local (loopback) viewers are never registered here, so they are
// untouched. Snapshots the cancels under the lock, then cancels outside it (each
// cancel unblocks a bridge goroutine that will deregister itself).
func (s *Server) closeRemoteSensitiveViewers() int {
	s.sensitiveViewerMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.sensitiveViewers))
	for _, v := range s.sensitiveViewers {
		cancels = append(cancels, v.cancel)
	}
	s.sensitiveViewerMu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// closeRemoteSensitiveViewersForDevice cancels every LIVE remote-exposed
// read-only sensitive viewer belonging to ONE device session key (F2) — the
// per-device analogue of closeRemoteSensitiveViewers. Called when a single
// device session is revoked (admin per-device revoke or self-logout) so a
// revoked device stops receiving attach/resume PTY output at once, WITHOUT
// disturbing other devices' still-authorized views. The key is the FULL device-
// session hash (deviceSessionKey / SessionInfo.ID), never the display prefix, so
// a fingerprint collision can never disconnect the wrong device's viewer. An
// empty key (the owner-local path, never registered here) matches nothing.
// Snapshots the matching cancels under the lock, then cancels outside it (each
// cancel unblocks a bridge goroutine that will deregister itself). Returns the
// count closed.
func (s *Server) closeRemoteSensitiveViewersForDevice(deviceKey string) int {
	if deviceKey == "" {
		return 0
	}
	s.sensitiveViewerMu.Lock()
	cancels := make([]context.CancelFunc, 0)
	for _, v := range s.sensitiveViewers {
		if v.deviceKey == deviceKey {
			cancels = append(cancels, v.cancel)
		}
	}
	s.sensitiveViewerMu.Unlock()
	for _, c := range cancels {
		c()
	}
	return len(cancels)
}

// sessionLifetimeFloor bounds how eagerly a bound sensitive viewer re-checks a
// device session it could not yet prove expired. Revoke/rotate arrive INSTANTLY
// on the revocation channel; only the timer-less TTL/idle expiry needs a wake.
// The floor clamps ONLY a non-positive (already-at/past-deadline) re-arm so it
// can't spin — a KNOWN positive deadline is honoured un-floored, so expiry
// cancels the viewer at the deadline instead of up to a floor's-width later.
const sessionLifetimeFloor = 250 * time.Millisecond

// deviceSessionLive reports whether the raw device-session cookie is STILL a
// live session — the F1a post-register re-validation. An absent controller
// (loopback) or an empty cookie is treated as live (not disproven): the remote
// authz chain that admits a real remote request always carries a valid session,
// so the only race this closes involves a cookie that authenticated and was then
// revoked/expired between the gate check and registration. Read-only (via
// SessionLifetime — no idle refresh).
func (s *Server) deviceSessionLive(raw string) bool {
	if raw == "" {
		return true
	}
	lt, ok := s.opts.Remote.(deviceSessionLifetimer)
	if !ok {
		return true
	}
	_, _, live := lt.SessionLifetime(raw)
	return live
}

// bindViewerLifetime cancels a live remote-sensitive viewer the moment its
// device session ends — revoked, rotated, or TTL/idle-expired (F1b) — closing
// the gap the allow_terminal_view drain alone leaves (a session can end without
// the operator ever flipping the view opt-in). A nil / non-lifetimer controller
// (loopback) or empty cookie is a no-op. The watch goroutine exits when the
// viewer disconnects (done closed) so it never leaks.
func (s *Server) bindViewerLifetime(raw string, done <-chan struct{}, cancel context.CancelFunc) {
	if raw == "" {
		return
	}
	lt, ok := s.opts.Remote.(deviceSessionLifetimer)
	if !ok {
		return
	}
	go watchSessionLifetime(raw, lt, done, cancel, sessionLifetimeFloor)
}

// watchSessionLifetime is the testable core of bindViewerLifetime. It selects on
// the session's revocation channel (instant revoke/rotate), a timer to the next
// TTL/idle deadline (re-armed on each wake so an idle refresh that extended the
// session is honoured), and done (viewer disconnect). On session-end it calls
// cancel; on a clean disconnect it returns WITHOUT cancelling. floor clamps ONLY
// a non-positive re-arm so a near-deadline poll can't spin — a known positive
// deadline is left un-floored so expiry cancels at the deadline, not later.
func watchSessionLifetime(raw string, lt deviceSessionLifetimer, done <-chan struct{}, cancel context.CancelFunc, floor time.Duration) {
	for {
		revoked, until, live := lt.SessionLifetime(raw)
		if !live {
			cancel()
			return
		}
		if until <= 0 {
			// Only a non-positive (deadline already reached) wake gets the
			// floor, to keep the re-arm from spinning; a KNOWN positive deadline
			// passes through un-floored so TTL/idle expiry cancels the viewer
			// promptly rather than up to floor later.
			until = floor
		}
		timer := time.NewTimer(until)
		select {
		case <-done:
			timer.Stop()
			return
		case <-revoked:
			timer.Stop()
			cancel()
			return
		case <-timer.C:
			// Deadline reached — loop re-reads liveness. If the session was
			// idle-refreshed by other activity it is still live with a fresh
			// deadline; otherwise SessionLifetime now reports live=false → cancel.
		}
	}
}

// sessionHasLiveSensitiveRun reports whether a live (non-exited) terminal run of
// a remote-sensitive kind (attach OR resume) is already bound to sessionID. It
// reads the SAME Snapshot + run-kind data the remote gates use: a resume run
// carries the resumed session as its Snapshot SessionID; a live attach run
// carries its OOB-correlated session id. Used by handleSessionResume to refuse a
// duplicate native resume (F5) that would otherwise spawn a second tool process
// writing the same transcript concurrently. A nil manager / empty id ⇒ false.
func (s *Server) sessionHasLiveSensitiveRun(sessionID string) bool {
	if s.opts.LaunchManager == nil || sessionID == "" {
		return false
	}
	for _, info := range s.opts.LaunchManager.Snapshot() {
		if info.Exited || info.SessionID != sessionID {
			continue
		}
		if termrun.IsRemoteSensitiveKind(termrun.Kind(info.Kind)) {
			return true
		}
	}
	return false
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

// sessionResumeInfo is the additive `resume` block on the session-detail
// payload (session-attach design Phase 3). It tells the frontend, by CAPABILITY
// SHAPE (never a tool-name branch), how a CLOSED session on this tool can be
// reopened:
//   - Kind "native":  the tool has a grounded ResumeNative contract — the
//     dashboard offers a Resume button that POSTs /resume and docks a terminal
//     running the tool's own resume mechanism (the real transcript).
//   - Kind "handoff": no native resume, but the tool is launchable in the
//     embedded terminal — the frontend points the operator at the existing
//     Continue-in… (handoff-fork) card rather than duplicating that UI.
//   - Kind "none":    neither — an honest-disabled affordance naming the gap.
//
// Subcommand carries the native launcher verb for kind "native" (empty
// otherwise) so the honest-disabled/hint copy can name the exact command.
type sessionResumeInfo struct {
	Kind       string `json:"kind"`
	Subcommand string `json:"subcommand"`
}

// resumeInfoForTool derives the session-detail `resume` block from the
// integration capability registry, dispatching on capability SHAPE (CLAUDE.md
// #3): a grounded ResumeNative spec → "native"; else a launchable
// handoff/continue-from capability → "handoff"; else "none". An unknown tool
// (no registry row) is "none" — the honest floor.
func resumeInfoForTool(tool string) sessionResumeInfo {
	cap, ok := integration.For(tool)
	if !ok {
		return sessionResumeInfo{Kind: "none"}
	}
	if cap.Resume.Kind == integration.ResumeNative {
		return sessionResumeInfo{Kind: "native", Subcommand: cap.Resume.Subcommand}
	}
	if cap.Handoff.Launchable() {
		return sessionResumeInfo{Kind: "handoff"}
	}
	return sessionResumeInfo{Kind: "none"}
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
	// HasProjectRoot lets the dock enable the Files/Git project panels
	// immediately on a fresh launch instead of waiting for a later
	// /api/launch/sessions rehydrate (finding 8). Resolved from the same
	// token→root seam the project panel Snapshot uses.
	HasProjectRoot bool `json:"has_project_root"`
}

// hasProjectRoot reports whether a freshly-minted terminal token resolves to a
// known, non-default project root — via the same ProjectRootResolver seam the
// project panel uses. A nil resolver (panel unwired) or a rootless/unknown token
// both yield false, so a POST response can honestly tell the dock up front
// whether the Files/Git panels are available (finding 8).
func (s *Server) hasProjectRoot(token string) bool {
	if s.opts.ProjectRootResolver == nil {
		return false
	}
	root, known := s.opts.ProjectRootResolver(token)
	return known && root != ""
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
	writeJSON(w, launchResponse{
		Token: handle, Subcommand: sub, SessionID: sessionID,
		HasProjectRoot: s.hasProjectRoot(handle),
	})
}

// resumeResponse is the reply from POST /api/session/<id>/resume. It carries
// the opaque terminal handle (`token` — the field the frontend dock.launch path
// reads, identical to handleSessionLaunch's launchResponse) plus the durable
// run id and the resolved launcher subcommand. session_id echoes the resumed
// session so the dock header can name it without re-deriving.
type resumeResponse struct {
	Token      string `json:"token"`
	RunID      string `json:"run_id"`
	Subcommand string `json:"subcommand"`
	SessionID  string `json:"session_id"`
	// HasProjectRoot: see launchResponse (finding 8).
	HasProjectRoot bool `json:"has_project_root"`
}

// resumeGate is a reference-counted per-session mutex used to single-flight the
// resume check+spawn (R2-3). refs is guarded by Server.resumeMu; mu serializes
// the critical section for one session id.
type resumeGate struct {
	mu   sync.Mutex
	refs int
}

// acquireResumeLock returns a function that releases the per-session resume
// gate. It blocks until this caller holds the gate for sessionID, so the
// liveness check + spawn between acquire and release run single-flight against
// any concurrent resume of the same session (R2-3). The gate is reference-
// counted: the last releaser deletes it from the map so idle sessions leave no
// entry behind. Different session ids never contend (independent gates).
func (s *Server) acquireResumeLock(sessionID string) func() {
	s.resumeMu.Lock()
	if s.resumeGates == nil {
		s.resumeGates = make(map[string]*resumeGate)
	}
	g := s.resumeGates[sessionID]
	if g == nil {
		g = &resumeGate{}
		s.resumeGates[sessionID] = g
	}
	g.refs++
	s.resumeMu.Unlock()

	g.mu.Lock()
	return func() {
		g.mu.Unlock()
		s.resumeMu.Lock()
		g.refs--
		if g.refs == 0 {
			delete(s.resumeGates, sessionID)
		}
		s.resumeMu.Unlock()
	}
}

// handleSessionResume serves POST /api/session/<id>/resume — the P3 NATIVE
// resume. It loads the closed session's tool + project_root, REQUIRES a grounded
// ResumeNative contract for that tool (else 409, naming the handoff-fork
// fallback honestly), composes the resume argv tail through the single pure seam
// integration.ResumeArgs (dispatch on capability SHAPE, never tool name —
// CLAUDE.md #3), and spawns a fresh dashboard-owned terminal via CreateResume
// under the application service's fresh-launch policy. The response mirrors
// handleSessionLaunch's wire shape (a `token`) so the frontend dock path is
// identical, plus run_id.
func (s *Server) handleSessionResume(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.LaunchManager == nil {
		http.Error(w, "resume unavailable — this dashboard runs without the embedded-terminal launcher (set [handoff].allow_dashboard_launch and run via `observer start`)", http.StatusServiceUnavailable)
		return
	}
	if strings.TrimSpace(sessionID) == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}

	// Load the session's tool + project_root (the only server-side inputs a
	// resume needs; argv is composed from the registry, never the client).
	var tool, projectRoot string
	err := s.db().QueryRowContext(
		r.Context(),
		`SELECT s.tool, COALESCE(p.root_path, '')
		 FROM sessions s LEFT JOIN projects p ON p.id = s.project_id
		 WHERE s.id = ?`, sessionID,
	).Scan(&tool, &projectRoot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		writeErr(w, err)
		return
	}

	// Require a grounded native-resume contract. A tool without one (ResumeNone
	// / ResumeFork) has no real transcript to reopen from the dashboard — 409
	// with an honest message naming the handoff-fork fallback the Continue-in…
	// card provides. Dispatch on capability SHAPE, never a tool-name switch.
	cap, ok := integration.For(tool)
	if !ok || cap.Resume.Kind != integration.ResumeNative {
		http.Error(w, "native resume not grounded for "+tool+"; use Continue in… to fork instead", http.StatusConflict)
		return
	}

	// Single-flight the duplicate-resume check + spawn per session id (R2-3):
	// hold the per-session gate across BOTH sessionHasLiveSensitiveRun and
	// CreateResume so two concurrent POSTs can't both pass the check and both
	// spawn. The second POST blocks here, then observes the first's now-live run
	// and 409s. Released at handler return (covers every branch below).
	releaseResume := s.acquireResumeLock(sessionID)
	defer releaseResume()

	// Refuse a DUPLICATE native resume (F5): if the session already has a live
	// (non-exited) terminal run of a remote-sensitive kind — a resume bound to
	// this session, or a live attach the operator can Jump into — spawning a
	// second one would run TWO tool processes writing the SAME transcript
	// concurrently. 409 with an honest message; the client flips the Resume
	// button to a disabled "already running" state on this status.
	if s.sessionHasLiveSensitiveRun(sessionID) {
		http.Error(w, "session already has a live terminal run — jump in or wait for it to end", http.StatusConflict)
		return
	}

	extraArgs, err := integration.ResumeArgs(cap.Resume, sessionID)
	if err != nil {
		// A grounded ResumeNative whose argv still can't be composed (empty/
		// unsafe id, unknown mechanism) is a 400 — the id came from the URL.
		http.Error(w, "cannot compose resume command: "+err.Error(), http.StatusBadRequest)
		return
	}

	handle, runID, err := s.opts.LaunchManager.CreateResume(ResumeLaunchSpec{
		Tool:        tool,
		Subcommand:  cap.Resume.Subcommand,
		SessionID:   sessionID,
		ProjectRoot: projectRoot,
		ExtraArgs:   extraArgs,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrLaunchFreshDisabled):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrLaunchToolNotAllowed):
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrLaunchProjectRootDenied):
			// F6: the project root is loaded server-side from the stored session,
			// so an allow-list rejection is an AUTHORIZATION-policy refusal (403),
			// not malformed client input (400). Mirrors fresh-disabled /
			// tool-denied, which correctly return 403.
			http.Error(w, err.Error(), http.StatusForbidden)
		case errors.Is(err, ErrLaunchTooMany):
			http.Error(w, err.Error(), http.StatusTooManyRequests)
		case errors.Is(err, ErrLaunchUnsupported):
			http.Error(w, err.Error(), http.StatusNotImplemented)
		default:
			http.Error(w, "resume failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// Spawn-time audit (F4, session-attach design §3.5): emit exactly one
	// metadata-only terminal_resume row at the spawn success point, BEFORE
	// returning the resume token. Without this a caller that POSTs /resume but
	// never opens the websocket produces a real process with NO remote_audit row
	// (the writer-lease audit only fires once a client attaches). Metadata only —
	// run id, handle, tool — never argv/env/content.
	s.auditSpawn(SpawnAuditKind(termrun.KindResume), tool, handle, runID, r)
	writeJSON(w, resumeResponse{
		Token: handle, RunID: runID, Subcommand: cap.Resume.Subcommand, SessionID: sessionID,
		HasProjectRoot: s.hasProjectRoot(handle),
	})
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
	// HasProjectRoot: see launchResponse (finding 8).
	HasProjectRoot bool `json:"has_project_root"`
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
	writeJSON(w, terminalLaunchResponse{
		Token: handle, Tool: body.Tool, Subcommand: sub,
		HasProjectRoot: s.hasProjectRoot(handle),
	})
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

// handleAttachSessions serves GET /api/attach/sessions — the live
// LIVE-ATTACHABLE session list (session-attach design Phase 2, dashboard "Jump
// in"). It returns exactly the visibleSnapshot(ctx) rows whose run Kind is
// "attach" and which have NOT exited: daemon-owned external sessions the
// operator launched with `observer <tool> --attach`, which a dashboard tab can
// join as viewer #2 over the existing /ws/launch/<handle> bridge. Classified
// VIEW (§9): metadata only, no content.
//
// Only Kind=="attach" is offered because attach sessions are the sole class
// with EXACT daemon-owned liveness (design §4); a bare external session's stdio
// belongs to the user's shell and is never re-parentable, so it is deliberately
// excluded here rather than presented with a dishonest "Jump in" affordance.
//
// Each row is the LaunchInfo JSON verbatim (no bespoke struct) — the frontend
// "Jump in" affordance builds against that shape directly.
//
// Remote callers get the SAME visibleSnapshot semantics as
// /api/terminal/sessions today (setup rows already redacted; attach rows are
// never setup). Phase 4 tightens remote VIEW of attach sessions behind
// [remote].allow_terminal_view — deliberately NOT built here.
func (s *Server) handleAttachSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.opts.LaunchManager == nil {
		http.Error(w, "launch unavailable", http.StatusServiceUnavailable)
		return
	}
	all := s.visibleSnapshot(r.Context())
	out := make([]LaunchInfo, 0, len(all))
	for _, info := range all {
		if info.Kind != string(termrun.KindAttach) || info.Exited {
			continue
		}
		out = append(out, info)
	}
	writeJSON(w, map[string]any{"sessions": out})
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
	// Gen identifies the session-monotonic writer-lease generation on
	// control_granted and control_revoked. The browser uses it to ignore a stale
	// revoke that arrives after a newer grant during rapid takeover ping-pong.
	Gen uint64 `json:"gen,omitempty"`
	// InitialRows/InitialCols ride the on-open {"t":"pty_size",…} geometry frame
	// (Feature 2) alongside Rows/Cols (the current size), so the web terminal can
	// restore the PTY's real dimensions. Omitted on every other control frame.
	InitialRows uint16 `json:"initial_rows,omitempty"`
	InitialCols uint16 `json:"initial_cols,omitempty"`
	// Cap + Confirm ride the remote writer-acquire control frame
	// ({"t":"acquire-writer","cap":…,"confirm":…}) — the single-use terminal-
	// control capability + its bound confirm (§4.γ). They are accepted ONLY in
	// this TEXT frame body over the already-Origin-checked, cookie-authenticated
	// websocket, never a URL/subprotocol/query (§8.1 #5).
	Cap     string `json:"cap,omitempty"`
	Confirm string `json:"confirm,omitempty"`
	// By identifies the requester that superseded this writer (local|remote).
	By string `json:"by,omitempty"`
	// Reason carries the typed denial taxonomy on control_denied.
	Reason ControlDenialReason `json:"reason,omitempty"`
}

// ptyGeometry is the PTY dimension snapshot the on-open (and control-transition)
// pty_size frame carries (Feature 2): the current and initial dimensions.
type ptyGeometry struct {
	rows, cols, initialRows, initialCols uint16
}

// ptySizeForHandle returns the PTY geometry the pty_size frame reports for handle
// (Feature 2). It reads the ONE live snapshot the launch manager owns — never a
// second geometry source — so the value tracks the manager's resize funnel. All
// zero (including an unknown handle) means "size not yet known".
func (s *Server) ptySizeForHandle(handle string) ptyGeometry {
	if s.opts.LaunchManager == nil {
		return ptyGeometry{}
	}
	for _, info := range s.opts.LaunchManager.Snapshot() {
		if info.ID == handle {
			return ptyGeometry{rows: info.Rows, cols: info.Cols, initialRows: info.InitialRows, initialCols: info.InitialCols}
		}
	}
	return ptyGeometry{}
}

// writePTYSize sends the on-open (and, cheaply, control-transition) geometry
// frame (Feature 2), the pinned wire contract {"t":"pty_size","rows","cols",
// "initial_rows","initial_cols"}. Best-effort like the other control writes.
func writePTYSize(ctx context.Context, c *websocket.Conn, g ptyGeometry) {
	_ = wsjson.Write(ctx, c, wsControl{
		T: "pty_size", Rows: g.rows, Cols: g.cols,
		InitialRows: g.initialRows, InitialCols: g.initialCols,
	})
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
	// Deny-by-default remote content access to a remote-sensitive session
	// (§3.2): a paired remote device may not READ the PTY of a session the
	// operator launched with `observer <tool> --attach` (KindAttach) OR a native
	// resume of a real closed transcript (KindResume) — its TUI can echo
	// secrets/customer data — UNLESS the owner has turned on the Phase-4 remote-
	// VIEW opt-in [remote].allow_terminal_view. When off, close with the SAME
	// "session not found" policy close SubscribeRemote uses for a refused setup
	// session, so a remote caller cannot distinguish "refused" from "absent".
	// When ON, the remote caller may VIEW (read-only subscribe) — but WRITER
	// acquisition stays governed by the EXISTING remote-write path below
	// (allow_terminal + the §4.δ execute conjunction); the view opt-in is
	// strictly weaker and never touches that. Gated on run KIND at the dashboard
	// boundary — never a termsession branch.
	if remoteExposed && s.opts.LaunchManager.IsRemoteSensitiveSession(handle) {
		if !s.allowTerminalView() {
			_ = c.Close(websocket.StatusPolicyViolation, "session not found")
			return
		}
		// View opted in: admit the read-only subscribe, but register this viewer
		// under a bridge-scoped cancelable context so an allow_terminal_view→false
		// flip tears it down immediately (§3.2 read-side revoke). Rebind the
		// request onto the cancelable context so the bridge below observes the
		// cancel. Keyed by the device fingerprint so a per-device revoke (F2) can
		// target exactly this viewer.
		vctx, vcancel := context.WithCancel(r.Context())
		defer vcancel()
		device := sessionCookie(r)
		// Key the viewer registry by the FULL device-session hash (F2b), never
		// the 32-bit display fingerprint, so a per-device revoke targets exactly
		// this device and a fingerprint collision can never disconnect it.
		unregister := s.registerSensitiveViewer(deviceSessionKey(device), vcancel)
		// F1: RE-CHECK the live gate AFTER registering. A concurrent
		// allow_terminal_view→false flips the gate and THEN drains the registry;
		// a subscribe that passed the first check but registered after that drain
		// would otherwise stream forever. Because the disable path orders
		// gate-flip → drain, and we order register → re-read, the two interleave
		// safely: either the drain observes our registration (and cancels it), or
		// the flip is visible to this re-read (and we refuse). Never both survive.
		if !s.allowTerminalView() {
			unregister()
			_ = c.Close(websocket.StatusPolicyViolation, "session not found")
			return
		}
		// F1a: RE-VALIDATE the DEVICE SESSION itself after registering — not just
		// the view gate. The SAME drain-then-register race closes for a session
		// REVOKE/expiry: an admin/self revoke drains the registry AFTER marking
		// the session dead, so a viewer that registered past that drain is caught
		// here by a post-register liveness re-read (revoked/expired ⇒ refuse). The
		// revoke orders mark-dead → drain and we order register → re-read, so the
		// two interleave safely (mirrors the gate re-check above).
		if !s.deviceSessionLive(device) {
			unregister()
			_ = c.Close(websocket.StatusPolicyViolation, "session not found")
			return
		}
		defer unregister()
		// F1b: BIND this viewer to the device session's lifetime — cancel it the
		// moment the session ends (revoke / rotate / TTL / idle expiry), so a
		// device that ends WITHOUT the operator flipping allow_terminal_view still
		// stops receiving attach/resume output at once. Exits on viewer disconnect.
		s.bindViewerLifetime(device, vctx.Done(), vcancel)
		r = r.WithContext(vctx)
	}
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

	// A remote-exposed request may never take the owner-local writer implicitly
	// (that would hand a remote principal an ungated PTY). It starts read-only and
	// must acquire through the §4.δ conjunction (AcquireWriterRemote) via a
	// writer-acquire control frame; only after that credential gate may live
	// policy permit a takeover. The owner-trusted loopback path keeps the
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
			standingCredential := remoteauth.IsStandingSecret(capTok)
			wl, err := s.opts.LaunchManager.AcquireWriterRemote(RemoteWriterRequest{
				Handle:          handle,
				DeviceSessionID: device,
				CapabilityToken: capTok,
				Confirm:         confirm,
				RemoteExposed:   true, // provenance, resolved at the boundary
			})
			if err != nil || wl == nil {
				reason := ControlDenialUnavailable
				var denial *ControlDeniedError
				if errors.As(err, &denial) {
					if denial.Reason != "" {
						reason = denial.Reason
					}
					if denial.CapabilityConsumed {
						s.auditTerminalControl("terminal_control_capability_consume", deviceFP, handle, peer, "allow", "consumed_before_"+string(reason))
					}
				}
				// The reason is coarse and stable; it never reveals which credential
				// sub-leg failed. A consumed capability is audited separately above.
				s.auditTerminalControl("terminal_control_denied", deviceFP, handle, peer, "deny", string(reason))
				return wl, err
			}
			if !standingCredential {
				s.auditTerminalControl("terminal_control_capability_consume", deviceFP, handle, peer, "allow", "")
			}
			return wl, err
		}
		// Denied-frame coalescer: a viewer with no writer lease flooding forged
		// input/control frames (§4.β drop) must not amplify the audit log — its
		// drops are batched into bounded rows carrying a coalesced count.
		denied := s.newDeniedFrameCoalescer(deviceFP, handle, peer)
		// closeOnHardRevoke=true: on the REMOTE bridge an admin/device revoke of
		// the writer lease closes the socket (the device is no longer trusted); a
		// lease takeover only demotes. The owner-local bridge below passes false
		// so its revoke behaviour stays byte-identical (always demote).
		s.bridgeTerminalWS(r.Context(), c, sub, nil, acquire, denied, true, func() ptyGeometry { return s.ptySizeForHandle(handle) })
		return
	}

	// Owner-local writer lease (loopback path). If the session vanished between
	// Subscribe and here we degrade to a read-only viewer rather than failing.
	writer, werr := s.opts.LaunchManager.AcquireWriterLocal(handle)
	if werr == nil {
		defer writer.Release()
	}

	// Local mid-stream re-acquire: after a native-terminal reclaim revokes this
	// seat's lease, the client asks for control back with the same
	// {"t":"acquire-writer"} frame the remote path uses. The loopback seat is
	// owner-trusted (it took the CapabilityLocal writer unconditionally above),
	// so re-acquiring is the same privilege — no conjunction, cap/confirm
	// ignored.
	localAcquire := func(_, _ string) (LaunchWriter, error) {
		return s.opts.LaunchManager.AcquireWriterLocal(handle)
	}

	// The owner-local loopback path has no forged-viewer amplification surface
	// (it holds the writer), so no denied-frame coalescer is wired.
	// closeOnHardRevoke=false: the local loopback path always DEMOTES on a
	// revoke (byte-identical to prior behaviour) — the socket-closing branch is
	// remote-only.
	s.bridgeTerminalWS(r.Context(), c, sub, writer, localAcquire, nil, false, func() ptyGeometry { return s.ptySizeForHandle(handle) })
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
// REMOTE bridge), a writer-lease revocation that is NOT a lease takeover — an
// admin disable/rotate/device-revoke/allow_terminal→false, i.e. the device is no
// longer trusted — CLOSES the socket; a lease takeover only demotes the client to
// a read-only viewer. When false (the owner-local bridge) every revoke merely
// demotes, keeping that path byte-identical. A normal PTY-exit teardown never
// reaches this branch: the exit notifier cancels the bridge context first, so
// watchRevoke returns via ctx.Done rather than the revoked channel.
func (s *Server) bridgeTerminalWS(parent context.Context, c *websocket.Conn, sub LaunchSubscription, writer LaunchWriter, acquire func(capTok, confirm string) (LaunchWriter, error), denied *deniedFrameCoalescer, closeOnHardRevoke bool, geo func() ptyGeometry) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// PTY geometry announce (Feature 2): send the pinned {"t":"pty_size",…} frame
	// right after bridge start so the web terminal can restore the PTY's real
	// dimensions, and re-send it on a control transition (cheap; the client
	// re-fits when control changes hands). geo re-reads the manager's live size
	// each call, so a control-transition re-send is never stale. nil geo (older
	// call sites / tests) disables the frame.
	sendPTYSize := func() {
		if geo != nil {
			writePTYSize(ctx, c, geo())
		}
	}
	sendPTYSize()
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

	// ctrlSeqMu serializes control_granted / control_revoked writes so the
	// client can never observe them out of order. Without it, watchRevoke (a
	// goroutine) and the read loop's grant branches write concurrently: a lease
	// revoked between the loop's staleness check and its wsjson.Write could land
	// control_revoked FIRST and control_granted SECOND, leaving the client
	// believing it is writable while every write is fenced. The grant branches
	// re-check the lease's Revoked signal UNDER this mutex: if a revoke has
	// already fired, the grant is withheld (the watcher sent — or is about to
	// send — the revoked frame); if the revoke fires after, the watcher blocks
	// on the mutex until the grant is on the wire, so the client converges on
	// revoked. Order is then always grant-before-revoke per lease.
	var ctrlSeqMu sync.Mutex

	// sendGrantChecked writes control_granted for wl unless wl is already
	// revoked (then it writes nothing — the revoke watcher owns the notice).
	// Returns whether the grant was sent. Also re-affirms geometry on a sent
	// grant (Feature 2).
	sendGrantChecked := func(wl LaunchWriter) bool {
		ctrlSeqMu.Lock()
		defer ctrlSeqMu.Unlock()
		select {
		case <-wl.Revoked():
			return false
		default:
		}
		_ = wsjson.Write(ctx, c, wsControl{T: "control_granted", Gen: launchWriterGen(wl)})
		sendPTYSize()
		return true
	}

	// watchRevoke tells the client it lost control when a writer's lease is
	// revoked (lease takeover / allow_terminal→false / device revoke). Started
	// once per writer (the initial loopback writer, and again after a remote
	// acquire installs a new one).
	watchRevoke := func(wl LaunchWriter) {
		go func() {
			select {
			case <-wl.Revoked():
				// Always tell the client it lost control. On the remote bridge, a
				// revocation that is NOT a lease takeover means the device is no
				// longer trusted (admin disable/rotate/device-revoke/allow_terminal
				// →false) — cancel the bridge so the deferred CloseNow tears the
				// socket down. A lease takeover (device still valid) only demotes.
				ctrlSeqMu.Lock()
				by := ""
				if rb, ok := wl.(interface{ RevokedBy() string }); ok && wl.RevokeIsTakeover() {
					by = rb.RevokedBy()
				}
				_ = wsjson.Write(ctx, c, wsControl{T: "control_revoked", By: by, Gen: launchWriterGen(wl)})
				// Re-affirm geometry on the control handoff (Feature 2, cheap):
				// after a native-terminal reclaim the client should re-fit to the
				// PTY's current dims.
				sendPTYSize()
				ctrlSeqMu.Unlock()
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
			case ctrl.T == "acquire-writer" && acquire != nil:
				var newlyAcquired LaunchWriter
				writer, newlyAcquired = bridgeAcquireWriter(ctx, c, ctrl, writer, acquire, sendGrantChecked)
				if newlyAcquired != nil {
					acquired = newlyAcquired
					watchRevoke(newlyAcquired)
				}
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

// launchWriterGen reads the concrete lease generation through an additive
// optional interface. LaunchWriter deliberately stays narrow so existing
// dashboard fakes and alternate adapters remain source-compatible; a writer
// without generation support preserves the pre-generation wire shape via
// wsControl's omitempty tag.
func launchWriterGen(wl LaunchWriter) uint64 {
	if withGen, ok := wl.(interface{ Gen() uint64 }); ok {
		return withGen.Gen()
	}
	return 0
}

// bridgeAcquireWriter handles one {"t":"acquire-writer"} control frame for
// bridgeTerminalWS's read loop. The loop's writer can be a STALE demoted lease:
// a revoke (native-terminal reclaim / any lease takeover) fires watchRevoke, but
// the loop variable is only cleared when a write fails — and a demoted client
// stops writing. So a still-live writer is re-affirmed (idempotent,
// revoke-checked under the control-order mutex), a stale one is cleared and
// re-acquired, and a denied acquire yields control_denied. Returns the writer
// the loop should hold afterward, plus the newly acquired lease when one was
// taken (nil otherwise) — the caller owns its release-on-exit and revoke
// watcher. A fresh lease that is already revoked at grant time is returned as
// acquired-but-not-held (nil loop writer): the grant is withheld and the
// caller's watcher surfaces the revoked notice on its own.
func bridgeAcquireWriter(ctx context.Context, c *websocket.Conn, ctrl wsControl,
	writer LaunchWriter,
	acquire func(capTok, confirm string) (LaunchWriter, error),
	sendGrantChecked func(LaunchWriter) bool,
) (loopWriter, newlyAcquired LaunchWriter) {
	if writer != nil {
		if sendGrantChecked(writer) {
			return writer, nil
		}
		// Revoked (either before the check or racing it): treat as stale and
		// fall through to a fresh acquire.
	}
	wl, aerr := acquire(ctrl.Cap, ctrl.Confirm)
	if aerr != nil || wl == nil {
		reason := ControlDenialUnavailable
		var denial *ControlDeniedError
		if errors.As(aerr, &denial) && denial.Reason != "" {
			reason = denial.Reason
		}
		_ = wsjson.Write(ctx, c, wsControl{T: "control_denied", Reason: reason})
		return nil, nil
	}
	// Grant-before-watch: sendGrantChecked withholds the grant if this fresh
	// lease is somehow already revoked.
	if !sendGrantChecked(wl) {
		return nil, wl
	}
	return wl, wl
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
