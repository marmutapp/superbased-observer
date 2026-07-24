package termsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termrun"
)

// Errors surfaced by the fresh-launch authorization gate. Handoff launches do
// not consult these — they are gated by [handoff].allow_dashboard_launch at the
// cmd layer (the existing narrow consent, deliberately untouched).
var (
	// ErrFreshLaunchDisabled — [terminal.launch].allow_fresh_agent is false.
	ErrFreshLaunchDisabled = errors.New("termsvc: fresh-agent launch is disabled (set [terminal.launch].allow_fresh_agent)")
	// ErrToolNotAllowed — the tool is not in [terminal.launch].allowed_tools.
	ErrToolNotAllowed = errors.New("termsvc: tool is not in the fresh-launch allow-list")
	// ErrNoLauncher — the service was constructed without a Launcher.
	ErrNoLauncher = errors.New("termsvc: no launcher configured")
	// ErrAttachToolRequired — LaunchAttachable was given an empty Tool.
	ErrAttachToolRequired = errors.New("termsvc: attach launch requires a tool")
	// ErrAttachSubcommandRequired — LaunchAttachable was given an empty Subcommand.
	ErrAttachSubcommandRequired = errors.New("termsvc: attach launch requires a subcommand")
	// ErrAttachDirNotAbsolute — LaunchAttachable was given a non-absolute Dir.
	ErrAttachDirNotAbsolute = errors.New("termsvc: attach launch dir must be absolute")
)

// Policy is the resolved fresh-launch authorization, built from the operator's
// [terminal.launch] block. The zero value denies every fresh launch.
type Policy struct {
	// AllowFresh is the master opt-in for non-handoff launches.
	AllowFresh bool
	// AllowedTools is the allow-list of launchable tool NAMES a fresh launch
	// may start. Empty = deny-all.
	AllowedTools []string
	// AllowedProjectRoots is the operator-configured directory allow-list a
	// fresh launch's project_root is validated against (canonicalized).
	AllowedProjectRoots []string
}

// toolAllowed reports whether tool is in the fresh-launch allow-list.
func (p Policy) toolAllowed(tool string) bool {
	for _, t := range p.AllowedTools {
		if t == tool {
			return true
		}
	}
	return false
}

// reasonChildExit is the durable end-reason termsvc stamps for a natural
// process exit (or a never-spawned run). It mirrors store.EndReasonChildExit —
// the store is the canonical owner of the value; termsvc keeps a local copy
// rather than importing the store (it deliberately speaks only termrun's pure
// types). The guarded recorder write means this never downgrades a
// graceful-shutdown/resumed stamp (review finding H2).
const reasonChildExit = "child_exit"

// RunRecorder persists run identity + scored correlations. It is satisfied by
// a cmd adapter over *store.Store; termsvc speaks internal/termrun's pure
// types so it never imports the store.
type RunRecorder interface {
	// RecordRun persists a newly minted run identity.
	RecordRun(ctx context.Context, run termrun.Run) error
	// EndRun records a run's exit (ended_at + exit code) and, when the run has
	// no durable end-reason stamped yet, the given reason (e.g. "child_exit" for
	// a natural exit). The reason write is GUARDED by the recorder so a natural
	// exit never downgrades a graceful-shutdown/resumed stamp (review finding
	// H2).
	EndRun(ctx context.Context, runID string, endedAt time.Time, exitCode int, reason string) error
	// RecordCorrelation upserts a scored run→session correlation (MAX-upgrade).
	RecordCorrelation(ctx context.Context, c termrun.Correlation) error
}

// LaunchRequest is the fully server-derived spawn request the Service hands the
// Launcher. Every field is derived from validated inputs; the client never
// supplies argv, paths, or the correlation nonce.
type LaunchRequest struct {
	// RunID is the durable run identity minted for this launch.
	RunID string
	// Tool is the target tool name (for the OOB channel + status feed).
	Tool string
	// Subcommand is the observer launcher verb (from the capability registry).
	Subcommand string
	// Kind is handoff or fresh.
	Kind termrun.Kind
	// Dir is the canonical project root for a fresh launch ("" = default cwd).
	Dir string
	// SessionID / Carry / FromMessage are the handoff continuation params
	// (unset for a fresh launch).
	SessionID   string
	Carry       string
	FromMessage int
	// Rows / Cols are the initial PTY size (0 = OS default).
	Rows uint16
	Cols uint16
	// CorrelationToken is the raw OOB nonce handed to the child out of band so
	// it can be echoed back on the trusted channel; never persisted in clear.
	CorrelationToken string
	// ExtraEnv is additional environment entries ("KEY=VALUE") the Launcher
	// appends to the child's environment at spawn. It carries the attach
	// launcher's proxy-routing variables (session-attach design §6 decision #7:
	// attach children are proxy-routed by default) so a daemon-spawned attach
	// session captures tokens through :8820 exactly like a bare launch. Empty
	// for dashboard fresh/handoff launches.
	ExtraEnv []string
	// ExtraArgs are additional, server-derived argv tokens the Launcher appends
	// to the inner `observer <Subcommand>` launch AFTER the subcommand. The
	// attach path uses them to express the routing escape hatch
	// (`--no-proxy-route`), `--proxy`/`--config` overrides, and the operator's
	// trailing tool args (`-- --model X`) in ARGV — the env-only approach is a
	// no-op because the inner launcher self-routes regardless of env (B2/B3).
	// Empty for dashboard fresh/handoff launches.
	ExtraArgs []string
}

