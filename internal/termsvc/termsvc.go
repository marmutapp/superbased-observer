package termsvc

import (
	"context"
	"errors"
	"fmt"
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

// RunRecorder persists run identity + scored correlations. It is satisfied by
// a cmd adapter over *store.Store; termsvc speaks internal/termrun's pure
// types so it never imports the store.
type RunRecorder interface {
	// RecordRun persists a newly minted run identity.
	RecordRun(ctx context.Context, run termrun.Run) error
	// EndRun records a run's exit (ended_at + exit code).
	EndRun(ctx context.Context, runID string, endedAt time.Time, exitCode int) error
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
}

// Launcher spawns a PTY-backed launcher and returns its opaque handle. It is
// satisfied by a cmd adapter over *termsession.Manager (plus the OOB FD
// plumbing). termsvc owns identity/authorization; the Launcher owns the PTY.
type Launcher interface {
	Spawn(req LaunchRequest) (handle string, err error)
}

// Service is the terminal application service. One instance per daemon.
type Service struct {
	policy   Policy
	rec      RunRecorder
	launcher Launcher
	feed     *termfeed.Feed // optional; nil disables the status event feed
	now      func() time.Time

	mu       sync.Mutex
	byHandle map[string]string // PTY handle -> run id
	byRun    map[string]string // run id -> PTY handle
}

// Options configures a Service. Recorder and Launcher are required; Feed and
// Now are optional.
type Options struct {
	Policy   Policy
	Recorder RunRecorder
	Launcher Launcher
	Feed     *termfeed.Feed
	Now      func() time.Time
}

// New builds a Service.
func New(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		policy:   opts.Policy,
		rec:      opts.Recorder,
		launcher: opts.Launcher,
		feed:     opts.Feed,
		now:      now,
		byHandle: make(map[string]string),
		byRun:    make(map[string]string),
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
		_ = s.rec.EndRun(ctx, runID, s.now(), -1)
		return LaunchResult{}, err
	}

	s.mu.Lock()
	s.byHandle[handle] = runID
	s.byRun[runID] = handle
	s.mu.Unlock()

	s.publish(termfeed.Event{
		Kind:  "term:launch:" + string(run.Kind),
		RunID: runID,
		Tool:  run.Tool,
		Trust: termfeed.TrustTrusted,
		At:    s.now(),
	})
	return LaunchResult{Handle: handle, RunID: runID}, nil
}

// RunIDForHandle returns the run id a PTY handle belongs to, if known.
func (s *Service) RunIDForHandle(handle string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	runID, ok := s.byHandle[handle]
	return runID, ok
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
// an exit event to the status feed and forgets the handle mapping.
func (s *Service) EndRunByHandle(ctx context.Context, handle string, exitCode int) {
	s.mu.Lock()
	runID, ok := s.byHandle[handle]
	if ok {
		delete(s.byHandle, handle)
		delete(s.byRun, runID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.rec.EndRun(ctx, runID, s.now(), exitCode)
	s.publish(termfeed.Event{
		Kind:  "term:exit",
		RunID: runID,
		Trust: termfeed.TrustTrusted,
		At:    s.now(),
	})
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
