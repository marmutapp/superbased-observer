package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/marmutapp/superbased-observer/internal/attachsock"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/dashboard"
	"github.com/marmutapp/superbased-observer/internal/remotenotify"
	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termrun"
	"github.com/marmutapp/superbased-observer/internal/termsession"
	"github.com/marmutapp/superbased-observer/internal/termstatus"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// attach_standalone.go owns the daemon's ONE terminal application service +
// PTY manager stack (terminalStack) and the two surfaces that consume it — the
// dashboard embedded-launch manager AND the owner-only session-attach socket —
// as INDEPENDENT capabilities off a single shared stack.
//
// This is the one-owner seam (CLAUDE.md #4): the Manager/Service is constructed
// exactly once per daemon, in buildTerminalStack above the per-surface
// constructors, so an
// attach-launched session and a dashboard-launched one share the manager's
// OnExit run-recording, the spawn audit sink, the lease model, and the status
// feed. Neither surface ever stands up a second Manager.
//
// Enablement is per-surface, not coupled: the attach socket serves whenever
// [terminal.attach].enabled is true, and the dashboard launch manager is wired
// whenever [handoff].allow_dashboard_launch is true — each independent of the
// other. Both still require an in-process PTY backend (termsession.PTYSupported).

// terminalStack is the single termsession.Manager + termsvc.Service the daemon
// builds, plus the shared status provider, spawn-audit sink, and teardown func.
// Its launchManager() and attachHost() methods derive the two surfaces over the
// SAME svc + mgr, so both funnel exits through one OnExit closure and arbitrate
// writer leases through one lease model.
type terminalStack struct {
	svc    *termsvc.Service
	mgr    *termsession.Manager
	status dashboard.TerminalStatusProvider
	// attachAudit records the metadata-only terminal_attach spawn-audit row (F4,
	// session-attach design §3.5). Nil when no DB is wired (auditing disabled).
	// Built over the SAME SpawnAuditKind vocabulary the dashboard resume handler
	// uses so the two paths write identically shaped rows.
	attachAudit func(runID, tool, handle string)
	// hub bridges the correlation feed to the attach socket (resilient-attach
	// Layer 1): per-run correlated-session delivery + the daemon-wide live view
	// the double-spawn guard reads. Built over the SAME feed the Service
	// publishes correlation/exit events on.
	hub *attachHub
	// resumableRuns maps each rediscovered orphan session id → ALL of its
	// startup-eligible PREDECESSOR run ids, newest first (the attach runs this
	// daemon rediscovered as orphans at startup: no recorded end + a correlated
	// session id). Membership validates a daemon-death AUTO-resume target (a run
	// that recorded its end is NOT resumable-by-restart); the run-id LIST lets the
	// supersede stamp EVERY eligible predecessor by id — not just the newest — so
	// older same-session orphans can't be re-offered on a later restart (round-4
	// multi-orphan finding), while never touching the fresh replacement (finding:
	// wrong-run supersede). Empty/nil when no DB is wired.
	resumableRuns map[string][]string
	// attachDir is the attach socket's owner-only directory. The attach host
	// takes a durable cross-process resume flock here (H3). Empty disables the
	// flock layer (no DB).
	attachDir string
	// supersedeResumed stamps rediscovered orphan rows end_reason='resumed' after
	// a successful auto-resume spawn, keyed by the PREDECESSOR run ids (H2 +
	// finding: wrong-run supersede + round-4 multi-orphan). Nil when no DB is wired.
	supersedeResumed func(runIDs []string)
	// resumeAuthority is the DURABLE store-backed double-spawn authority (round-5
	// finding 1): it reports whether the store already holds a LIVE run correlated
	// to a resume target session — the authority the attach host's resume conflict
	// check consults alongside the in-memory hub guard, so a dashboard resume (or
	// any dashboard-spawned run that correlates to an AI session), which never
	// rides the attach hub's feed and never takes the resume flock, still blocks a
	// duplicate attach resume. Nil when no DB is wired (the in-memory guard stands
	// alone). The attach host passes the target session's rediscovered predecessor
	// run ids to exclude, so the authority never self-blocks a crash-orphan
	// auto-resume by matching its own predecessor (review finding 1).
	resumeAuthority func(sessionID string, excludeRunIDs []string) bool
	// reclaimOnInput is [terminal.attach].reclaim_on_input (Feature 1): the
	// native-terminal writer-reclaim capability, resolved once at build time and
	// handed to the attach host. Default TRUE.
	reclaimOnInput bool
	// close stops the status-hub reaper and kills every live session; wire it
	// into the command's teardown BEFORE the DB is closed (start.go relies on
	// defer-LIFO ordering to run it first).
	close func()
}