// Launcher spawns a PTY-backed launcher and returns its opaque handle. It is
// satisfied by a cmd adapter over *termsession.Manager (plus the OOB FD
// plumbing). termsvc owns identity/authorization; the Launcher owns the PTY.
type Launcher interface {
	Spawn(req LaunchRequest) (handle string, err error)
}

// runMeta captures the per-handle run facts the Service already knows at spawn
// time — the run Kind and target Tool — so a Snapshot consumer can label a live
// PTY handle without a store read. Populated beside byHandle on spawn success;
// RETAINED (not dropped) when the run ends so the remote-sensitivity gates keep
// classifying an exited-but-lingering handle (F1). endedAt is the exit stamp:
// zero while the run is live, set by EndRunByHandle at exit. A zero endedAt is
// what makes PruneEndedHandles race-proof — a still-live entry is never pruned
// even against a STALE live-handle set (R2-2) — and it gates the grace-based
// aging that eventually GCs a long-dead entry.
type runMeta struct {
	Kind termrun.Kind
	Tool string
	// dir is the canonical, already-validated project root this run was launched
	// with ("" when launched with the default cwd). It is the SAME value the run
	// hashed into ProjectRootHash at spawn — retained in memory (never persisted;
	// terminal_runs stores only the hash) so the dashboard's project panel can
	// resolve a token to its root without a store read. Retained past exit like
	// the rest of runMeta and read through Service.ProjectRoot.
	dir     string
	endedAt time.Time
}

// endedHandleGrace is how long a run's retained classification metadata (byMeta)
// outlives the run's exit before it may be garbage-collected. It MUST exceed
// BOTH (a) the termsession Manager's ExitLinger default (30s — the window during
// which an exited PTY is still subscribable, so its classification must survive)
// AND (b) any plausible staleness of the live-handle set a PruneEndedHandles
// caller passes. The inequality is grace(90s) > ExitLinger(30s) + snapshot
// staleness: even if a snapshot captured BEFORE a handle registered is used to
// prune AFTER that handle registered-and-exited, the just-set endedAt is far
// younger than the grace, so the entry is retained and stays classified through
// its whole linger (R2-2). Over-classification (retaining a bit past linger) is
// safe — nothing can subscribe to a reaped handle; under-classification was the
// confidentiality hole this closes.
const endedHandleGrace = 90 * time.Second

// endedHandleGCBound bounds how many ended (retained) byMeta entries accumulate
// before EndRunByHandle opportunistically sheds the ones older than
// endedHandleGrace. It keeps byMeta bounded on a daemon driven ONLY through
// repeated attach sessions with no dashboard Snapshot polling (the usual
// PruneEndedHandles caller) — R2-7. It is a soft high-water mark, not a hard cap:
// entries younger than the grace are always kept (their linger classification is
// still load-bearing), so the live size settles near the number of runs that
// ended within the last grace window.
const endedHandleGCBound = 256

// Service is the terminal application service. One instance per daemon.
type Service struct {
	policy   Policy
	rec      RunRecorder
	launcher Launcher
	feed     *termfeed.Feed // optional; nil disables the status event feed
	logger   *slog.Logger   // optional; nil disables the no-mapping debug log
	now      func() time.Time
	// exitStatus, when set, is the authoritative "has this handle already
	// exited?" query (termsession.Manager.ExitStatus). launch() consults it the
	// instant it installs the handle→run mapping, to close the PRE-REGISTRATION
	// exit gap: a child that exits between Spawn returning and the mapping being
	// installed fires the Manager's OnExit → EndRunByHandle with NO mapping yet
	// (a silent no-op), so the exit would otherwise never be recorded. Nil
	// disables the reconcile (tests / no Manager).
	exitStatus func(handle string) (exited bool, code int, ok bool)
	// onRunExit, when set, is the DIRECT, reliable per-run exit seam fired
	// synchronously from EndRunByHandle the moment a run's exit is recorded —
	// exactly once per run (EndRunByHandle dedupes by handle). It is what the
	// attach hub keys its liveness / flock-release / tombstone CORRECTNESS off,
	// independent of the lossy status feed (resilient-attach round-4). Because
	// EVERY end path funnels through EndRunByHandle — the Manager's OnExit
	// closure, launch()'s pre-registration reconcile, and the attach host's
	// honest-close/teardown — the hub gets one direct notification per run from
	// whichever path wins, with no dependence on term:exit surviving the feed.
	onRunExit func(runID string)

	mu       sync.Mutex
	byHandle map[string]string  // PTY handle -> run id
	byRun    map[string]string  // run id -> PTY handle
	byMeta   map[string]runMeta // PTY handle -> run kind + tool (Snapshot labeling)
	// bySession maps a LIVE run id -> its established observer-session link
	// (session id + the confidence it was established at). Populated by
	// Correlate ONLY for a run byRun still tracks (no resurrection of an ended
	// run) and ONLY when the new observation is at least as strong as the one
	// already stored (MAX-upgrade — a weaker later marker never clobbers a
	// stronger OOB link). Read by SessionForRun so a Snapshot can carry a
	// session id for an attach run (which has no source session at spawn)
	// without a store query. A run with no established link has no entry — an
	// honest "not correlated yet" (correlation is scored + asynchronous).
	bySession map[string]sessionLink
}

