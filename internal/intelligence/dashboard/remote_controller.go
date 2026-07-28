package dashboard

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/termlease"
)

// Cookie + header names for the remote auth flow (plan §4.3/§4.5).
const (
	remoteSessionCookie = "sb_remote_session"
	remoteCSRFHeader    = "X-Remote-CSRF"    //nolint:gosec // header name, not a credential
	remoteExecuteHeader = "X-Remote-Execute" //nolint:gosec // header name, not a credential
)

// RemoteOptions configures a concrete RemoteController (plan §4). Built by cmd
// from [remote] once exposure is wired (Phase 2); assembled here so the HTTP
// glue (cookies, CSRF, pairing) lives with the mux, not in the pure
// internal/remoteauth primitives it injects.
type RemoteOptions struct {
	// HashedSecret is the argon2id hash of the pairing secret at rest. Empty ⇒
	// not ready (Ready() is false).
	HashedSecret string
	// AllowedHosts is the Host allow-list (configured bind + trusted_hosts).
	AllowedHosts []string
	// RateLimitPerMin throttles pairing attempts (0 disables).
	RateLimitPerMin int
	// AllowTerminal mirrors [remote].allow_terminal for remote fresh terminal
	// launch gating. Writer control is still authorized separately by
	// termlease.Authorize.
	AllowTerminal bool
	// AllowRemoteTerminalTakeover mirrors
	// [remote].allow_remote_terminal_takeover. It is consulted only after the
	// credential conjunction succeeds and defaults true through config.Default.
	// Hot-swapped by ReloadAllowRemoteTerminalTakeover.
	AllowRemoteTerminalTakeover bool
	// AllowTerminalView mirrors [remote].allow_terminal_view — the independent
	// READ opt-in that relaxes the remote-sensitive (attach/resume) deny-by-
	// default (session-attach design §3.2). It ONLY relaxes the VIEW gate
	// (snapshot row + read-only subscribe); the writer path stays governed by
	// AllowTerminal + the execute-tier conjunction. Default TRUE (seeded in
	// config.Default, mirroring AllowRemoteTerminalTakeover) — an explicit
	// allow_terminal_view = false restores the deny-read posture. Hot-swapped
	// by ReloadAllowTerminalView.
	AllowTerminalView bool
	// StandingTerminalSecretHash is the argon2id hash-at-rest of the OPT-IN
	// standing terminal-control secret (standing-terminal-access §B), read from
	// remotecfg.StandingTerminalSecretPath at construction. Empty ⇒ no standing
	// secret provisioned (the standing path denies). Hot-swapped by
	// ReloadStandingTerminalSecret on a dashboard mint/revoke.
	StandingTerminalSecretHash string
	// StandingTerminalEnabled mirrors [remote].allow_standing_terminal_control:
	// the master gate the standing verification leg checks BEFORE the hash
	// compare. Default false. Hot-swapped alongside the hash.
	StandingTerminalEnabled bool
	// StandingSecretAtRest answers, at verify time, whether a standing
	// terminal-control secret still EXISTS at rest on this node. It is the ONE
	// piece of evidence that separates a genuine REVOKE from a temporary
	// DISABLE, which are otherwise byte-identical on the live controller: both
	// arrive as ReloadStandingTerminalSecret("", false).
	//
	//	revoke  → remotecfg.StandingTerminalDisable DELETES the hash file
	//	          ⇒ probe false ⇒ no secret any device holds can ever be
	//	          accepted again (a re-mint issues a DIFFERENT one), so the
	//	          denial is PERMANENT and the device may discard its copy
	//	disable → allow_terminal→false / remote-disable / a config toggle only
	//	          flip the gate and LEAVE the hash file ⇒ probe true ⇒ the
	//	          refusal is TRANSIENT and the device must keep its secret
	//
	// It is deliberately a live probe rather than a reload-time snapshot: the
	// disk is the durable truth, and no hot-reload plumbing can then drift out
	// of sync with it. It MUST fail safe — return true whenever the answer is
	// unknown (stat error, no probe wired) — because a wrong "false" destroys a
	// user's saved secret irreversibly, while a wrong "true" costs one more
	// denied attempt. Nil ⇒ every such denial stays transient (the pre-A2
	// behaviour). Wired by cmd/observer from
	// remotecfg.StandingTerminalSecretPath.
	StandingSecretAtRest func() bool
	// Session tunes the device-session lifecycle.
	Session remoteauth.SessionParams
	// CapabilityTTL is the execute-capability lifetime (0 ⇒ default).
	CapabilityTTL time.Duration
	// Audit receives metadata-only pairing/logout records (never secrets).
	Audit func(RemoteAuditRecord)
	// Now is the clock (test hook). Nil defaults to time.Now.UTC.
	Now func() time.Time
}