// buildTerminalStack constructs the shared terminal stack exactly once (see
// terminalStack). It returns (nil, nil) — not an error — when the OS has no
// in-process PTY backend (a native-Windows daemon): a nil stack is the honest
// "disabled" state both surfaces treat as unavailable, and the message is logged
// here so callers don't each duplicate it. A future ConPTY backend flips
// termsession.PTYSupported() to true and re-enables the stack with no caller
// change.
//
// Fresh-agent launch (F1) is a SEPARATE, default-off opt-in resolved into the
// termsvc.Policy from [terminal] + [terminal.launch]; building the stack never
// widens it (that stays gated on [terminal.launch].allow_fresh_agent).
func buildTerminalStack(cfg config.Config, database *sql.DB, logger *slog.Logger) (*terminalStack, error) {
	// Leave the stack unbuilt on an OS with no in-process PTY backend (a
	// native-Windows daemon). A nil stack is the honest "disabled" state: the
	// dashboard "Launch here" button is hidden and the attach socket is not
	// served, rather than either failing on use.
	if !termsession.PTYSupported() {
		logger.Info("embedded terminal disabled — no in-process PTY backend on this OS (run the daemon under WSL/Linux); dashboard launch + session-attach are unavailable, handoff-doc migration is unaffected")
		return nil, nil
	}
	binPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("buildTerminalStack: resolve observer binary: %w", err)
	}

	// The status event feed (F4 prereq): a bounded in-process fan-out the
	// terminal service publishes launch/exit/correlation events to. Consumers
	// (F4 status) subscribe; it never back-pressures the producer.
	feed := termfeed.New(termfeed.Options{})

	// Resilient-attach Layer 1: the correlation hub is built EARLY (before svc)
	// because svc now wires the hub's DIRECT exit seam (hub.NotifyExit) into
	// termsvc.Options.OnRunExit — the reliable, feed-independent signal the hub
	// keys liveness / flock release / tombstones off (round-4). It subscribes to
	// the feed for advisory correlation only; the exit-driven correctness reaches
	// it through EndRunByHandle, not this subscription.
	hub := newAttachHub(feed, logger)

	// svc is assigned after the manager is built, but the manager's OnExit
	// closure references it by variable so the daemon-observed exit signal can
	// mark the run ended (and publish an exit event). The pre-registration
	// fast-exit gap — a child that exits before termsvc.launch installs the
	// handle→run mapping, so OnExit finds nothing — is closed inside termsvc.launch
	// by an ExitStatus reconcile (wired below), so the exit is recorded (and
	// NotifyExit fired) exactly once regardless of ordering.
	var svc *termsvc.Service

	// The untrusted OSC hint tap (F3): one bounded scanner per PTY handle,
	// turning OSC 133/633/title/BEL hints into TrustHint status events + durable
	// command boundaries. runID is resolved through svc by variable.
	scanHub := newTerminalScanHub(feed, store.New(database), func(handle string) (string, bool) {
		if svc == nil {
			return "", false
		}
		return svc.RunIDForHandle(handle)
	}, logger)

	opts := termsession.Options{
		Logger: logger,
		// Startup seed closes the construction-before-controller-wiring window.
		// wireRemoteExecuteTier replaces this with the live controller reader
		// before either dashboard listener starts serving.
		AllowRemoteTakeover: func() bool { return cfg.Remote.AllowRemoteTerminalTakeover },
	}
	applyTerminalBounds(&opts, cfg.Terminal, logger)
	// Remote writer-lease lifetimes (§4.α.2c) come from [remote]; 0 falls back
	// to the termsession defaults (5m idle / 30m hard cap).
	if cfg.Remote.WriterLeaseIdleMinutes > 0 {
		opts.WriterLeaseIdle = time.Duration(cfg.Remote.WriterLeaseIdleMinutes) * time.Minute
	}
	if cfg.Remote.WriterLeaseMaxMinutes > 0 {
		opts.WriterLeaseMax = time.Duration(cfg.Remote.WriterLeaseMaxMinutes) * time.Minute
	}
	opts.OnOutput = scanHub.Observe
	// Metadata-only writer-lease audit tap (Phase-4 execute-tier audit
	// lifecycle, plan §8.1). termsession stays store-free — this cmd-side FUNC
	// SEAM maps each content-free LeaseEvent to a typed remote_audit row.
	opts.OnLeaseEvent = newLeaseAuditSink(database)
	notifier := newRemoteNotifier(cfg, logger)
	// DEFERRED (review B6): this OnExit closure runs on the manager's own exit
	// goroutine and calls svc.EndRunByHandle (a DB write) with a background
	// context. At daemon shutdown that write can, in principle, race the DB
	// close. This is PRE-EXISTING behaviour shared with every terminal launch
	// (handoff/fresh/attach all funnel exits through here) — decoupling attach
	// from the dashboard gate did NOT introduce it; an attach session is just
	// another run recorded through the same seam. A proper fix (track these
	// writes in a WaitGroup the shutdown path joins, or gate them on a shutdown
	// flag) is a broader launch-manager change out of scope here; noted so the
	// race has a documented home.
	opts.OnExit = func(se termsession.SessionExit) {
		if svc != nil {
			svc.EndRunByHandle(context.Background(), se.Handle, se.ExitCode)
		}
		scanHub.Drop(se.Handle)
		if notifier != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			if nerr := notifier.Notify(ctx, remotenotify.Event{
				Type:       remotenotify.EventSessionFinished,
				SessionID:  se.SessionID,
				Tool:       se.Subcommand,
				Subcommand: se.Subcommand,
				ExitCode:   se.ExitCode,
				Time:       se.At,
			}); nerr != nil {
				logger.Warn("remote notify: session-finished delivery failed", "err", nerr, "session", se.SessionID)
			}
		}
	}

	mgr := termsession.NewManager(opts)
	launcher := &ptyLauncher{mgr: mgr, binPath: binPath, feed: feed, logger: logger}
	svc = termsvc.New(termsvc.Options{
		Policy:   terminalLaunchPolicy(cfg.Terminal),
		Recorder: termRunRecorder{st: store.New(database)},
		Launcher: launcher,
		Feed:     feed,
		Logger:   logger,
		// Close the pre-registration exit gap: launch() reconciles a just-spawned
		// handle against the authoritative Manager exit status the instant it
		// installs the mapping (Manager stays the one owner of exit truth).
		ExitStatus: mgr.ExitStatus,
		// The DIRECT per-run exit seam: EndRunByHandle fires this once per run so
		// the attach hub's correctness never rides the lossy status feed.
		OnRunExit: hub.NotifyExit,
	})
	// Wire the OOB run->session correlation seam (P2-1): when the launcher
	// wrapper announces the child's agent session id on the trusted OOB channel,
	// drainOOB establishes the link through the service (bySession) so a live
	// attach run's Snapshot carries a session id and "Jump in" can match it.
	// Assigned after svc exists (launcher is svc's Launcher, so the two are
	// mutually referential — the same shape as the OnExit/scanHub closures).
	launcher.correlate = svc.Correlate

	// F4 agent-status hub: fuses the feed (OSC hints + OOB/lifecycle) with
	// termsession output-recency/exit into a per-run status. Gated by
	// [terminal.status].enabled; when off, no provider is wired (endpoints 503).
	var statusProvider dashboard.TerminalStatusProvider
	stopStatus := func() {}
	if cfg.Terminal.Status.Enabled {
		hub := newTerminalStatusHub(
			feed,
			mgr.LastActivity,
			mgr.ExitStatus,
			svc.RunIDForHandle,
			svc.HandleForRun,
			func() []string {
				live := mgr.Snapshot()
				ids := make([]string, 0, len(live))
				for _, s := range live {
					ids = append(ids, s.ID)
				}
				return ids
			},
			termstatus.Thresholds{},
		)
		statusProvider = hub
		stopStatus = hub.Stop
	}

	// Resilient-attach Layer 1: rediscover this daemon's orphaned attach runs so
	// an auto-resume target can be validated (the hub itself was built early,
	// above, so svc could wire its direct exit seam). Additive over the SAME
	// store the rest of the stack already uses.
	resumable := rediscoverResumableSessions(database, logger)

	// H3/H2 durable-resume wiring. attachDir is the attach socket's owner-only
	// directory (the flock lives beside the socket); supersedeResumed stamps the
	// old orphan row after a successful auto-resume, keyed by the PREDECESSOR run
	// id so the fresh replacement (which can correlate to the same session via OOB
	// first) is never touched. Both are gated on a wired DB.
	var attachDir string
	var supersedeResumed func(runIDs []string)
	var resumeAuthority func(sessionID string, excludeRunIDs []string) bool
	if database != nil {
		attachDir = filepath.Dir(attachSocketPath(cfg.Observer.DBPath))
		st := store.New(database)
		supersedeResumed = func(runIDs []string) {
			if len(runIDs) == 0 {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := st.StampResumedByRunIDs(ctx, runIDs); err != nil && logger != nil {
				logger.Warn("attach: stamp resumed orphans failed", "err", err, "runs", runIDs)
			}
		}
		// The DURABLE double-spawn authority (round-5 finding 1): refuse a resume
		// whenever the store already holds a LIVE run correlated to the target
		// session — including a dashboard resume that never rides the attach hub's
		// feed and never takes the resume flock. The in-memory hub guard remains
		// the fast path; this catches everything persisted. The confidence gate is
		// termrun.MinLinkConfidence so a weak heuristic guess never fabricates a
		// conflict (the same ABSTAIN rule the hub applies). Fail OPEN on a query
		// error (return false = no conflict): the hub guard + the H3 flock still
		// apply, and blocking every resume on a transient DB error is the worse
		// failure — matching the best-effort, benign-direction posture of the rest
		// of the resilient-attach path.
		resumeAuthority = func(sessionID string, excludeRunIDs []string) bool {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			live, err := st.LiveRunForSessionExcluding(ctx, sessionID, termrun.MinLinkConfidence, excludeRunIDs)
			if err != nil {
				if logger != nil {
					logger.Warn("attach: live-run authority query failed — falling back to the in-memory guard + flock", "err", err, "session", sessionID)
				}
				return false
			}
			return live
		}
	}

	return &terminalStack{
		svc:              svc,
		mgr:              mgr,
		status:           statusProvider,
		attachAudit:      newSpawnAuditSink(database, dashboard.SpawnAuditKind(termrun.KindAttach)),
		hub:              hub,
		resumableRuns:    resumable,
		attachDir:        attachDir,
		supersedeResumed: supersedeResumed,
		resumeAuthority:  resumeAuthority,
		reclaimOnInput:   cfg.Terminal.Attach.ReclaimOnInput,
		close: func() {
			// H2: stamp every LIVE attach run 'daemon_shutdown' SYNCHRONOUSLY,
			// BEFORE mgr.Shutdown() kills the PTYs (whose async OnExit would
			// otherwise race the DB close) and before start.go's defer-LIFO closes
			// the DB. The durable stamp — NOT the racy ended_at — is what makes
			// those runs deterministically resumable-by-restart on the next
			// daemon. Best-effort: a failure only risks a shutdown orphan not
			// being offered (the honest resume hint still fires).
			if database != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				st := store.New(database)
				if n, err := st.StampLiveAttachRunsShutdown(ctx); err != nil {
					if logger != nil {
						logger.Warn("attach: stamp live attach runs at shutdown failed", "err", err)
					}
				} else if n > 0 && logger != nil {
					logger.Info("attach: stamped live attach runs for resumable-on-restart", "count", n)
				}
				// Sibling hygiene sweep: stamp the LIVE non-attach kinds
				// (resume/fresh/handoff) too, so a graceful shutdown never strands
				// them as ended_at-NULL "live" rows in the runs-history view. This is
				// disjoint from the attach sweep above (kind != 'attach') and does not
				// touch the attach resume-offer path.
				if n, err := st.StampLiveNonAttachRunsShutdown(ctx); err != nil {
					if logger != nil {
						logger.Warn("attach: stamp live non-attach runs at shutdown failed", "err", err)
					}
				} else if n > 0 && logger != nil {
					logger.Info("attach: stamped live non-attach runs at shutdown", "count", n)
				}
				cancel()
			}
			stopStatus()
			hub.stop()
			mgr.Shutdown()
		},
	}, nil
}