// sessionLink is a run's established in-memory correlation: the observer
// session id and the confidence the link was established at. The confidence is
// retained so Correlate can honor the MAX-upgrade contract in memory (only
// strictly stronger evidence replaces an existing link).
type sessionLink struct {
	sessionID  string
	confidence float64
}

// Options configures a Service. Recorder and Launcher are required; the rest
// are optional.
type Options struct {
	Policy   Policy
	Recorder RunRecorder
	Launcher Launcher
	Feed     *termfeed.Feed
	Logger   *slog.Logger
	Now      func() time.Time
	// ExitStatus is the authoritative Manager exit query used by launch() to
	// close the pre-registration exit gap (see Service.exitStatus). Satisfied by
	// termsession.Manager.ExitStatus. Nil disables the reconcile.
	ExitStatus func(handle string) (exited bool, code int, ok bool)
	// OnRunExit is the direct per-run exit callback fired from EndRunByHandle
	// (see Service.onRunExit). Nil disables the direct seam (the status feed's
	// term:exit is then the only exit signal — advisory only).
	OnRunExit func(runID string)
}

// New builds a Service.
func New(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		policy:     opts.Policy,
		rec:        opts.Recorder,
		launcher:   opts.Launcher,
		feed:       opts.Feed,
		logger:     opts.Logger,
		now:        now,
		exitStatus: opts.ExitStatus,
		onRunExit:  opts.OnRunExit,
		byHandle:   make(map[string]string),
		byRun:      make(map[string]string),
		byMeta:     make(map[string]runMeta),
		bySession:  make(map[string]sessionLink),
	}
}

// FreshRequest is the dashboard-derived fresh-launch request. Tool is validated
// against the allow-list; Subcommand is resolved by the dashboard from the
// capability registry; ProjectRoot is the client-influenced input the service
// canonicalizes + allow-list-checks.
type FreshRequest struct {
	Tool        string
	Subcommand  string
	ProjectRoot string
	Rows        uint16
	Cols        uint16
}

// LaunchResult is what a launch returns to the dashboard.
type LaunchResult struct {
	Handle string
	RunID  string
}

// LaunchFresh authorizes and starts a fresh agent (no --continue-from). It
// fails closed on every authorization miss BEFORE minting a run or spawning a
// process.
func (s *Service) LaunchFresh(ctx context.Context, req FreshRequest) (LaunchResult, error) {
	if s.launcher == nil {
		return LaunchResult{}, ErrNoLauncher
	}
	if !s.policy.AllowFresh {
		return LaunchResult{}, ErrFreshLaunchDisabled
	}
	if !s.policy.toolAllowed(req.Tool) {
		return LaunchResult{}, fmt.Errorf("%w: %q", ErrToolNotAllowed, req.Tool)
	}
	dir, err := ValidateProjectRoot(req.ProjectRoot, s.policy.AllowedProjectRoots)
	if err != nil {
		return LaunchResult{}, err
	}
	run := termrun.Run{
		Tool:            req.Tool,
		Kind:            termrun.KindFresh,
		ProjectRootHash: termrun.HashProjectRoot(dir),
		LaunchedAt:      s.now(),
	}
	return s.launch(ctx, run, LaunchRequest{
		Subcommand: req.Subcommand,
		Kind:       termrun.KindFresh,
		Dir:        dir,
		Rows:       req.Rows,
		Cols:       req.Cols,
	})
}

// HandoffRequest is the dashboard-derived continue-a-session request. It does
// not consult the fresh-launch allow-list — handoff-continue is the existing
// narrow consent, gated by [handoff].allow_dashboard_launch at the cmd layer.
type HandoffRequest struct {
	Tool        string
	Subcommand  string
	SessionID   string
	Carry       string
	FromMessage int
	Rows        uint16
	Cols        uint16
}

// LaunchHandoff mints a run + starts a --continue-from launch. The source
// session id is recorded on the run as SourceSessionID (NEVER a correlation
// target — the two are deliberately distinct, plan §2.1a).
func (s *Service) LaunchHandoff(ctx context.Context, req HandoffRequest) (LaunchResult, error) {
	if s.launcher == nil {
		return LaunchResult{}, ErrNoLauncher
	}
	run := termrun.Run{
		Tool:            req.Tool,
		Kind:            termrun.KindHandoff,
		SourceSessionID: req.SessionID,
		LaunchedAt:      s.now(),
	}
	return s.launch(ctx, run, LaunchRequest{
		Subcommand:  req.Subcommand,
		Kind:        termrun.KindHandoff,
		SessionID:   req.SessionID,
		Carry:       req.Carry,
		FromMessage: req.FromMessage,
		Rows:        req.Rows,
		Cols:        req.Cols,
	})
}