// remoteController is the concrete RemoteController: it authenticates requests
// against a device-session store, mints/consumes single-use execute
// capabilities, rate-limits + constant-time-verifies pairing, and issues CSRF
// tokens for cookie-auth mutations.
type remoteController struct {
	// secretMu guards hashedSecret so a dashboard rotate/enable can swap it on
	// the LIVE controller (ReloadSecret) without a daemon restart, while
	// handlePair/Ready read it concurrently.
	secretMu     sync.RWMutex
	hashedSecret string
	allowedHosts []string
	sessions     *remoteauth.SessionStore
	caps         *remoteauth.CapabilityStore
	limiter      *remoteauth.RateLimiter
	audit        func(RemoteAuditRecord)
	now          func() time.Time

	csrfMu sync.Mutex
	csrf   map[string]string // sessionID → csrf token

	// standingMu guards the OPT-IN standing terminal-control secret state so a
	// dashboard mint/revoke (ReloadStandingTerminalSecret) can swap it on the
	// LIVE controller while VerifyStandingTerminalControl reads it concurrently
	// — the same hot-reload discipline as secretMu/hashedSecret.
	standingMu      sync.RWMutex
	standingHash    string
	standingEnabled bool
	// standingAtRest is the RemoteOptions.StandingSecretAtRest probe (nil ⇒
	// "unknown", i.e. fail-safe transient). Set once at construction and only
	// read, so it needs no lock.
	standingAtRest func() bool
	// standingGen is the standing-access GENERATION, bumped on EVERY
	// ReloadStandingTerminalSecret (mint / rotate / revoke / disable). The cmd
	// acquire adapter captures it BEFORE the §4.δ verify and re-checks it AFTER
	// the lease install: an argon2 verify that began against a since-revoked or
	// since-rotated secret can pass, but its lease is torn down at install time
	// (the finding-1 TOCTOU close — an in-flight verify can never outlive the
	// revoke that raced it).
	standingGen     uint64
	standingLimiter *remoteauth.RateLimiter
	// allowTerminal is the LIVE [remote].allow_terminal gate. It is guarded by
	// standingMu (co-located with the standing state it moves in lockstep with)
	// and hot-swapped by ReloadAllowTerminal so a dashboard allow_terminal→false
	// / remote-disable immediately refuses BOTH the reusable-standing AND the
	// single-use capability acquire path (finding 3 residual) — not just the
	// standing one, and without waiting for a daemon restart.
	allowTerminal bool
	// allowRemoteTerminalTakeover is the LIVE post-authorization writer-policy
	// gate. It shares standingMu with the sibling terminal gates and is read by
	// termsession at the Decide linearization point through the cmd adapter.
	allowRemoteTerminalTakeover bool
	// allowTerminalView is the LIVE [remote].allow_terminal_view gate — the
	// independent READ opt-in for the remote-sensitive (attach/resume) view
	// gate, which now DEFAULTS TRUE (session-attach design §3.2). Guarded by standingMu (like
	// allowTerminal, the sibling terminal gate) and hot-swapped by
	// ReloadAllowTerminalView so a dashboard flip takes effect on the live VIEW
	// gate (visibleSnapshot + the /ws/launch subscribe gate) without a daemon
	// restart. STRICTLY WEAKER than allowTerminal: it never relaxes the writer
	// path, only whether a remote caller may SEE / read-only-subscribe.
	allowTerminalView bool

	// sessionRevokeHook, when set (SetSessionRevokeHook), is invoked with a
	// device's FULL session key (its sha256 hash, NOT the 32-bit display
	// fingerprint) whenever THAT device revokes its OWN session via logout (F2).
	// The Server wires it to closeRemoteSensitiveViewersForDevice so a self-logout
	// tears down the device's already-open read-only sensitive viewers — the
	// self-revocation analogue of the admin per-device revoke. Keying on the full
	// hash (not the display prefix) closes the prefix-collision hole where a
	// device with a colliding fingerprint could disconnect another's viewer.
	// Guarded by hookMu; set once at wiring, read on the logout request path.
	hookMu            sync.Mutex
	sessionRevokeHook func(sessionKey string)
}

// NewRemoteController builds a RemoteController from RemoteOptions. It is
// Ready() only when the pairing secret hash and a non-empty Host allow-list are
// present (plan §4.6 — auth configured AND host allow-list active AND rate limit
// active; route-registry completeness is enforced by New()/the invariant test).
func NewRemoteController(o RemoteOptions) RemoteController {
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	sp := o.Session
	if sp.Now == nil {
		sp.Now = now
	}
	return &remoteController{
		hashedSecret:                o.HashedSecret,
		allowedHosts:                append([]string(nil), o.AllowedHosts...),
		sessions:                    remoteauth.NewSessionStore(sp),
		caps:                        remoteauth.NewCapabilityStore(o.CapabilityTTL, now),
		limiter:                     remoteauth.NewRateLimiter(o.RateLimitPerMin, o.RateLimitPerMin, now),
		allowTerminal:               o.AllowTerminal,
		allowTerminalView:           o.AllowTerminalView,
		allowRemoteTerminalTakeover: o.AllowRemoteTerminalTakeover,
		audit:                       o.Audit,
		now:                         now,
		csrf:                        map[string]string{},
		standingHash:                o.StandingTerminalSecretHash,
		standingEnabled:             o.StandingTerminalEnabled,
		standingAtRest:              o.StandingSecretAtRest,
		// A dedicated limiter for standing-secret acquire attempts so a brute
		// force against the reusable standing secret is throttled independently
		// of the pairing limiter. rate_limit_per_min=0 means "unlimited" for the
		// pairing limiter, but for THIS limiter 0 clamps to the default (6/min):
		// each attempt costs a 19 MiB argon2 compute + a synchronous audit write,
		// so an unlimited standing verifier would be a paired-device memory/IO
		// DoS (finding 5).
		standingLimiter: remoteauth.NewRateLimiter(clampStandingRate(o.RateLimitPerMin), clampStandingRate(o.RateLimitPerMin), now),
	}
}

// clampStandingRate maps the [remote] rate_limit_per_min knob onto the standing
// verifier's limiter: <=0 (the pairing limiter's "unlimited") becomes the
// secure default 6/min — the standing verifier is never unlimited.
func clampStandingRate(perMin int) int {
	if perMin <= 0 {
		return 6
	}
	return perMin
}

