package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/store"
	"github.com/marmutapp/superbased-observer/internal/termfeed"
	"github.com/marmutapp/superbased-observer/internal/termoob"
	"github.com/marmutapp/superbased-observer/internal/termrun"
	"github.com/marmutapp/superbased-observer/internal/termsession"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// terminal_launch.go holds the cmd-side adapters the terminal application
// service (internal/termsvc) depends on: the persistence recorder over
// internal/store, and the PTY launcher over internal/termsession that also
// allocates + drains the trusted out-of-band control channel (internal/termoob,
// plan §2.1b / F1). termsvc speaks internal/termrun's pure types; these
// adapters translate to the store's row types and to the termsession spec.

// termRunRecorder persists run identity + correlations for termsvc.
type termRunRecorder struct{ st *store.Store }

func (r termRunRecorder) RecordRun(ctx context.Context, run termrun.Run) error {
	return r.st.InsertTerminalRun(ctx, store.TerminalRun{
		RunID:                run.RunID,
		Tool:                 run.Tool,
		Kind:                 string(run.Kind),
		SourceSessionID:      run.SourceSessionID,
		ProjectRootHash:      run.ProjectRootHash,
		CorrelationTokenHash: run.CorrelationTokenHash,
		LaunchedAt:           run.LaunchedAt,
	})
}

func (r termRunRecorder) EndRun(ctx context.Context, runID string, endedAt time.Time, exitCode int) error {
	return r.st.EndTerminalRun(ctx, runID, endedAt, exitCode)
}

func (r termRunRecorder) RecordCorrelation(ctx context.Context, c termrun.Correlation) error {
	return r.st.UpsertCorrelation(ctx, store.TerminalCorrelation{
		RunID:      c.RunID,
		SessionID:  c.SessionID,
		Confidence: c.Confidence,
		Source:     string(c.Source),
		ObservedAt: c.ObservedAt,
	})
}

// Environment variables the daemon injects so the launcher wrapper (F3) can
// find and authenticate the OOB channel. The tool child never inherits the FD
// once F3 sets close-on-exec; F1 only allocates + drains it.
const (
	envOOBFD   = "OBSERVER_OOB_FD"   // inherited FD number (3+)
	envOOBAuth = "OBSERVER_OOB_AUTH" // per-session Hello auth secret
	envOOBCorr = "OBSERVER_OOB_CORR" // run correlation nonce to echo
	envOOBTool = "OBSERVER_OOB_TOOL" // target tool name
	envOOBRun  = "OBSERVER_OOB_RUN"  // run id (informational)
)

// ptyLauncher implements termsvc.Launcher over *termsession.Manager. It
// allocates the trusted OOB control-channel pipe, hands the child the write end
// at fd 3 (via ExtraFiles), keeps + drains the read end on a goroutine, and
// builds the server-derived termsession spec.
type ptyLauncher struct {
	mgr     *termsession.Manager
	binPath string
	feed    *termfeed.Feed
	logger  *slog.Logger
}

// Spawn starts a PTY-backed launcher for a validated request and returns its
// opaque handle. It is the single place the OOB FD is allocated: after the
// child is spawned the daemon closes its own copy of the write end so the read
// end reports EOF when the child exits, and a goroutine drains framed OOB
// signals (which the launcher wrapper begins emitting in F3).
func (l *ptyLauncher) Spawn(req termsvc.LaunchRequest) (string, error) {
	authToken, err := termoob.NewSessionToken()
	if err != nil {
		return "", err
	}
	oobRead, oobWrite, err := os.Pipe()
	if err != nil {
		return "", err
	}

	spec := termsession.Spec{
		BinPath:     l.binPath,
		Subcommand:  req.Subcommand,
		Fresh:       req.Kind == termrun.KindFresh,
		SessionID:   req.SessionID,
		Carry:       req.Carry,
		FromMessage: req.FromMessage,
		Rows:        req.Rows,
		Cols:        req.Cols,
		Dir:         req.Dir,
		Env:         launchChildEnv(req, authToken),
		ExtraFiles:  []*os.File{oobWrite},
	}
	handle, cerr := l.mgr.Create(spec)
	// Close the daemon's copy of the write end unconditionally: the child has
	// its own dup (on success), and on failure it frees the pipe.
	_ = oobWrite.Close()
	if cerr != nil {
		_ = oobRead.Close()
		return "", cerr
	}
	go l.drainOOB(req.RunID, req.Tool, oobRead, authToken)
	l.auditEnv(req, spec.Env)
	return handle, nil
}