// AttachRequest is the CLI-derived request behind `observer <tool> --attach`
// (session-attach design §2.2/Phase 1). Tool + Subcommand come from the wired
// launcher the operator invoked; Dir is the child cwd; Rows/Cols seed the PTY;
// ExtraEnv carries the launcher's proxy-routing env for the child.
type AttachRequest struct {
	// Tool is the target tool name (e.g. "claude-code").
	Tool string
	// Subcommand is the observer launcher verb (e.g. "claude").
	Subcommand string
	// Dir is the child cwd. Empty uses the launcher's default cwd; when set it
	// must be absolute.
	Dir string
	// Rows / Cols are the initial PTY size (0 = OS default).
	Rows uint16
	Cols uint16
	// ExtraEnv is additional child environment ("KEY=VALUE"), carrying the
	// attach launcher's proxy-routing variables.
	ExtraEnv []string
	// ExtraArgs are additional, allow-listed argv tokens the CLI attach client
	// forwards to the inner `observer <Subcommand>` launcher (routing escape
	// hatch + `--` tool remainder). Explicit + allow-listed, never a blind argv
	// copy (B2/B3).
	ExtraArgs []string
}

// LaunchAttachable mints a KindAttach run and spawns a daemon-owned PTY on
// behalf of the operator's own terminal, reached over the attach socket.
//
// Deliberate policy difference from LaunchFresh (session-attach design §3.3):
// the attach socket is AF_UNIX mode 0600, owner-only, so the caller is the node
// operator at their own shell — OS filesystem permissions ARE the authorization.
// The dashboard-launch Policy allow-lists (AllowFresh / AllowedTools /
// AllowedProjectRoots) therefore do NOT gate an attach launch: the operator can
// attach-launch exactly what they could already exec bare. A Policy that would
// deny LaunchFresh still permits LaunchAttachable. It nonetheless mints a run
// identity with Kind=KindAttach through the shared launch() path, so every
// attach session is recorded (before spawn) and feed-published like any other
// run. Validation is intentionally minimal: a non-empty Tool + Subcommand, and
// an absolute Dir when one is supplied.
func (s *Service) LaunchAttachable(ctx context.Context, req AttachRequest) (LaunchResult, error) {
	if s.launcher == nil {
		return LaunchResult{}, ErrNoLauncher
	}
	if req.Tool == "" {
		return LaunchResult{}, ErrAttachToolRequired
	}
	if req.Subcommand == "" {
		return LaunchResult{}, ErrAttachSubcommandRequired
	}
	if req.Dir != "" && !filepath.IsAbs(req.Dir) {
		return LaunchResult{}, fmt.Errorf("%w: %q", ErrAttachDirNotAbsolute, req.Dir)
	}
	run := termrun.Run{
		Tool:            req.Tool,
		Kind:            termrun.KindAttach,
		ProjectRootHash: termrun.HashProjectRoot(req.Dir),
		LaunchedAt:      s.now(),
	}
	return s.launch(ctx, run, LaunchRequest{
		Subcommand: req.Subcommand,
		Kind:       termrun.KindAttach,
		Dir:        req.Dir,
		Rows:       req.Rows,
		Cols:       req.Cols,
		ExtraEnv:   req.ExtraEnv,
		ExtraArgs:  req.ExtraArgs,
	})
}

// ResumeRequest is the dashboard-derived NATIVE-resume request (session-attach
// design Phase 3): reopen a CLOSED session's real transcript by spawning the
// tool's own resume mechanism in a fresh dashboard terminal. Tool is validated
// against the fresh-launch allow-list; Subcommand is the launcher verb resolved
// from the capability registry; ExtraArgs is the resume tail composed at the
// boundary via integration.ResumeArgs (uniformly `["--resume", <id>]`);
// SourceSessionID is the session being resumed; ProjectRoot is the
// client-influenced input canonicalized + allow-list-checked here.
type ResumeRequest struct {
	Tool            string
	Subcommand      string
	ProjectRoot     string
	SourceSessionID string
	ExtraArgs       []string
	Rows            uint16
	Cols            uint16
}

// LaunchResume authorizes and starts a NATIVE resume of a closed session. It
// mints a KindResume run and spawns through the shared launch() path.
//
// Deliberate policy CONTRAST with LaunchAttachable (session-attach design §3.3):
// a resume is a DASHBOARD-initiated Execute, not the owner's own shell over the
// AF_UNIX attach socket, so it is NOT exempt from the launch policy. It enforces
// the IDENTICAL fresh-launch gate as LaunchFresh — AllowFresh + toolAllowed +
// ValidateProjectRoot — failing closed on every authorization miss BEFORE
// minting a run or spawning a process. A Policy that denies LaunchFresh
// therefore also denies LaunchResume (whereas LaunchAttachable, whose caller is
// authorized by the socket's 0600 filesystem permissions, would still permit
// it). The resumed session id is recorded on the run as SourceSessionID (NEVER a
// correlation target — the same distinction LaunchHandoff draws, plan §2.1a).
func (s *Service) LaunchResume(ctx context.Context, req ResumeRequest) (LaunchResult, error) {
	if s.launcher == nil {
		return LaunchResult{}, ErrNoLauncher
	}
	if !s.policy.AllowFresh {
		return LaunchResult{}, ErrFreshLaunchDisabled
	}
	if !s.policy.toolAllowed(req.Tool) {
		return LaunchResult{}, fmt.Errorf("%w: %q", ErrToolNotAllowed, req.Tool)
	}
	dir, err := ValidateProjectRoot(req.ProjectRoot, s.policy.AllowedProjectRoots)
	if err != nil {
		return LaunchResult{}, err
	}
	run := termrun.Run{
		Tool:            req.Tool,
		Kind:            termrun.KindResume,
		SourceSessionID: req.SourceSessionID,
		ProjectRootHash: termrun.HashProjectRoot(dir),
		LaunchedAt:      s.now(),
	}
	return s.launch(ctx, run, LaunchRequest{
		Subcommand: req.Subcommand,
		Kind:       termrun.KindResume,
		Dir:        dir,
		SessionID:  req.SourceSessionID,
		ExtraArgs:  req.ExtraArgs,
		Rows:       req.Rows,
		Cols:       req.Cols,
	})
}