// Ready reports the §4.6 predicate this controller owns: auth configured
// (pairing secret hash present) AND a Host allow-list is active AND rate
// limiting is wired. A false result keeps the listener loopback-only.
func (c *remoteController) Ready() bool {
	return c.currentSecret() != "" && len(c.allowedHosts) > 0 && c.limiter != nil && c.sessions != nil
}

// currentSecret reads the live pairing-secret hash under the read lock.
func (c *remoteController) currentSecret() string {
	c.secretMu.RLock()
	defer c.secretMu.RUnlock()
	return c.hashedSecret
}

// ReloadSecret swaps the live pairing-secret hash so a dashboard rotate/enable
// takes effect on the RUNNING controller immediately — a freshly-minted QR then
// pairs without a daemon restart (previously the controller kept the hash it
// loaded at construction, so a rotated QR 401'd until restart). An empty string
// leaves the controller not-Ready (disable).
func (c *remoteController) ReloadSecret(hashed string) {
	c.secretMu.Lock()
	c.hashedSecret = hashed
	c.secretMu.Unlock()
}

// AllowedHosts returns the Host allow-list for browserGuard.
func (c *remoteController) AllowedHosts() []string { return append([]string(nil), c.allowedHosts...) }

// AllowTerminal reports whether remote terminal launch/control is enabled in
// the config snapshot this controller was built from.
func (c *remoteController) AllowTerminal() bool {
	c.standingMu.RLock()
	defer c.standingMu.RUnlock()
	return c.allowTerminal
}

// ReloadAllowTerminal hot-swaps the LIVE allow_terminal gate so a dashboard
// allow_terminal→false / remote-disable takes effect on the RUNNING controller
// immediately for BOTH acquire paths (finding 3 residual). The single-use
// authorize path reads this via the cmd adapter's allowTerminal() closure
// (wired to AllowTerminal()), and the fresh-launch gate reads it too, so a live
// "off" refuses all new terminal control without a restart.
func (c *remoteController) ReloadAllowTerminal(allow bool) {
	c.standingMu.Lock()
	c.allowTerminal = allow
	c.standingMu.Unlock()
}

// AllowRemoteTerminalTakeover reports the live
// [remote].allow_remote_terminal_takeover policy. The credential gate remains
// upstream and unchanged; this value is read only when an authorized requester
// reaches the manager's writer-policy decision.
func (c *remoteController) AllowRemoteTerminalTakeover() bool {
	c.standingMu.RLock()
	defer c.standingMu.RUnlock()
	return c.allowRemoteTerminalTakeover
}

// ReloadAllowRemoteTerminalTakeover hot-swaps the live post-authorization
// takeover policy without restarting the daemon. Existing leases are not
// revoked merely by toggling it; the next acquire reads the new value.
func (c *remoteController) ReloadAllowRemoteTerminalTakeover(allow bool) {
	c.standingMu.Lock()
	c.allowRemoteTerminalTakeover = allow
	c.standingMu.Unlock()
}

// AllowTerminalView reports whether [remote].allow_terminal_view is enabled on
// the LIVE controller — the independent READ opt-in that lets a remote caller
// SEE / read-only-subscribe to a remote-sensitive (attach/resume) session
// (session-attach design §3.2). It NEVER relaxes the writer path. Read under
// standingMu (co-located with the sibling allowTerminal gate).
func (c *remoteController) AllowTerminalView() bool {
	c.standingMu.RLock()
	defer c.standingMu.RUnlock()
	return c.allowTerminalView
}

// ReloadAllowTerminalView hot-swaps the LIVE allow_terminal_view gate so a
// dashboard flip takes effect on the RUNNING controller's VIEW gate immediately
// (no daemon restart). The dashboard boundary (visibleSnapshot + the
// /ws/launch subscribe gate) reads it via AllowTerminalView() on every request,
// so a →false flip refuses NEW remote-sensitive views at once (open viewers are
// torn down separately by the management handler). Mirrors ReloadAllowTerminal.
func (c *remoteController) ReloadAllowTerminalView(allow bool) {
	c.standingMu.Lock()
	c.allowTerminalView = allow
	c.standingMu.Unlock()
}

// SetSessionRevokeHook installs the callback invoked with a device's FULL
// session key (its sha256 hash) when that device revokes its own session
// (logout) — the Server wires it to closeRemoteSensitiveViewersForDevice so a
// self-logout tears down the device's open read-only sensitive viewers (F2). Set
// once at wiring, before serving.
func (c *remoteController) SetSessionRevokeHook(hook func(sessionKey string)) {
	c.hookMu.Lock()
	c.sessionRevokeHook = hook
	c.hookMu.Unlock()
}

// fireSessionRevokeHook invokes the installed session-revoke hook (if any) with
// a device's FULL session key. Nil-safe: no hook wired ⇒ no-op.
func (c *remoteController) fireSessionRevokeHook(sessionKey string) {
	c.hookMu.Lock()
	hook := c.sessionRevokeHook
	c.hookMu.Unlock()
	if hook != nil {
		hook(sessionKey)
	}
}