// launchChildEnv builds the child environment: the daemon env with any stale
// OOB variables stripped (defence — a nested launch must never inherit a
// parent's channel), plus this launch's fresh OOB variables.
//
// NOTE (deviation from a strict allow-list, plan §F1): a coding agent
// legitimately needs the operator's provider env (API keys it uses to talk to
// its model), so a hard allow-list would break every launched tool. The
// daemon, when started via `observer start`, runs with the user's own shell
// env (observer keeps its own secrets in config files, not env), so the
// residual exposure is the user's own env — which the agent needs anyway. We
// therefore strip only the internal OOB channel variables and emit a per-launch
// env audit line. A tighter policy is a documented follow-up.
func launchChildEnv(req termsvc.LaunchRequest, authToken string) []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+5)
	for _, kv := range parent {
		if strings.HasPrefix(kv, "OBSERVER_OOB_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(
		out,
		envOOBFD+"=3", // ExtraFiles[0] → fd 3 in the child
		envOOBAuth+"="+authToken,
		envOOBCorr+"="+req.CorrelationToken,
		envOOBTool+"="+req.Tool,
		envOOBRun+"="+req.RunID,
	)
	return out
}

// auditEnv logs one metadata-only line per launch: the var count and the
// launch's identity (no values). This is the plan §F1 per-launch env audit.
func (l *ptyLauncher) auditEnv(req termsvc.LaunchRequest, env []string) {
	if l.logger == nil {
		return
	}
	l.logger.Info("terminal launch: spawned",
		"run", req.RunID, "tool", req.Tool, "kind", string(req.Kind),
		"env_vars", strconv.Itoa(len(env)), "dir_set", req.Dir != "")
}

// drainOOB reads framed OOB signals from the child on a daemon goroutine. In
// F1 the launcher wrapper does not yet emit (that is F3), so this typically
// just blocks until the child exits (EOF). When frames do arrive it publishes
// them to the status feed as TRUSTED events (the channel is authenticated and
// unforgeable — §2.1b). Any framing violation poisons the decoder (fail-closed)
// and ends the drain; it never affects the PTY session.
func (l *ptyLauncher) drainOOB(runID, tool string, r *os.File, authToken string) {
	defer func() { _ = r.Close() }()
	dec := termoob.NewDecoder(r, authToken)
	for {
		frame, err := dec.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && l.logger != nil {
				l.logger.Debug("terminal oob: drain ended", "run", runID, "err", err)
			}
			return
		}
		l.publishOOB(runID, tool, frame)
	}
}

// publishOOB turns a trusted OOB frame into a status-feed event (F4 consumes
// it). Unknown frame types are ignored (forward-compat).
func (l *ptyLauncher) publishOOB(runID, tool string, frame termoob.Frame) {
	if l.feed == nil {
		return
	}
	switch frame.Type {
	case termoob.TypeHello:
		l.feed.Publish(termfeed.Event{
			Kind: "oob:hello", RunID: runID, Tool: tool,
			Trust: termfeed.TrustTrusted, At: time.Now().UTC(),
		})
	case termoob.TypeLifecycle:
		if frame.Lifecycle != nil {
			l.feed.Publish(termfeed.Event{
				Kind: "oob:" + string(frame.Lifecycle.Phase), RunID: runID, Tool: tool,
				Trust: termfeed.TrustTrusted, At: time.Now().UTC(),
			})
		}
	}
}