// launch is the shared mint→record→spawn→map path for both kinds. It records
// the run BEFORE spawning so a record failure never leaves an orphan process,
// and marks the run ended if the spawn itself fails.
func (s *Service) launch(ctx context.Context, run termrun.Run, lr LaunchRequest) (LaunchResult, error) {
	runID, err := termrun.NewRunID()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("termsvc: mint run id: %w", err)
	}
	corr, err := termrun.NewCorrelationToken()
	if err != nil {
		return LaunchResult{}, fmt.Errorf("termsvc: mint correlation token: %w", err)
	}
	run.RunID = runID
	run.CorrelationTokenHash = termrun.HashCorrelationToken(corr)
	lr.RunID = runID
	lr.CorrelationToken = corr

	lr.Tool = run.Tool
	if err := s.rec.RecordRun(ctx, run); err != nil {
		return LaunchResult{}, fmt.Errorf("termsvc: record run: %w", err)
	}

	handle, err := s.launcher.Spawn(lr)
	if err != nil {
		// The run exists but never produced a process — close it out so it is
		// not left dangling as "running".
		_ = s.rec.EndRun(ctx, runID, s.now(), -1, reasonChildExit)
		return LaunchResult{}, err
	}

	s.mu.Lock()
	s.byHandle[handle] = runID
	s.byRun[runID] = handle
	// dir is lr.Dir — the canonical, already-validated project root each caller
	// (LaunchFresh/LaunchResume via ValidateProjectRoot, LaunchAttachable via
	// AttachRequest.Dir) hashed into run.ProjectRootHash. One place, same value.
	//
	// Pin the retained root by fully resolving its symlinks HERE, the single
	// storage point (finding 2a): the project panel resolves this token back to
	// its root and browses under it, so canonicalizing at launch means a symlink
	// component retargeted after launch cannot silently move the panel's root to
	// a new target. On any resolution error (path missing/unreadable) store ""
	// → treated as "no browsable project root" (ProjectRoot returns ok=false).
	dir := lr.Dir
	if dir != "" {
		if resolved, rerr := filepath.EvalSymlinks(dir); rerr == nil {
			dir = resolved
		} else {
			dir = ""
		}
	}
	s.byMeta[handle] = runMeta{Kind: run.Kind, Tool: run.Tool, dir: dir}
	s.mu.Unlock()

	s.publish(termfeed.Event{
		Kind:  "term:launch:" + string(run.Kind),
		RunID: runID,
		Tool:  run.Tool,
		Trust: termfeed.TrustTrusted,
		At:    s.now(),
	})

	// Reconcile the PRE-REGISTRATION exit gap (resilient-attach round-4). The
	// child can exit in the window between s.launcher.Spawn returning above and
	// the byHandle mapping being installed just now. In that window the Manager's
	// OnExit → EndRunByHandle found NO mapping and returned silently, so no exit
	// was ever recorded and neither term:exit nor onRunExit ever fired — the hub
	// would then reserve liveness + park the flock-release callback forever. Now
	// that the mapping exists, ask the authoritative Manager whether the handle
	// already exited; if so, run the SAME EndRunByHandle end path exactly once.
	// It is idempotent with a later OnExit (EndRunByHandle dedupes by handle: the
	// second caller finds no mapping and no-ops), and it fires onRunExit → the
	// hub's direct exit seam so the fast-exit tombstone is recorded reliably
	// rather than depending on a term:exit that never came. Manager stays the one
	// owner of exit truth; termsvc only asks.
	if s.exitStatus != nil {
		if exited, code, ok := s.exitStatus(handle); ok && exited {
			s.EndRunByHandle(ctx, handle, code)
		}
	}
	return LaunchResult{Handle: handle, RunID: runID}, nil
}

// RunIDForHandle returns the run id a PTY handle belongs to, if known.
func (s *Service) RunIDForHandle(handle string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runID, ok := s.byHandle[handle]
	return runID, ok
}

// KindForHandle returns the run Kind and target Tool a live PTY handle belongs
// to, resolved from the run identity the Service minted at spawn — no store
// read. ok=false for an unknown/ended handle. Additive read seam for Snapshot
// labeling (session-attach design Phase 2): the daemon already knows a run's
// Kind + Tool at launch, so a live handle can be labeled without a query, and a
// consumer can dispatch on run SHAPE (e.g. "is this an attach session?") rather
// than a tool name.
func (s *Service) KindForHandle(handle string) (termrun.Kind, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// F4: opportunistically shed a stale ended-entry burst here too. A burst of
	// short runs that all END within one grace window leaves every entry too
	// young for the EndRunByHandle-time GC; if activity then stops and no
	// dashboard Snapshot ever calls PruneEndedHandles, nothing re-triggers the
	// GC once the grace lapses. This read path is exercised by any live daemon
	// (Snapshot labeling + the remote-sensitivity gates), so folding a bounded
	// GC in here reclaims the burst on the next real activity. (A fully-idle
	// daemon retaining a burst until its next activity is accepted — the
	// entries are past-linger dead metadata, harmless until reclaimed.)
	s.gcEndedMetaLocked()
	m, ok := s.byMeta[handle]
	return m.Kind, m.Tool, ok
}

