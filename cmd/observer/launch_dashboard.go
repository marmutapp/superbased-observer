package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/remoteauth"
	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/termlease"
	"github.com/marmutapp/superbased-observer/internal/termrun"
	"github.com/marmutapp/superbased-observer/internal/termsession"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// launch_dashboard.go wires the dashboard's embedded web-terminal launch
// seam (dashboard.LaunchManager) to the terminal application service
// (internal/termsvc) over internal/termsession. termsvc owns the run identity,
// the fresh-launch authorization, and the correlation model; termsession owns
// only the PTY/viewer lifecycle. The dashboard never imports either — this
// adapter is the single boundary that translates the dashboard's server-derived
// specs into termsvc calls and maps errors onto the dashboard's sentinels
// (the same pattern as handoffRunner behind BuildHandoff).

// launchManagerAdapter bridges the terminal service + PTY manager to
// dashboard.LaunchManager.
type launchManagerAdapter struct {
	svc *termsvc.Service
	mgr *termsession.Manager
	// remoteAuthz runs the §4.δ authorization conjunction and mints the
	// unforgeable WriterGrant for a remote writer acquire. Nil until the
	// remote-execute tier is wired (Phase 4 §4.γ/§4.δ) — a nil authorizer fails
	// AcquireWriterRemote closed. The returned recheck func (nil for the
	// single-use capability path) is the standing-path TOCTOU close: it is run
	// AFTER the lease install and must return true for the lease to survive —
	// false means the standing secret was revoked/rotated while the (argon2)
	// verify was in flight, so the just-installed lease is torn down.
	remoteAuthz func(dashboard.RemoteWriterRequest) (termlease.WriterGrant, func() bool, error)
	// attachAudit records the metadata-only terminal_attach spawn-audit row (F4,
	// session-attach design §3.5) for attach-socket launches. Nil when no DB is
	// wired (auditing disabled). Built in newLaunchManager over the SAME
	// SpawnAuditKind vocabulary the dashboard resume handler uses.
	attachAudit func(runID, tool, handle string)
}

// newLaunchManager builds the embedded-terminal launch manager, or returns a
// nil seam (+ no-op close) when [handoff].allow_dashboard_launch is false —
// the dashboard treats a nil LaunchManager as the disabled state (503 + the
// button hidden). The returned close func stops the reaper and kills every
// live session; wire it into the command's teardown.
//
// It is a THIN WRAPPER over buildTerminalStack (the one-owner shared builder,
// attach_standalone.go): the gate decides only whether the dashboard SURFACE is
// wired, not whether the Manager exists. `observer start` builds the stack
// directly (via buildTerminalSurfaces) so the attach socket can serve off the
// same stack independent of this gate; `observer dashboard` uses this wrapper.
//
// Fresh-agent launch (F1) is a SEPARATE, default-off opt-in resolved into the
// termsvc.Policy from [terminal] + [terminal.launch]; it never widens the
// handoff-continue consent (which stays gated by allow_dashboard_launch — the
// gate that decides whether the launch SURFACE is wired at all).
func newLaunchManager(cfg config.Config, database *sql.DB, logger *slog.Logger) (dashboard.LaunchManager, dashboard.TerminalStatusProvider, func(), error) {
	if !cfg.Handoff.AllowDashboardLaunch {
		return nil, nil, func() {}, nil
	}
	stack, err := buildTerminalStack(cfg, database, logger)
	if err != nil {
		return nil, nil, func() {}, err
	}
	if stack == nil {
		// No in-process PTY backend (logged inside buildTerminalStack): the
		// dashboard treats a nil LaunchManager as the honest disabled state.
		return nil, nil, func() {}, nil
	}
	return stack.launchManager(), stack.status, stack.close, nil
}