// Principal resolves the capability a request proves (plan §4.2/§4.3):
//   - no/invalid session cookie ⇒ Public (anonymous).
//   - valid session ⇒ View; but an unsafe method (mutation) without a matching
//     CSRF token is DOWNGRADED to Public (deny) — CSRF on cookie-auth mutations.
//   - a valid single-use execute capability header (bound to session+action,
//     consumed here) ⇒ Execute.
func (c *remoteController) Principal(r *http.Request) Capability {
	raw := sessionCookie(r)
	if raw == "" || c.sessions.Validate(raw) != nil {
		return CapabilityPublic
	}
	// The in-memory CSRF + capability maps are keyed by the session HASH (the
	// same identifier the store and List surface), never the raw bearer.
	h := remoteauth.HashSessionID(raw)
	if isUnsafeMethod(r.Method) && !c.csrfOK(h, r) {
		return CapabilityPublic
	}
	if tok := r.Header.Get(remoteExecuteHeader); tok != "" {
		action := r.Method + " " + r.URL.Path
		if c.caps.Consume(tok, h, action) {
			return CapabilityExecute
		}
	}
	return CapabilityView
}

// Routes returns the controller's own HTTP routes.
func (c *remoteController) Routes() []ExtraRoute {
	return []ExtraRoute{
		// Pairing is Public (the un-paired client must reach it); whoami/logout
		// are Public too (they read the caller's own cookie and are safe for an
		// anonymous caller — logout is a no-op without a session).
		{Pattern: "/api/remote/pair", Handler: c.handlePair, Capability: CapabilityPublic},
		{Pattern: "/api/remote/whoami", Handler: c.handleWhoami, Capability: CapabilityPublic},
		{Pattern: "/api/remote/logout", Handler: c.handleLogout, Capability: CapabilityPublic},
	}
}

// Sessions lists the live device sessions as metadata-only fingerprints
// (dashboard-management-surface plan §2F). The full session id never leaves the
// controller — SessionInfo.ID is carried for server-side revoke targeting only.
func (c *remoteController) Sessions() []remoteauth.SessionInfo { return c.sessions.List() }

// RevokeSession revokes ONE live session by its full id and tears down its
// capabilities + CSRF token. Instant, no restart (§C). Returns whether a live
// session was found and revoked.
func (c *remoteController) RevokeSession(id string) bool {
	found := false
	for _, s := range c.sessions.List() {
		if s.ID == id {
			found = true
			break
		}
	}
	// id is the session HASH (List surfaces it). Durable-first revoke: on a
	// persist failure report NOT revoked (fail-closed) rather than lie.
	if err := c.sessions.RevokeByHash(id); err != nil {
		return false
	}
	c.caps.RevokeSession(id)
	c.clearCSRF(id)
	return found
}

// RotateSessions revokes ALL live device sessions immediately (terminate-all),
// closing every revocation channel so open privileged sockets tear down.
// Instant, no restart (§C). Returns an error only when the durable rotate fails
// (fail-closed — the sessions stay live so the operator can retry).
func (c *remoteController) RotateSessions() error { return c.sessions.Rotate() }

// MintExecute issues a single-use execute capability for (sessionID, action)
// after a local approval step (plan §4.7). Exposed for the future
// `observer remote approve-execute` verb + tests; not reachable remotely.
func (c *remoteController) MintExecute(sessionID, action string) (string, error) {
	// Capabilities are keyed by the session HASH (matching Principal's Consume),
	// so callers may pass the RAW cookie value and it hashes consistently.
	return c.caps.Mint(remoteauth.HashSessionID(sessionID), action)
}

// TerminalControlAuthorizer is the subset of the remote substrate the cmd launch
// adapter needs to run the §4.δ terminal-control conjunction (via
// termlease.Authorize) and to mint at the LOCAL approve-execute step (§4.γ). It
// is exposed via a type assertion on RemoteController so the seam stays minimal
// and the fakes are unaffected. The concrete *remoteController implements it.
//
// Validate + ConsumeTerminalControl deliberately match termlease.SessionValidator
// and termlease.CapabilityConsumer so the same value can be passed as both to
// termlease.Authorize without a further adapter.
type TerminalControlAuthorizer interface {
	// Validate reports whether a raw device-session id is a live session
	// (termlease.SessionValidator).
	Validate(deviceSessionID string) error
	// ConsumeTerminalControl consumes a single-use terminal-control capability +
	// bound confirm for a raw device-session id + terminal handle
	// (termlease.CapabilityConsumer). A failed confirm consumes nothing.
	ConsumeTerminalControl(token, confirm, deviceSessionID, handle string) bool
	// MintTerminalControl mints a single-use terminal-control capability + bound
	// confirm for a device-session HASH (the id surfaced by Sessions()) + handle.
	// Returned only to the LOCAL approve-execute surface (§4.γ / §6).
	MintTerminalControl(deviceSessionHash, handle string) (token, confirm string, err error)
}

// StandingTerminalVerifier is the subset of the remote substrate the cmd launch
// adapter needs to run the OPT-IN standing terminal-control leg via
// termlease.AuthorizeStanding (standing-terminal-access §B). It is separate from
// TerminalControlAuthorizer so a fake that only exercises the single-use path is
// unaffected (additive, CLAUDE.md #6); the concrete *remoteController satisfies
// both. VerifyStandingTerminalControl deliberately matches
// termlease.StandingVerifier so the same value passes straight through.
type StandingTerminalVerifier interface {
	VerifyStandingTerminalControl(secret, deviceSessionID, handle string) bool
	// StandingTerminalGeneration returns a counter that advances on EVERY
	// standing-secret swap (mint/rotate/revoke/disable). The acquire adapter
	// captures it before verifying and re-checks it after the lease install,
	// so a verify that raced a revoke can never leave a surviving writer.
	StandingTerminalGeneration() uint64
}