// ProjectRoot returns the canonical project root a live PTY handle was launched
// with, resolved from the run identity the Service minted at spawn — no store
// read. ok=false when the handle is unknown OR the run was launched with the
// default cwd (empty dir): both are honest "no browsable project root" answers.
// Served from byMeta (retained past exit-linger), so it stays answerable for a
// lingering handle exactly like KindForHandle. Additive read seam for the
// dashboard project panel (Arc A): the daemon already knows a run's validated
// root at launch, so a token can be resolved to its root without a query, and
// the browser never supplies a filesystem path.
func (s *Service) ProjectRoot(handle string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byMeta[handle]
	if !ok || m.dir == "" {
		return "", false
	}
	return m.dir, true
}

// SessionForRun returns the observer session id a run has been correlated to,
// once the correlation is established (>= termrun.MinLinkConfidence). ok=false
// when the run has no established link yet — correlation is scored and
// asynchronous, so a freshly-spawned attach session legitimately has none. The
// value is served from the in-memory map Correlate maintains (no store read on
// the Snapshot hot path).
func (s *Service) SessionForRun(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// F4: same opportunistic burst-shed as KindForHandle — this is the other
	// per-run read the Snapshot hot path drives, so a live daemon reclaims a
	// past-grace ended-entry burst here even when no PruneEndedHandles ever runs.
	s.gcEndedMetaLocked()
	link, ok := s.bySession[runID]
	return link.sessionID, ok
}

// SessionLinkForRun returns the observer session id a run has been correlated
// to AND the confidence that link was established at, once the correlation is
// established (>= termrun.MinLinkConfidence). ok=false when the run has no
// established link yet — correlation is scored and asynchronous, so a
// freshly-spawned attach session legitimately has none. It is the
// confidence-carrying companion of SessionForRun (which drops the field):
// same in-memory map, same opportunistic burst-shed, same no-store-read
// contract on the Snapshot hot path. The confidence honors the MAX-upgrade
// semantics Correlate maintains, so a caller sees the strongest evidence
// scored so far.
func (s *Service) SessionLinkForRun(runID string) (sessionID string, confidence float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Same opportunistic burst-shed as SessionForRun — this is another per-run
	// read the Snapshot hot path can drive, so a live daemon reclaims a
	// past-grace ended-entry burst here even when no PruneEndedHandles ever runs.
	s.gcEndedMetaLocked()
	link, ok := s.bySession[runID]
	return link.sessionID, link.confidence, ok
}

// ResolveHandleLink resolves a PTY handle to its full Session-Cockpit link in
// ONE lock acquisition: liveness (byHandle), run identity (byMeta Kind+Tool),
// and the correlated observer session id + confidence (bySession) are read
// ATOMICALLY. The three-call chain it replaces (RunIDForHandle → KindForHandle
// → SessionLinkForRun) took s.mu three separate times, so a run ending between
// the first and third call could return known=true carrying a correlation that
// EndRunByHandle had already deleted (a live=true answer with a stale/deleted
// session link). Reading all three maps under the single lock closes that race.
// ok=false for an unknown/exited handle (byHandle tracks LIVE runs only — same
// liveness posture as RunIDForHandle). The composed methods are left untouched
// for their existing callers.
func (s *Service) ResolveHandleLink(handle string) (runID string, kind termrun.Kind, tool, sessionID string, confidence float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runID, live := s.byHandle[handle]
	if !live {
		return "", "", "", "", 0, false
	}
	// Opportunistic burst-shed, matching KindForHandle/SessionLinkForRun — this
	// read is on the same Snapshot/gate hot path. It never sheds the live handle
	// resolved here (GC only reaps ended entries past grace).
	s.gcEndedMetaLocked()
	if m, mok := s.byMeta[handle]; mok {
		kind, tool = m.Kind, m.Tool
	}
	if link, lok := s.bySession[runID]; lok {
		sessionID, confidence = link.sessionID, link.confidence
	}
	return runID, kind, tool, sessionID, confidence, true
}

// HandleForRun returns the PTY handle a run currently maps to, if live.
func (s *Service) HandleForRun(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	handle, ok := s.byRun[runID]
	return handle, ok
}