// newSpawnAuditSink builds the metadata-only spawn-audit closure (F4,
// session-attach design §3.5) that persists one remote_audit row per terminal
// spawn through the ONE store seam (store.InsertRemoteAudit). kind is a
// dashboard.SpawnAuditKind value resolved once at wiring; an empty kind or a nil
// DB yields a nil sink (auditing disabled). Metadata ONLY — the run identity,
// the tool label, and the opaque handle (correlation, like the lease-audit
// Route) — NEVER argv, env, or terminal content. Best-effort + bounded; a failed
// audit never affects the spawn.
func newSpawnAuditSink(database *sql.DB, kind string) func(runID, tool, handle string) {
	if database == nil || kind == "" {
		return nil
	}
	st := store.New(database)
	return func(runID, tool, handle string) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = st.InsertRemoteAudit(ctx, store.RemoteAuditEvent{
			Kind:      kind,
			SessionID: runID,   // the run identity minted at spawn (never a secret)
			Principal: "local", // an attach socket is owner-only (AF_UNIX 0600)
			Route:     handle,  // correlate on the terminal handle like every terminal event
			Decision:  "ok",
			Detail:    tool,
		})
	}
}

// applyTerminalBounds maps the [terminal] knobs onto the termsession Options
// (0/empty leaves the termsession default). IdleTimeout is validated at config
// load, so a parse failure here is only a defensive log.
func applyTerminalBounds(opts *termsession.Options, tc config.TerminalConfig, logger *slog.Logger) {
	opts.MaxConcurrent = tc.MaxConcurrent
	opts.RingBytes = tc.RingBytes
	opts.MaxSubscribers = tc.MaxSubscribers
	if tc.IdleTimeout != "" {
		if d, err := time.ParseDuration(tc.IdleTimeout); err == nil {
			opts.IdleTimeout = d
		} else {
			logger.Warn("terminal: ignoring invalid idle_timeout", "value", tc.IdleTimeout, "err", err)
		}
	}
}

// terminalLaunchPolicy resolves the fresh-launch authorization from config.
// [terminal].enabled gates the terminal-wide surface, so fresh launch requires
// BOTH it and the [terminal.launch].allow_fresh_agent opt-in.
func terminalLaunchPolicy(tc config.TerminalConfig) termsvc.Policy {
	return termsvc.Policy{
		AllowFresh:          tc.Enabled && tc.Launch.AllowFreshAgent,
		AllowedTools:        tc.Launch.AllowedTools,
		AllowedProjectRoots: tc.Launch.AllowedProjectRoots,
	}
}

// --- dashboard.LaunchManager implementation ---

func (a *launchManagerAdapter) Create(spec dashboard.LaunchSpec) (string, error) {
	res, err := a.svc.LaunchHandoff(context.Background(), termsvc.HandoffRequest{
		Tool:        spec.Subcommand, // the launcher verb doubles as the tool label here
		Subcommand:  spec.Subcommand,
		SessionID:   spec.SessionID,
		Carry:       spec.Carry,
		FromMessage: spec.FromMessage,
		Rows:        spec.Rows,
		Cols:        spec.Cols,
	})
	if err != nil {
		return "", mapLaunchErr(err)
	}
	return res.Handle, nil
}

func (a *launchManagerAdapter) CreateFresh(spec dashboard.FreshLaunchSpec) (string, error) {
	res, err := a.svc.LaunchFresh(context.Background(), termsvc.FreshRequest{
		Tool:        spec.Tool,
		Subcommand:  spec.Subcommand,
		ProjectRoot: spec.ProjectRoot,
		Rows:        spec.Rows,
		Cols:        spec.Cols,
	})
	if err != nil {
		return "", mapFreshErr(err)
	}
	return res.Handle, nil
}

// CreateResume spawns a NATIVE resume of a closed session (session-attach
// design Phase 3) through the terminal application service, which enforces the
// SAME fresh-launch policy as CreateFresh (a dashboard-initiated Execute
// respects [terminal.launch] — unlike the owner-only CLI attach socket). It
// returns the opaque handle AND the durable run id; errors map onto the
// dashboard sentinels through mapFreshErr (fresh-disabled / tool-not-allowed /
// project-root-denied + the termsession spawn errors).
func (a *launchManagerAdapter) CreateResume(spec dashboard.ResumeLaunchSpec) (string, string, error) {
	res, err := a.svc.LaunchResume(context.Background(), termsvc.ResumeRequest{
		Tool:            spec.Tool,
		Subcommand:      spec.Subcommand,
		ProjectRoot:     spec.ProjectRoot,
		SourceSessionID: spec.SessionID,
		ExtraArgs:       spec.ExtraArgs,
		Rows:            spec.Rows,
		Cols:            spec.Cols,
	})
	if err != nil {
		return "", "", mapFreshErr(err)
	}
	return res.Handle, res.RunID, nil
}