// standingTerminalReloader is the hot-swap seam the dashboard management
// handlers use to make a standing mint/revoke take effect on the LIVE
// controller without a daemon restart. Type-asserted off the RemoteController so
// nil/fake controllers stay valid.
type standingTerminalReloader interface {
	ReloadStandingTerminalSecret(hashed string, enabled bool)
}

// allowTerminalReloader is the hot-swap seam for the LIVE allow_terminal gate
// (finding 3 residual). Type-asserted off the RemoteController so nil/fake
// controllers stay valid.
type allowTerminalReloader interface {
	ReloadAllowTerminal(allow bool)
}

// allowRemoteTerminalTakeoverReloader is the additive hot-swap seam for the
// live authenticated-remote takeover policy. Nil/fake controllers remain valid.
type allowRemoteTerminalTakeoverReloader interface {
	ReloadAllowRemoteTerminalTakeover(allow bool)
}

// allowTerminalViewer is the READ seam for the LIVE allow_terminal_view gate
// (session-attach design §3.2, Phase 4). It is type-asserted off the
// RemoteController — rather than added to the base interface — so nil/fake
// controllers (which deny by omission) stay valid, additive per CLAUDE.md #6.
// The concrete *remoteController satisfies it.
type allowTerminalViewer interface {
	AllowTerminalView() bool
}

// allowTerminalViewReloader is the hot-swap seam for the LIVE allow_terminal_view
// gate, mirroring allowTerminalReloader. Type-asserted off the RemoteController
// so nil/fake controllers stay valid.
type allowTerminalViewReloader interface {
	ReloadAllowTerminalView(allow bool)
}

// sessionRevokeHooker is the wiring seam (F2) letting the Server register a
// callback the controller fires on a device self-logout, so the device's open
// read-only sensitive viewers are torn down. Type-asserted off the
// RemoteController so nil/fake controllers stay valid (additive, CLAUDE.md #6).
type sessionRevokeHooker interface {
	SetSessionRevokeHook(func(sessionKey string))
}

// deviceSessionLifetimer is the READ seam (session-attach F1a/F1b) binding a
// privileged read-only viewer to its device session's lifetime. SessionLifetime
// returns the session's revocation channel (closed on revoke/rotate/eviction),
// the duration until its next TTL/idle expiry, and whether it is currently live
// — used BOTH to re-validate after registration (F1a: the drain-then-register
// race) AND to cancel the viewer on a later session-end (F1b). Type-asserted off
// the RemoteController so nil/fake controllers (loopback, which never expose a
// remote-sensitive viewer) stay valid, additive per CLAUDE.md #6.
type deviceSessionLifetimer interface {
	SessionLifetime(raw string) (revoked <-chan struct{}, until time.Duration, live bool)
}

// Validate reports whether a raw device-session id is currently valid — the
// termlease.SessionValidator leg of the §4.δ conjunction.
func (c *remoteController) Validate(deviceSessionID string) error {
	return c.sessions.Validate(deviceSessionID)
}

// SessionLifetime exposes the device-session lifetime binding a privileged
// read-only viewer uses to tear down when its session ends — revoked, rotated,
// or TTL/idle-expired (session-attach F1a/F1b). It delegates to the session
// store's additive accessor: the revocation channel (closed on
// revoke/rotate/eviction), the duration until the next TTL/idle expiry, and the
// current liveness. Read-only — it never refreshes the idle clock, so a bound
// viewer is not itself an activity that keeps a session alive.
func (c *remoteController) SessionLifetime(raw string) (<-chan struct{}, time.Duration, bool) {
	return c.sessions.SessionLifetime(raw)
}

// TouchDeviceSession refreshes a LIVE device session's idle clock on behalf of a
// currently-attached terminal websocket (the deviceSessionToucher seam), and
// reports whether the session was live. It is the deliberate, explicitly-named
// counterpart to the read-only SessionLifetime above — that method must keep its
// "bind to, never extend" contract for its own callers, so extending is a
// separate call rather than a change of its semantics. Extends only: it cannot
// resurrect a revoked/expired session, cross a generation bump, or push past the
// absolute TTL.
func (c *remoteController) TouchDeviceSession(raw string) bool {
	return c.sessions.TouchAttachedViewer(raw)
}

// ConsumeTerminalControl consumes a single-use terminal-control capability + its
// bound confirm for (raw device session, handle) — the termlease.CapabilityConsumer
// leg. The raw session id is hashed here (matching MintTerminalControl's stored
// hash and the hashing discipline everywhere else) so the raw bearer never
// enters a capability record. A failed confirm consumes nothing (§4.γ.2).
func (c *remoteController) ConsumeTerminalControl(token, confirm, deviceSessionID, handle string) bool {
	return c.caps.ConsumeTerminalControl(token, confirm, remoteauth.HashSessionID(deviceSessionID), handle)
}

// MintTerminalControl mints a single-use terminal-control capability + confirm
// bound to (device-session HASH, handle). The caller (the local approve-execute
// handler) passes the HASH surfaced by Sessions() — ConsumeTerminalControl hashes
// the raw cookie value to the same key, so mint (hash) and consume (raw→hash)
// agree.
func (c *remoteController) MintTerminalControl(deviceSessionHash, handle string) (string, string, error) {
	return c.caps.MintTerminalControl(deviceSessionHash, handle)
}