// EndRunByHandle records a run's exit, keyed by the PTY handle termsession
// reports on process exit (the daemon-observed, trusted lifecycle signal). It
// is idempotent and best-effort; an unknown handle is ignored. It also publishes
// an exit event to the status feed.
//
// It forgets the LIVE-run mappings (byHandle/byRun/bySession) so no later
// correlation can resurrect an ended run and the live-run gates immediately see
// the run as gone. It DELIBERATELY retains byMeta (the run Kind + Tool), because
// the daemon-observed exit only starts termsession's ExitLinger — the PTY stays
// subscribable in the Manager for up to that window, and the remote sensitivity
// gates (visibleSnapshot / handleLaunchWS / IsRemoteSensitiveSession →
// KindForHandle) MUST keep classifying an exited-but-lingering attach/resume
// handle as sensitive for as long as the Manager can still hand out its bytes.
// byMeta is instead RETAINED here (with its endedAt stamped) and garbage-
// collected later — by PruneEndedHandles (Snapshot-driven) and the opportunistic
// gcEndedMetaLocked below — only once the entry has been ended for longer than
// endedHandleGrace (which exceeds ExitLinger + snapshot staleness). So a byMeta
// entry outlives the handle in the Manager by a bounded grace (safe over-
// classification), and never UNDER-lives it (the confidentiality hole this
// closes: the pre-fix delete here made KindForHandle false mid-linger, and the
// pre-R2-2 set-only prune reopened it against a stale snapshot).
func (s *Service) EndRunByHandle(ctx context.Context, handle string, exitCode int) {
	s.mu.Lock()
	runID, ok := s.byHandle[handle]
	if ok {
		delete(s.byHandle, handle)
		delete(s.byRun, runID)
		delete(s.bySession, runID)
		// byMeta is intentionally NOT deleted here — see the doc comment. Instead
		// stamp its exit time so PruneEndedHandles (and the opportunistic GC just
		// below) can age it out past endedHandleGrace, while a zero endedAt keeps
		// a still-live entry classified even against a stale live-handle set (R2-2).
		if m, mok := s.byMeta[handle]; mok {
			m.endedAt = s.now()
			s.byMeta[handle] = m
		}
		// Opportunistic bound (R2-7): on a daemon driven only through attach
		// sessions, no dashboard Snapshot ever calls PruneEndedHandles, so ended
		// entries would accumulate one-per-run until restart. Shed the long-dead
		// ones here when the retained-ended count crosses the soft bound.
		s.gcEndedMetaLocked()
	}
	s.mu.Unlock()
	if !ok {
		// No live mapping for this handle. Post the round-4 reconcile this is the
		// IDEMPOTENT case (the run already ended — reconcile beat OnExit, or vice
		// versa) OR a genuinely unknown handle; either way it is deliberately a
		// no-op, but it is logged (was silently swallowed before) so the
		// missing-producer path that motivated the reconcile is observable rather
		// than invisible.
		if s.logger != nil {
			s.logger.Debug("termsvc: EndRunByHandle found no live mapping (idempotent no-op or unknown handle)", "handle", handle)
		}
		return
	}
	// Natural, daemon-observed exit. The recorder GUARDS end_reason so if a
	// graceful-shutdown sweep already stamped this run 'daemon_shutdown' (the PTY
	// kill that triggered THIS exit), the reason is preserved and only ended_at
	// is updated — the run stays resumable-by-restart (review finding H2). The
	// DB error stays swallowed (best-effort at exit): a fully-failed write leaves
	// the run looking like a crash orphan (reason '', ended_at NULL), which the
	// rediscovery gate WOULD offer — but native-resuming an already-finished
	// session is safe (the tool just reopens its own transcript), so the failure
	// direction is benign.
	_ = s.rec.EndRun(ctx, runID, s.now(), exitCode, reasonChildExit)
	s.publish(termfeed.Event{
		Kind:  "term:exit",
		RunID: runID,
		Trust: termfeed.TrustTrusted,
		At:    s.now(),
	})
	// Fire the DIRECT, reliable per-run exit seam (resilient-attach round-4). It
	// runs exactly once per run — only the EndRunByHandle call that found the live
	// mapping reaches here — and post runID resolution, so the hub can key its
	// liveness / flock-release / tombstone correctness off it without depending on
	// the term:exit above surviving the lossy feed. Fired OUTSIDE s.mu (the hub
	// takes its own lock) to keep the two lock orders independent.
	if s.onRunExit != nil {
		s.onRunExit(runID)
	}
}

// PruneEndedHandles garbage-collects retained per-handle classification metadata
// (byMeta) for handles the Manager no longer tracks. The caller — the Snapshot
// enrichment adapter, the ONE owner of the Manager's live view — passes the set
// of handles currently present in the Manager (live OR exited-but-lingering).
//
// It is race-proof against a STALE live-handle set (R2-2). A byMeta entry is
// dropped ONLY when ALL of:
//
//	(a) it is absent from the passed live set, AND
//	(b) it is not still live-tracked in byHandle, AND
//	(c) it has actually ENDED (non-zero endedAt), AND
//	(d) that end is older than endedHandleGrace.
//
// The pre-fix version dropped an entry the instant it was merely absent from the
// live set + byHandle. That reopened the exit-linger hole under this interleave:
// a Snapshot captured a live set BEFORE handle H registered; H then registered,
// classified sensitive, and exited (EndRunByHandle removed byHandle while the
// Manager kept H for linger); the OLD, STALE set — which never saw H — then
// pruned H, making KindForHandle(H) false mid-linger. With the endedAt stamp,
// H's just-set endedAt is far younger than endedHandleGrace (which exceeds
// ExitLinger + staleness), so (d) fails and H is retained — classification
// survives its whole linger. A still-LIVE entry (zero endedAt) fails (c) and is
// never pruned even against a stale set.
//
// It only shrinks byMeta, so it is safe to call on every Snapshot. A nil/empty
// set no longer prunes everything: an ended-but-within-grace entry is still
// kept (grace, not set-membership, is the aging clock now).
func (s *Service) PruneEndedHandles(liveHandles map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for handle, meta := range s.byMeta {
		if _, live := liveHandles[handle]; live {
			continue
		}
		// Retain while still live-tracked (byHandle) — a belt-and-braces guard so
		// a Snapshot that races a just-completed launch (handle mapped but not yet
		// in the passed set) never drops a live run's classification.
		if _, tracked := s.byHandle[handle]; tracked {
			continue
		}
		// A zero endedAt means the run has not been recorded as ended — retain it
		// even if a STALE live set omits it (R2-2 core).
		if meta.endedAt.IsZero() {
			continue
		}
		// An ended entry ages out only past the grace, which exceeds ExitLinger +
		// any plausible snapshot staleness, so it never drops mid-linger.
		if now.Sub(meta.endedAt) < endedHandleGrace {
			continue
		}
		delete(s.byMeta, handle)
	}
}