// CreateSetup spawns a fixed, server-derived local operator setup command
// (e.g. the one-time Tailscale operator grant) DIRECTLY through the PTY manager
// — bypassing termsvc (AI-launch policy) and terminal_run identity, with no OOB
// channel. Env is the daemon's own os.Environ() (a setup command needs a sane
// PATH/TERM) with internal child vars stripped PLUS the OBSERVER_DAEMON_CHILD
// marker via setupChildEnv, so this non-termsvc path still satisfies the "every
// daemon child carries the marker" invariant (finding: marker completeness). The
// session is SpecSetup → local-writer-only.
func (a *launchManagerAdapter) CreateSetup(spec dashboard.SetupSpec) (string, error) {
	handle, err := a.mgr.Create(termsession.Spec{
		Kind:       termsession.SpecSetup,
		SetupArgv:  spec.Argv,
		SetupLabel: spec.Label, // keys the setup single-flight (one PTY per kind)
		Env:        setupChildEnv(),
		Rows:       spec.Rows,
		Cols:       spec.Cols,
	})
	if err != nil {
		return "", mapLaunchErr(err)
	}
	return handle, nil
}

func (a *launchManagerAdapter) Subscribe(handle string) (dashboard.LaunchSubscription, error) {
	sub, err := a.mgr.Subscribe(handle)
	if err != nil {
		return nil, err
	}
	return sub, nil // *termsession.Subscription satisfies dashboard.LaunchSubscription
}