// ReloadStandingTerminalSecret swaps the live standing terminal-control secret
// hash + enabled gate so a dashboard mint/rotate/revoke takes effect on the
// RUNNING controller immediately — no daemon restart. A mint passes the fresh
// hash + true; a revoke passes "" + false (the standing verify then denies even
// if a stale hash lingered). Mirrors ReloadSecret's hot-swap discipline.
func (c *remoteController) ReloadStandingTerminalSecret(hashed string, enabled bool) {
	c.standingMu.Lock()
	c.standingHash = hashed
	c.standingEnabled = enabled
	// Bump the generation on EVERY swap so an in-flight verify that started
	// against the previous state can be rejected at lease-install time (the
	// finding-1 TOCTOU close). Callers order this BEFORE the writer kill.
	c.standingGen++
	c.standingMu.Unlock()
}

// StandingTerminalGeneration returns the current standing-access generation
// (bumped by every ReloadStandingTerminalSecret). The cmd acquire adapter
// captures it before the standing verify and rejects the lease after install if
// it moved — closing the verify→install TOCTOU against revoke/rotate.
func (c *remoteController) StandingTerminalGeneration() uint64 {
	c.standingMu.RLock()
	defer c.standingMu.RUnlock()
	return c.standingGen
}

// VerifyStandingTerminalControl is the termlease.StandingVerifier leg for the
// OPT-IN standing terminal-control path (standing-terminal-access §B). It is
// reached ONLY after termlease.AuthorizeStanding has already proven the rest of
// the §4.δ conjunction (remote-exposed + allow_terminal + valid device session
// + launch policy); this method owns the final credential leg and enforces, in
// order: the standing-enabled master gate, a per-device rate limit, and a
// constant-time argon2id compare of the presented secret against the hash at
// rest. Every attempt — success AND failure — is audited with the device
// fingerprint + handle (never the secret). The secret is NOT consumed (reusable
// until the operator revokes it).
func (c *remoteController) VerifyStandingTerminalControl(secret, deviceSessionID, handle string) bool {
	ok, _ := c.VerifyStandingTerminalControlReason(secret, deviceSessionID, handle)
	return ok
}

// VerifyStandingTerminalControlReason is the termlease.ReasoningStandingVerifier
// form of the leg above: identical verification, plus a classification of the
// denial that decides whether the DEVICE should destroy its saved secret.
//
//	malformed / bad_secret  → BadSecret   the value itself was judged and failed
//	standing_revoked        → Revoked     no secret exists at rest at all
//	standing_disabled       → Unavailable the gate is off, the secret survives
//	rate_limited            → Unavailable never reached the compare
//
// The revoked/disabled split (operator decision A2, 2026-07-25) is the reason
// this is not a plain boolean. A genuine revoke DELETES the hash at rest, while
// a temporary disable leaves it — but both reach the live controller as the
// same ("", false) reload, so the state in memory cannot tell them apart. The
// StandingSecretAtRest probe supplies the missing evidence, and it fails SAFE:
// with no probe wired, or any answer other than a definite "no secret exists",
// the denial stays transient. Getting this wrong in the permanent direction
// silently wipes a valid secret off the device and costs the operator a manual
// re-mint; getting it wrong in the transient direction costs one more denied
// attempt.
//
// The authorization outcome is unchanged in every case — this only classifies.
func (c *remoteController) VerifyStandingTerminalControlReason(secret, deviceSessionID, handle string) (ok bool, denial termlease.StandingDenial) {
	c.standingMu.RLock()
	enabled := c.standingEnabled
	hash := c.standingHash
	c.standingMu.RUnlock()

	fp := deviceFingerprint(deviceSessionID)
	if !enabled || hash == "" {
		// Nothing to compare against. WHICH refusal this is depends on whether a
		// standing secret still exists at rest (see StandingSecretAtRest).
		if c.standingSecretRevoked() {
			c.auditStanding(fp, handle, "deny", "standing_revoked")
			return false, termlease.StandingDenialRevoked
		}
		c.auditStanding(fp, handle, "deny", "standing_disabled")
		return false, termlease.StandingDenialUnavailable // never compared the secret
	}
	// Rate-limit standing attempts on ONE GLOBAL bucket, deliberately NOT the
	// device-session identity: a session id is freely rotatable (logout +
	// re-pair mints a fresh session and would mint a fresh bucket), so a
	// per-session key lets an attacker reset the throttle at will (finding 4).
	// A global bucket cannot be reset by identity churn; standing acquires are
	// rare (one per page refresh), so a shared 6/min budget never impedes
	// legitimate multi-device use. The audit row still carries the (rotatable)
	// device fingerprint — identity churn shows up as fingerprint churn there.
	if c.standingLimiter != nil && !c.standingLimiter.Allow("standing") {
		c.auditStanding(fp, handle, "deny", "rate_limited")
		return false, termlease.StandingDenialUnavailable // never compared the secret
	}
	raw, err := remoteauth.DecodeStandingSecret(secret)
	if err != nil {
		c.auditStanding(fp, handle, "deny", "malformed")
		return false, termlease.StandingDenialBadSecret // the presented value itself is unusable
	}
	if !remoteauth.VerifySecret(hash, raw) {
		c.auditStanding(fp, handle, "deny", "bad_secret")
		return false, termlease.StandingDenialBadSecret // judged and rejected
	}
	c.auditStanding(fp, handle, "ok", "standing")
	return true, termlease.StandingDenialNone
}

// standingSecretRevoked reports whether this node can PROVE that no standing
// terminal-control secret exists at rest — the evidence that turns "standing
// access denied" into a PERMANENT verdict on the presented secret.
//
// Fail-safe by construction: with no probe wired it returns false ("unknown"),
// so the denial stays transient and the device keeps its secret. The probe
// itself owes the same discipline (any stat error ⇒ report present).
func (c *remoteController) standingSecretRevoked() bool {
	if c.standingAtRest == nil {
		return false
	}
	return !c.standingAtRest()
}