// gcEndedMetaLocked opportunistically sheds ENDED byMeta entries older than
// endedHandleGrace when the retained-ended count crosses endedHandleGCBound.
// It bounds byMeta growth on a daemon driven ONLY through repeated attach
// sessions with no dashboard Snapshot polling (which is the usual
// PruneEndedHandles caller) — R2-7. It is called from EndRunByHandle (on each
// exit) AND from the KindForHandle / SessionForRun read paths (F4), so a burst
// of runs that all end WITHIN one grace window — leaving every entry too young
// for the exit-time GC — is still reclaimed on the next real activity once the
// grace lapses, rather than lingering until the next Snapshot or daemon restart.
// A fully-idle daemon (no exits AND no reads after the burst) retains the burst
// until its next activity; that is accepted — the entries are past-linger dead
// metadata, harmless until reclaimed. Self-contained in termsvc: it needs no
// Manager live set, because a run older than the grace is necessarily reaped
// past ExitLinger (grace > ExitLinger). It NEVER drops a still-live entry (zero
// endedAt) or a within-grace ended entry, so it preserves the exit-linger
// classification guarantee identically to PruneEndedHandles; it only sheds
// long-dead metadata. Must be called under s.mu.
func (s *Service) gcEndedMetaLocked() {
	ended := 0
	for _, m := range s.byMeta {
		if !m.endedAt.IsZero() {
			ended++
		}
	}
	if ended <= endedHandleGCBound {
		return
	}
	now := s.now()
	for handle, m := range s.byMeta {
		if m.endedAt.IsZero() {
			continue
		}
		if now.Sub(m.endedAt) < endedHandleGrace {
			continue
		}
		delete(s.byMeta, handle)
	}
}

// Correlate records a scored run→session correlation observation. It scores the
// single observation through internal/termrun (the one owner of the source→
// confidence policy) and upserts it (MAX-upgrade). Source ordering is
// oob > marker > heuristic; a weaker later observation never downgrades a
// stronger established link.
func (s *Service) Correlate(ctx context.Context, runID, sessionID string, source termrun.Source, at time.Time) error {
	if runID == "" || sessionID == "" {
		return nil
	}
	corr, ok := termrun.Score(runID, sessionID, []termrun.Observation{{Source: source, At: at}})
	if !ok {
		return nil
	}
	if err := s.rec.RecordCorrelation(ctx, corr); err != nil {
		return fmt.Errorf("termsvc: record correlation: %w", err)
	}
	// Remember an ESTABLISHED link in memory so Snapshot can label a run (an
	// attach session carries no source session at spawn) with its correlated
	// observer session id without a store read. Only a link at or above
	// MinLinkConfidence is surfaced — a weak heuristic never fills a session id,
	// matching the "links attach only once established" rule (design §2.1a).
	//
	// Two invariants enforced under mu (P2-3):
	//   (a) LIFECYCLE — update only when byRun[runID] still tracks a LIVE run.
	//       RecordCorrelation above ran WITHOUT the lock (a store write), so an
	//       EndRunByHandle could have deleted this run's maps in the meantime.
	//       Writing bySession here without the guard would resurrect an ended
	//       run's entry that nothing can ever delete (EndRunByHandle already
	//       fired). Gating on byRun keeps bySession a strict subset of live
	//       runs, and rejects an observation for an unknown/never-launched run.
	//   (b) MAX-UPGRADE — replace an existing link only with strictly stronger
	//       evidence, so a later marker/heuristic observation can never clobber
	//       a stronger OOB link in memory (the store keeps its own MAX-upgrade;
	//       this mirrors it for the in-memory hot-path copy).
	if corr.Linkable() {
		s.mu.Lock()
		if _, live := s.byRun[runID]; live {
			if cur, exists := s.bySession[runID]; !exists || corr.Confidence > cur.confidence {
				s.bySession[runID] = sessionLink{sessionID: sessionID, confidence: corr.Confidence}
			}
		}
		s.mu.Unlock()
	}
	// A correlation event enters the feed so F4 can attach a session id to a
	// run's status once the link is established.
	trust := termfeed.TrustHint
	if source == termrun.SourceOOB {
		trust = termfeed.TrustTrusted
	}
	s.publish(termfeed.Event{
		Kind:      "term:correlate:" + string(source),
		RunID:     runID,
		SessionID: sessionID,
		Trust:     trust,
		At:        at,
	})
	return nil
}

// publish emits an event to the status feed when one is configured.
func (s *Service) publish(ev termfeed.Event) {
	if s.feed != nil {
		s.feed.Publish(ev)
	}
}