// SubscribeRemote is the remote-principal viewer path: it refuses a SpecSetup
// (privileged, local-only) session at the manager seam so a paired remote device
// can never READ a setup PTY's output (a typed sudo password / login URL).
func (a *launchManagerAdapter) SubscribeRemote(handle string) (dashboard.LaunchSubscription, error) {
	sub, err := a.mgr.SubscribeRemote(handle)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// IsSetupSession reports whether a handle is a SpecSetup (privileged, local-only)
// session — the dashboard redacts these from remote snapshots and refuses their
// remote termination.
func (a *launchManagerAdapter) IsSetupSession(handle string) bool {
	return a.mgr.IsSetupSession(handle)
}

// IsRemoteSensitiveSession reports whether a handle is a remote-deny-by-default
// run — an external `observer <tool> --attach` session (KindAttach) OR a native
// resume of a real closed transcript (KindResume) — resolved from the run
// identity termsvc minted at spawn and the shared termrun.IsRemoteSensitiveKind
// table (dispatch on run SHAPE, never a tool name — CLAUDE.md #3/#5). The
// dashboard denies a remote-exposed caller the snapshot row and the websocket
// for such a handle by default (§3.2). Because termsvc retains byMeta through
// ExitLinger (F1), this keeps classifying an exited-but-lingering handle as
// sensitive for as long as the Manager can still replay its bytes. Unknown/reaped
// handle ⇒ false.
func (a *launchManagerAdapter) IsRemoteSensitiveSession(handle string) bool {
	kind, _, ok := a.svc.KindForHandle(handle)
	return ok && termrun.IsRemoteSensitiveKind(kind)
}

func (a *launchManagerAdapter) Unsubscribe(sub dashboard.LaunchSubscription) {
	if ts, ok := sub.(*termsession.Subscription); ok {
		a.mgr.Unsubscribe(ts)
	}
}

func (a *launchManagerAdapter) AcquireWriterLocal(handle string) (dashboard.LaunchWriter, error) {
	l, err := a.mgr.AcquireWriterLocal(handle)
	if err != nil {
		return nil, err
	}
	return l, nil // *termsession.WriterLease satisfies dashboard.LaunchWriter
}

// termsvcLaunchPolicy resolves the termlease.LaunchPolicy leg of the §4.δ
// conjunction: a remote writer may target ONLY a live, termsvc-tracked terminal
// run. A setup session (the one-time local operator grant) is created directly
// through the manager with no termsvc run, so it has no RunIDForHandle mapping
// and is refused here — in addition to the manager's own local-writer-only pin.
// An unknown/dead handle is likewise refused (fail closed). The richer tool/
// project-root allow-list refinement is a documented Phase-4 follow-up; the run
// was already policy-checked at launch, so a live run is the minimal applicable
// policy.
type termsvcLaunchPolicy struct{ svc *termsvc.Service }

func (p termsvcLaunchPolicy) Allowed(handle string) bool {
	if p.svc == nil {
		return false
	}
	_, ok := p.svc.RunIDForHandle(handle)
	return ok
}

// wireRemoteExecute installs the §4.δ remote-writer authorizer onto the adapter
// once the [remote] substrate exists. authz owns the device-session +
// capability stores (the SAME instances the local approve-execute mint uses),
// so a capability minted at approve-execute is the one consumed here.
// allowTerminal reads the live remote.allow_terminal gate; RemoteExposed comes
// from the request's boundary-resolved provenance flag — NEVER a client body
// field. Until this is called, AcquireWriterRemote stays fail-closed.
func (a *launchManagerAdapter) wireRemoteExecute(authz dashboard.TerminalControlAuthorizer, allowTerminal func() bool) {
	policy := termsvcLaunchPolicy{svc: a.svc}
	a.remoteAuthz = func(req dashboard.RemoteWriterRequest) (termlease.WriterGrant, func() bool, error) {
		// A standing terminal-control secret (opt-in §B) rides the SAME
		// acquire-writer cap field, distinguished by its collision-free prefix.
		// Route it to the AuthorizeStanding leg — the IDENTICAL §4.δ conjunction
		// with a reusable-standing-secret verify replacing the single-use
		// capability consume. This is a boundary branch on CREDENTIAL SHAPE, not
		// a second websocket path (CLAUDE.md #3).
		if remoteauth.IsStandingSecret(credOf(req)) {
			standing, ok := authz.(dashboard.StandingTerminalVerifier)
			if !ok {
				return termlease.WriterGrant{}, nil, termlease.ErrCapabilityRejected
			}
			// TOCTOU close (finding 1): capture the standing generation BEFORE
			// the verify. Every revoke/rotate bumps it BEFORE killing writers,
			// so if it moved by the time the lease is installed, the verify
			// raced an admin transition and the lease must not survive.
			gen := standing.StandingTerminalGeneration()
			sr := termlease.AuthorizeRequest{
				Handle:          req.Handle,
				DeviceSessionID: req.DeviceSessionID,
				RemoteExposed:   req.RemoteExposed,
				AllowTerminal:   allowTerminal(),
			}
			setCred(&sr, credOf(req)) // the standing secret rides the credential field
			grant, err := termlease.AuthorizeStanding(sr, authz, policy, standing)
			// The install-time recheck fences ALL THREE lifecycle races that can
			// land during the slow argon2 verify: the SECRET lifecycle
			// (generation bumped by mint/rotate/revoke/disable), the SESSION
			// lifecycle (device revoke / logout / rotate / revoke-all do NOT
			// bump the generation, so re-validate the session directly — finding
			// 1 residual), AND the allow_terminal GATE (an allow_terminal→false
			// flip + its RevokeAllRemoteWriters sweep can complete BEFORE this
			// not-yet-installed lease exists, so re-read the LIVE gate — finding
			// 3 residual: without this a verify that read allow_terminal=true at
			// gate time could leave a surviving writer past the disable). Any of
			// the three moving means the acquire raced an admin transition and
			// the just-installed lease must not survive.
			recheck := func() bool {
				return standing.StandingTerminalGeneration() == gen && authz.Validate(req.DeviceSessionID) == nil && allowTerminal()
			}
			return grant, recheck, err
		}
		// authz satisfies BOTH termlease.SessionValidator (Validate) and
		// termlease.CapabilityConsumer (ConsumeTerminalControl); pass it as both.
		// The single-use consume is atomic (no slow argon2), so its race window
		// is tiny — but a session revoke OR an allow_terminal→false flip can
		// still land between the gate read and the lease install. Re-validate
		// the session AND re-read the LIVE allow_terminal gate at install time
		// so neither a since-revoked/logged-out device nor a just-disabled
		// terminal toggle can leave a surviving writer here (finding 1 + finding
		// 3 residuals, uniform with the standing path). allow_terminal is read
		// LIVE at gate time below AND fenced again in the recheck.
		grant, err := termlease.Authorize(termlease.AuthorizeRequest{
			Handle:          req.Handle,
			DeviceSessionID: req.DeviceSessionID,
			CapabilityToken: req.CapabilityToken,
			Confirm:         req.Confirm,
			RemoteExposed:   req.RemoteExposed, // boundary-resolved provenance
			AllowTerminal:   allowTerminal(),   // LIVE allow_terminal (finding 3 residual)
		}, authz, policy, authz)
		recheck := func() bool { return authz.Validate(req.DeviceSessionID) == nil && allowTerminal() }
		return grant, recheck, err
	}
}

// wireRemoteExecuteTier connects the remote-execute authorizer to the launch
// manager once BOTH the [remote] substrate and the PTY launcher exist. It is a
// no-op — leaving AcquireWriterRemote fail-closed (ErrLaunchExecuteUnavailable) —
// when either is absent. Both `observer dashboard` and `observer start` call it
// with the identical assembly, so the two commands share one authorization path.
func wireRemoteExecuteTier(cfg config.Config, launchMgr dashboard.LaunchManager, remoteCtrl dashboard.RemoteController) {
	a, ok := launchMgr.(*launchManagerAdapter)
	if !ok || a == nil {
		return
	}
	authz, ok := remoteCtrl.(dashboard.TerminalControlAuthorizer)
	if !ok {
		return
	}
	// Read allow_terminal LIVE from the controller (finding 3 residual), not the
	// startup cfg snapshot: a dashboard allow_terminal→false / remote-disable
	// hot-swaps the controller's flag (ReloadAllowTerminal), so BOTH the
	// single-use and standing acquire paths immediately refuse without a restart.
	// remoteCtrl.AllowTerminal() returns the construction value until the first
	// hot-swap, so the initial behaviour is identical to the cfg snapshot.
	_ = cfg // allow_terminal now comes from the live controller, not this snapshot
	a.wireRemoteExecute(authz, remoteCtrl.AllowTerminal)
}

// AcquireWriterRemote runs the single §4.δ authorization conjunction over the
// request-derived inputs (via the injected authorizer that owns the capability
// store + session validator + launch policy + live allow_terminal), mints the
// unforgeable WriterGrant, and acquires the remote writer lease. Until the
// remote-execute tier is wired (a nil authorizer), it fails closed.
func (a *launchManagerAdapter) AcquireWriterRemote(req dashboard.RemoteWriterRequest) (dashboard.LaunchWriter, error) {
	if a.remoteAuthz == nil {
		return nil, dashboard.ErrLaunchExecuteUnavailable
	}
	grant, recheck, err := a.remoteAuthz(req)
	if err != nil {
		return nil, err
	}
	l, err := a.mgr.AcquireWriterRemote(req.Handle, grant)
	if err != nil {
		return nil, err
	}
	// Standing-path TOCTOU close (finding 1): the reusable-secret verify runs
	// OUTSIDE any lock (argon2 is slow by design), so a revoke/rotate can land
	// between the verify's snapshot and this install. Re-check the standing
	// generation NOW — after the lease exists, so the admin kill sweep and this
	// check overlap: either the kill sees the installed lease (and revokes it),
	// or the generation moved (and we tear it down here). No input can have
	// ridden the lease yet — it has not been returned to the bridge.
	if recheck != nil && !recheck() {
		l.Release()
		return nil, termlease.ErrCapabilityRejected
	}
	return l, nil
}

func (a *launchManagerAdapter) Close(handle string) { a.mgr.Close(handle) }

// RevokeAllRemoteWriters delegates to the PTY manager's remote-only global kill
// (leaving the owner-local loopback writer untouched) through the ONE
// termsession revocation funnel — the admin-transition seam the dashboard drives
// for remote disable / rotate / allow_terminal→false (§8.1 item 8).
func (a *launchManagerAdapter) RevokeAllRemoteWriters(reason string) int {
	return a.mgr.RevokeAllRemoteWriters(reason)
}

// RevokeRemoteWriterByHolder delegates to the manager's single-device kill,
// matching on the unified holder key (grant.Holder() = sha256(device-session)
// [:8]). Used by a device-session revoke. The parameter is the holder key, not a
// raw device fingerprint — it is already the hashed, truncated holder identity.
func (a *launchManagerAdapter) RevokeRemoteWriterByHolder(holderKey, reason string) bool {
	return a.mgr.RevokeRemoteWriterByHolder(holderKey, reason)
}

func (a *launchManagerAdapter) Snapshot() []dashboard.LaunchInfo {
	live := a.mgr.Snapshot()
	// GC retained run classification (termsvc.byMeta) for handles the Manager no
	// longer tracks (reaped past ExitLinger). This is the Snapshot-enrichment
	// prune half of the F1 exit-linger fix: EndRunByHandle keeps byMeta alive so
	// the remote sensitivity gates stay honest through linger; this drops it once
	// the handle is truly gone. The Manager's Snapshot is the ONE live view, so
	// the adapter that already reads it is the one owner that prunes.
	liveHandles := make(map[string]struct{}, len(live))
	for _, s := range live {
		liveHandles[s.ID] = struct{}{}
	}
	a.svc.PruneEndedHandles(liveHandles)
	out := make([]dashboard.LaunchInfo, 0, len(live))
	for _, s := range live {
		info := dashboard.LaunchInfo{
			ID:           s.ID,
			Subcommand:   s.Subcommand,
			SessionID:    s.SessionID,
			CreatedAt:    s.CreatedAt,
			Attached:     s.Viewers > 0,
			Viewers:      s.Viewers,
			WriterHolder: s.WriterHolder,
			Setup:        s.Setup,
			Exited:       s.Exited,
			ExitCode:     s.ExitCode,
		}
		if runID, ok := a.svc.RunIDForHandle(s.ID); ok {
			info.RunID = runID
			// An attach run carries no source session at spawn — it is
			// correlated to an observer session later via the OOB flow. Fill the
			// established correlation ONLY when the PTY spec supplied no session
			// id, so a handoff row's own SessionID is never clobbered.
			if info.SessionID == "" {
				if sid, ok := a.svc.SessionForRun(runID); ok {
					info.SessionID = sid
				}
			}
		}
		// Label the run Kind + Tool from the run identity the service minted at
		// spawn so the dashboard can gate "Jump in" on run SHAPE (attach) rather
		// than a tool name.
		if kind, tool, ok := a.svc.KindForHandle(s.ID); ok {
			info.Kind = string(kind)
			info.Tool = tool
		}
		out = append(out, info)
	}
	return out
}

// leaseAuditKind maps a termsession writer-lease transition to its typed
// remote_audit event kind (Phase-4 execute-tier audit lifecycle, plan §8.1).
// Table-driven so a new lease kind is one row, never a nested branch.
func leaseAuditKind(k termsession.LeaseEventKind) string {
	switch k {
	case termsession.LeaseAcquired:
		return "terminal_writer_acquire"
	case termsession.LeaseReleased:
		return "terminal_writer_release"
	case termsession.LeaseRevoked:
		return "terminal_writer_revoke"
	case termsession.LeaseTakenOver:
		return "terminal_local_takeover"
	default:
		return "terminal_writer_" + string(k)
	}
}

// leaseHolderPrincipal classifies a lease holder ("local" vs a remote device
// fingerprint) into a capability class for the audit principal column — never
// the raw identity.
func leaseHolderPrincipal(holder string) string {
	if holder == "" || holder == "local" {
		return "local"
	}
	return "remote"
}

// newLeaseAuditSink is the FUNC SEAM (CLAUDE.md #1/#2) bridging termsession's
// OnLeaseEvent tap to typed, node-local remote_audit rows through the ONE store
// seam (store.InsertRemoteAudit). termsession stays store-free: it emits only
// the content-free LeaseEvent; this cmd-side closure persists it. Metadata
// ONLY — the terminal handle, the holder fingerprint (already truncated by
// termlease, never the raw bearer), a coarse reason, and the transition kind;
// NEVER a capability, confirm, grant, terminal byte, command, or env. A nil DB
// yields a nil sink (auditing disabled). The insert is best-effort + bounded (a
// failed audit never affects a lease transition) and synchronous so persisted
// rows preserve transition order (remote_audit orders by autoincrement id).
func newLeaseAuditSink(database *sql.DB) func(termsession.LeaseEvent) {
	if database == nil {
		return nil
	}
	st := store.New(database)
	return func(ev termsession.LeaseEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// A privileged SpecSetup PTY is LOCAL-ONLY for its whole lifecycle, but
		// remote_audit is a View-tier route paired remote devices can read. Its
		// opaque handle must therefore never land there (a remote viewer could
		// otherwise learn the handle + its lease-activity timing). Store an opaque
		// "setup:<label>" route instead — the fact of the setup op is legitimate
		// audit (row kept), only the handle leakage is closed, and the label (e.g.
		// "tailscale-login") carries MORE local-owner forensic value than the
		// ephemeral handle. Redacted AT EMIT, so it is uniform for every reader
		// (local + remote). FIX A(i), second adversarial review 2026-07-16.
		route := ev.Handle // the opaque terminal session handle
		if ev.Setup {
			route = "setup"
			if ev.Label != "" {
				route = "setup:" + ev.Label
			}
		}
		_ = st.InsertRemoteAudit(ctx, store.RemoteAuditEvent{
			TS:        ev.At,
			Kind:      leaseAuditKind(ev.Kind),
			SessionID: ev.Holder, // "local" or a device-session fingerprint — never the raw id
			Principal: leaseHolderPrincipal(ev.Holder),
			Route:     route,
			Decision:  "ok",
			Detail:    ev.Reason, // coarse, non-sensitive transition reason
		})
	}
}

// credOf returns the credential the remote writer-acquire request carries in
// its capability field — a single-use capability token OR (opt-in §B) a
// standing terminal-control secret. The acquire boundary branches on the
// credential SHAPE (remoteauth.IsStandingSecret), so both ride one field.
func credOf(req dashboard.RemoteWriterRequest) string { return req.CapabilityToken }

// setCred puts the credential onto a termlease authorize request's capability
// field (the standing-secret path; AuthorizeStanding reads it as the secret).
func setCred(r *termlease.AuthorizeRequest, cred string) { r.CapabilityToken = cred }

// mapLaunchErr translates termsession's sentinels onto the dashboard's so the
// HTTP handler can pick an honest status without importing termsession.
func mapLaunchErr(err error) error {
	switch {
	case errors.Is(err, termsession.ErrTooManySessions):
		return dashboard.ErrLaunchTooMany
	case errors.Is(err, termsession.ErrPlatformUnsupported):
		return dashboard.ErrLaunchUnsupported
	case errors.Is(err, termsession.ErrSetupInFlight):
		return dashboard.ErrLaunchSetupInFlight
	default:
		return err
	}
}

// mapFreshErr translates the termsvc fresh-launch authorization sentinels (and
// the underlying termsession spawn errors) onto the dashboard's.
func mapFreshErr(err error) error {
	switch {
	case errors.Is(err, termsvc.ErrFreshLaunchDisabled):
		return dashboard.ErrLaunchFreshDisabled
	case errors.Is(err, termsvc.ErrToolNotAllowed):
		return dashboard.ErrLaunchToolNotAllowed
	case errors.Is(err, termsvc.ErrProjectRootDenied):
		return dashboard.ErrLaunchProjectRootDenied
	default:
		return mapLaunchErr(err)
	}
}