// auditStanding records one standing terminal-control acquire attempt through
// the injected audit sink (metadata only — device fingerprint + handle +
// outcome, NEVER the secret). Best-effort; nil sink is a no-op.
func (c *remoteController) auditStanding(deviceFP, handle, decision, detail string) {
	if c.audit == nil {
		return
	}
	c.audit(RemoteAuditRecord{
		Kind:      "terminal_control_standing_acquire",
		SessionID: deviceFP, // device fingerprint (hash[:8]) — never the raw bearer
		Principal: "remote",
		Route:     handle, // the opaque terminal session handle
		Decision:  decision,
		Detail:    detail,
	})
}

type pairRequest struct {
	Secret string `json:"secret"`
}

type pairResponse struct {
	OK   bool   `json:"ok"`
	CSRF string `json:"csrf,omitempty"`
}

// pairErrorResponse is the MACHINE-READABLE pairing failure. Only the failures
// a device can act on get one (today: the device cap); every other failure
// stays a bare status so no new signal is handed to an unauthenticated caller.
type pairErrorResponse struct {
	OK    bool   `json:"ok"`
	Code  string `json:"code"`
	Error string `json:"error"`
}

// pairCodeTooManyDevices is returned (with 409 Conflict) when the device-session
// cap is full. It is deliberately distinguishable from the generic 503: the cap
// is fail-CLOSED by design — pairing must never silently evict a live device —
// so the only remedy is for the operator to revoke a device, and the UI can only
// say that if the failure is nameable.
const pairCodeTooManyDevices = "too_many_devices"

// sessionCookieMaxAge is the device cookie's Max-Age, in seconds, derived from
// the session store's OWN absolute TTL.
//
// Why it must exist at all: a Set-Cookie with neither Max-Age nor Expires is a
// BROWSER-SESSION cookie. Mobile Safari/Chrome drop those when the browsing
// session ends or a backgrounded tab is evicted — which they do within an hour
// or two — so a device with a perfectly live, durable, 48h server session
// stopped sending its bearer and every request 401'd behind a rendered shell.
// The server session was never the problem; the cookie carrying it was.
//
// Why the absolute TTL and not the idle window: the cookie is minted at the
// same instant as the session, and the session's absolute deadline is
// createdAt+TTL, so a Max-Age of TTL expires the cookie at exactly the moment
// the session it carries dies. Using the (shorter) idle window would recreate
// the bug in miniature — the cookie would expire while the session was still
// being refreshed. Clamped to >= 1 because a 0 Max-Age would omit the attribute
// and silently restore the session-cookie behaviour this fixes.
func (c *remoteController) sessionCookieMaxAge() int {
	secs := int(c.sessions.TTL().Seconds())
	if secs < 1 {
		secs = 1
	}
	return secs
}