// rediscoverResumableSessions builds the AUTO-resume rediscovery map on daemon
// startup (resilient-attach Layer 1, part 2). It queries terminal_run for
// KindAttach runs that have NO recorded end AND a correlated agent session id,
// and returns a map from each such session id to its PREDECESSOR run id. The
// gate — no recorded end — is exactly the "ended by DAEMON DEATH, not child-exit"
// distinction: a child that exits on its own is recorded via termsvc's OnExit →
// EndRunByHandle (ended_at set), so it is NOT resumable-by-restart; a run whose
// daemon was killed before it could record the exit keeps ended_at NULL, so it
// IS an orphan a returning client may auto-resume. The membership half drives the
// validation predicate; the run-id value lets the auto-resume supersede stamp the
// exact predecessor (never the fresh replacement — finding: wrong-run supersede).
// A nil DB (or a query error) yields an empty map, so validation fails closed and
// nothing is superseded. Read-only; the map is a snapshot at startup (in-memory
// on the stack).
func rediscoverResumableSessions(database *sql.DB, logger *slog.Logger) map[string][]string {
	if database == nil {
		return map[string][]string{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runs, err := store.New(database).ListTerminalRuns(ctx, 500)
	if err != nil {
		if logger != nil {
			logger.Warn("attach: rediscover resumable sessions failed — auto-resume validation will reject all targets", "err", err)
		}
		return map[string][]string{}
	}
	set := resumableSessionSet(runs)
	if logger != nil && len(set) > 0 {
		logger.Info("attach: rediscovered orphaned attach sessions eligible for auto-resume", "count", len(set))
	}
	return set
}

// resumableSessionSet is the pure rediscovery gate over a terminal-run history:
// it maps the agent session id of each resumable-by-restart session to the LIST
// of that session's eligible KindAttach PREDECESSOR run ids (newest first), for
// the KindAttach runs that carry a correlated session id. Split out (no DB) so
// the resumability distinction is unit-tested directly.
//
// The value is the FULL list of run ids (not just the newest, and not a bare
// bool) so the auto-resume supersede stamps EVERY eligible predecessor by id
// (StampResumedByRunIDs) rather than by session — a by-session stamp would also
// hit the FRESH replacement run once it correlates to the same session via OOB
// (finding: wrong-run supersede), while stamping only the newest would leave
// OLDER same-session orphans (historical duplicates / prior stamp failures)
// offerable on every future restart (round-4 multi-orphan finding). Newest-first
// input order (ListTerminalRuns) is preserved in each list, so element 0 remains
// the offered predecessor.
//
// Resumability (review finding H2) uses the DURABLE end_reason, not the racy
// ended_at:
//   - end_reason 'resumed'    → already superseded by a prior auto-resume; NEVER
//     re-offer (excluded even though ended_at may be NULL).
//   - end_reason 'child_exit' → a natural exit; NOT resumable.
//   - end_reason 'daemon_shutdown' → stamped synchronously at graceful shutdown;
//     ALWAYS resumable, even if a racing OnExit later set ended_at.
//   - end_reason ” (running or crash orphan) → resumable only while ended_at is
//     NULL (a crash left no record; a live run is legitimately mid-flight and a
//     resume of it is caught downstream by the live-session guard + flock).
func resumableSessionSet(runs []store.TerminalRunSummary) map[string][]string {
	set := make(map[string][]string)
	for _, r := range runs {
		if r.Kind != string(termrun.KindAttach) {
			continue // only owner-terminal attach runs are auto-resumable
		}
		if r.BestSessionID == "" {
			continue // no correlated session ⇒ nothing to resume
		}
		switch r.EndReason {
		case store.EndReasonResumed, store.EndReasonChildExit:
			continue // superseded, or a clean child exit — not resumable-by-restart
		}
		// Accumulate EVERY eligible orphan for the session (newest first, since
		// the input is newest-first), so a successful resume can supersede all of
		// them, not merely the newest (round-4 multi-orphan finding).
		if r.EndedAt == nil || r.EndReason == store.EndReasonDaemonShutdown {
			set[r.BestSessionID] = append(set[r.BestSessionID], r.RunID)
		}
	}
	return set
}

// launchManager wraps the shared stack in the dashboard.LaunchManager adapter.
// The adapter's remote-execute authorizer stays nil until wireRemoteExecuteTier
// installs it (fail-closed until then). The returned adapter drives the SAME
// svc + mgr the attach host does.
func (s *terminalStack) launchManager() *launchManagerAdapter {
	return &launchManagerAdapter{svc: s.svc, mgr: s.mgr, attachAudit: s.attachAudit}
}

// attachHost builds the session-attach socket Host over the shared stack's
// terminal service + PTY manager — the SAME *termsvc.Service and
// *termsession.Manager the dashboard launch manager drives (session-attach
// design Phase 1). Reusing them (rather than a parallel PTY stack) means an
// attach-launched session's exit is recorded by the shared OnExit closure, its
// writer lease arbitrates against dashboard writers through the one lease model,
// its spawn writes the same metadata-only audit row, and it appears in the same
// Snapshot / status feed as a dashboard launch. Crucially this reaches the
// attach host DIRECTLY off the stack, so it serves even when the dashboard
// launch manager is disabled ([handoff].allow_dashboard_launch = false).
func (s *terminalStack) attachHost() attachsock.Host {
	// Derive the two resume seams from the ONE rediscovery map: a non-empty list
	// is the auto-resume validation predicate; the run-id LIST resolves EVERY
	// predecessor the supersede stamps by exact id — all eligible same-session
	// orphans, never the fresh replacement (finding: wrong-run supersede +
	// round-4 multi-orphan). Reading a nil map is safe (zero value, empty slice).
	resumable := func(sessionID string) bool { return len(s.resumableRuns[sessionID]) > 0 }
	predecessors := func(sessionID string) ([]string, bool) {
		ids := s.resumableRuns[sessionID]
		return ids, len(ids) > 0
	}
	return newAttachHost(s.svc, s.mgr, s.attachAudit).
		withResume(s.hub, resumable).
		withDurableResume(s.attachDir, s.supersedeResumed, predecessors).
		withResumeAuthority(s.resumeAuthority).
		withReclaim(s.reclaimOnInput)
}

// terminalSurfaces is the decoupled per-surface result start.go consumes: the
// dashboard launch manager (nil unless [handoff].allow_dashboard_launch), its
// status provider, and the attach-socket host (nil unless
// [terminal.attach].enabled) — all derived from ONE shared terminalStack, plus
// its single teardown func. A nil field is the honest "surface disabled" state
// for that capability alone; the OTHER surface is unaffected.
type terminalSurfaces struct {
	launchMgr    dashboard.LaunchManager
	launchStatus dashboard.TerminalStatusProvider
	attachHost   attachsock.Host
	// mgr is the concrete one-owner session manager (nil when no stack was
	// built). Exposed so start.go can register post-construction hooks that
	// need the concrete type (SetOnStandingLocalTakeover) — the dashboard
	// itself keeps talking through the LaunchManager interface.
	mgr   *termsession.Manager
	close func()
}

// buildTerminalSurfaces constructs the shared terminal stack ONCE and derives
// the requested surfaces from it, decoupled: the attach host is wired whenever
// [terminal.attach].enabled is true and the launch manager whenever
// [handoff].allow_dashboard_launch is true, independently. When neither surface
// is requested (or there is no PTY backend) it returns a zero-value result with
// a no-op close and NO stack is built. The single source of the per-surface
// gating truth — shared by `observer start` and its tests.
func buildTerminalSurfaces(cfg config.Config, database *sql.DB, logger *slog.Logger) (terminalSurfaces, error) {
	surf := terminalSurfaces{close: func() {}}
	// Nothing requested → build no stack (and no PTY reaper goroutine).
	if !cfg.Handoff.AllowDashboardLaunch && !cfg.Terminal.Attach.Enabled {
		return surf, nil
	}
	stack, err := buildTerminalStack(cfg, database, logger)
	if err != nil {
		return surf, err
	}
	if stack == nil {
		// No PTY backend (logged inside buildTerminalStack) — both surfaces stay
		// honestly disabled.
		return surf, nil
	}
	surf.close = stack.close
	surf.mgr = stack.mgr
	// Assign the launch manager only when the gate is on so the field stays a
	// nil interface (not a non-nil interface over a nil pointer) when disabled —
	// the dashboard keys off that nil to hide the button, and
	// wireRemoteExecuteTier's type assertion no-ops on it.
	if cfg.Handoff.AllowDashboardLaunch {
		surf.launchMgr = stack.launchManager()
		surf.launchStatus = stack.status
	}
	if cfg.Terminal.Attach.Enabled {
		surf.attachHost = stack.attachHost()
	}
	return surf, nil
}