// handlePair verifies the pairing secret (from the JSON BODY only — never a
// query param or subprotocol), rate-limited + constant-time, and on success
// mints a device session, sets the Secure/HttpOnly cookie, and returns a CSRF
// token. On failure it returns 401 with no timing signal beyond the argon2
// verify.
func (c *remoteController) handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := hostnameOnly(r.RemoteAddr)
	if !c.limiter.Allow(key) {
		c.auditPair(r, "fail", "rate_limited")
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return
	}
	var body pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil || body.Secret == "" {
		c.auditPair(r, "fail", "bad_request")
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	raw, err := remoteauth.DecodeSecret(body.Secret)
	if err != nil || !remoteauth.VerifySecret(c.currentSecret(), raw) {
		c.auditPair(r, "fail", "bad_secret")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	rawSession, err := c.sessions.Create()
	if err != nil {
		if errors.Is(err, remoteauth.ErrTooManySessions) {
			// Fail-closed by design: evicting a live session to make room would
			// sign out somebody else's device on nothing but possession of the
			// pairing secret. Say so instead, so the operator knows the exact
			// remedy (revoke a device) rather than reading "cannot create
			// session" off a 503.
			c.auditPair(r, "fail", "session_limit")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(pairErrorResponse{
				OK:    false,
				Code:  pairCodeTooManyDevices,
				Error: "the maximum number of devices are already paired — revoke one on the host dashboard (Remote → Paired devices), then pair again",
			})
			return
		}
		c.auditPair(r, "fail", "session_create")
		http.Error(w, "cannot create session", http.StatusServiceUnavailable)
		return
	}
	// The RAW token is the cookie bearer; the HASH keys the CSRF map + audit so
	// no bearer is ever stored server-side or logged.
	h := remoteauth.HashSessionID(rawSession)
	csrf, err := remoteauth.GenerateCSRFToken()
	if err != nil {
		_ = c.sessions.Revoke(rawSession)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	c.setCSRF(h, csrf)
	c.limiter.Reset(key)
	// MaxAge makes this a PERSISTENT cookie that survives a browser restart /
	// tab eviction; every other attribute is unchanged (HttpOnly + Secure +
	// SameSite=Strict + Path=/ are load-bearing security properties — see
	// sessionCookieMaxAge for why the lifetime is the session's own TTL).
	//
	// It is NOT re-issued on activity, deliberately. The server refreshes the
	// IDLE clock (SessionStore.Validate) but never the ABSOLUTE deadline, which
	// stays createdAt+TTL for the session's whole life. A cookie re-stamped on
	// activity would therefore drift PAST the deadline of the session it
	// carries and start presenting a bearer for a session the store has already
	// dropped — the mirror image of the bug being fixed, and pure noise. Minted
	// once, expiring exactly with its session, is the coherent choice; the
	// residual case (a cookie outliving an IDLE-expired session) surfaces as the
	// client's "pair this device again" prompt, not a broken dashboard.
	http.SetCookie(w, &http.Cookie{
		Name:     remoteSessionCookie,
		Value:    rawSession,
		Path:     "/",
		MaxAge:   c.sessionCookieMaxAge(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	c.auditSession(r, h, "session_paired", "ok", "paired")
	writeJSON(w, pairResponse{OK: true, CSRF: csrf})
}

// handleWhoami reports the caller's authentication state (Public — safe for an
// anonymous caller). Read-only.
func (c *remoteController) handleWhoami(w http.ResponseWriter, r *http.Request) {
	id := sessionCookie(r)
	authed := id != "" && c.sessions.Validate(id) == nil
	cap := CapabilityPublic
	csrf := ""
	if authed {
		cap = CapabilityView
		h := remoteauth.HashSessionID(id)
		csrf = c.csrfForSession(h)
	}
	writeJSON(w, map[string]any{"authenticated": authed, "capability": cap.String(), "csrf": csrf})
}

// handleLogout revokes the caller's session (+ its capabilities and CSRF token)
// and clears the cookie. A no-op for an anonymous caller.
func (c *remoteController) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if raw := sessionCookie(r); raw != "" {
		// F2a: fire the session-revoke hook ONLY when a LIVE session actually
		// existed. A logout with an unknown/expired cookie must close nothing —
		// look up liveness BEFORE the revoke (which idempotently no-ops for an
		// unknown session) so the hook fires exactly when a real session is
		// removed, never on a no-op logout that could otherwise be replayed to
		// disturb another device.
		wasLive := c.sessions.Validate(raw) == nil
		// Logout always clears the client cookie; the store cleanup is
		// durable-first but its error is non-fatal to the UX logout.
		_ = c.sessions.Revoke(raw)
		h := remoteauth.HashSessionID(raw)
		c.caps.RevokeSession(h)
		c.clearCSRF(h)
		if wasLive {
			// Close any read-only sensitive viewer this device left open (F2): a
			// self-logout must stop the device's attach/resume PTY stream NOW, the
			// same as an admin per-device revoke. Keyed by the FULL session hash
			// (h) — the value the viewer registered under — never the 32-bit
			// display fingerprint, so a prefix collision can never target another
			// device's viewer.
			c.fireSessionRevokeHook(h)
		}
		c.auditSession(r, h, "session_revoked", "ok", "logout")
	}
	http.SetCookie(w, &http.Cookie{Name: remoteSessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, map[string]any{"ok": true})
}

func (c *remoteController) csrfOK(id string, r *http.Request) bool {
	c.csrfMu.Lock()
	want := c.csrf[id]
	c.csrfMu.Unlock()
	got := r.Header.Get(remoteCSRFHeader)
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (c *remoteController) setCSRF(id, tok string) {
	c.csrfMu.Lock()
	c.csrf[id] = tok
	c.csrfMu.Unlock()
}

func (c *remoteController) csrfForSession(id string) string {
	c.csrfMu.Lock()
	defer c.csrfMu.Unlock()
	if tok := c.csrf[id]; tok != "" {
		return tok
	}
	tok, err := remoteauth.GenerateCSRFToken()
	if err != nil {
		return ""
	}
	c.csrf[id] = tok
	return tok
}

func (c *remoteController) clearCSRF(id string) {
	c.csrfMu.Lock()
	delete(c.csrf, id)
	c.csrfMu.Unlock()
}

func (c *remoteController) auditPair(r *http.Request, decision, detail string) {
	c.auditSession(r, "", "auth_failed", decision, detail)
}

func (c *remoteController) auditSession(r *http.Request, id, kind, decision, detail string) {
	if c.audit == nil {
		return
	}
	c.audit(RemoteAuditRecord{
		Kind: kind, SessionID: id, RemoteAddr: hostnameOnly(r.RemoteAddr),
		Route: r.URL.Path, Decision: decision, Detail: detail,
	})
}

// deviceFingerprint derives the short, non-reversible device identity used in
// the execute-tier audit rows from a RAW device-session id: the first 8 hex
// chars of its sha256 hash — byte-identical to remoteauth's SessionInfo
// fingerprint (and the value the local approve-execute step records), so the
// capability-lifecycle events correlate on ONE device token. Never the raw
// bearer. Empty in ⇒ empty out.
func deviceFingerprint(rawSession string) string {
	if rawSession == "" {
		return ""
	}
	h := remoteauth.HashSessionID(rawSession)
	if len(h) <= 8 {
		return h
	}
	return h[:8]
}

// deviceSessionKey derives the FULL, non-reversible device-session key from a
// RAW cookie: its sha256 hash (HashSessionID) — the SAME identity the session
// store keys on. It is the registry key for read-only sensitive viewers (F2b),
// distinct from deviceFingerprint (the 32-bit display prefix used only in audit
// rows / the management surface). Keying the viewer registry on the full hash —
// never the display prefix — closes the collision hole where a device whose
// fingerprint prefix matches another's could disconnect that other's viewer.
// Empty in ⇒ empty out (the owner-local path, never registered per-device).
func deviceSessionKey(rawSession string) string {
	if rawSession == "" {
		return ""
	}
	return remoteauth.HashSessionID(rawSession)
}

func sessionCookie(r *http.Request) string {
	ck, err := r.Cookie(remoteSessionCookie)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(ck.Value)
}

func isUnsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}
