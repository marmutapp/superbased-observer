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

func (r termRunRecorder) EndRun(ctx context.Context, runID string, endedAt time.Time, exitCode int, reason string) error {
	return r.st.EndTerminalRun(ctx, runID, endedAt, exitCode, reason)
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

// envDaemonChild is the INFALLIBLE marker the daemon sets in EVERY child env it
// spawns. The two agent-launch paths (attach-spawn + dashboard terminal) funnel
// through launchChildEnv, and the ONE non-termsvc path — a SpecSetup session
// (CreateSetup), which bypasses this launcher — carries it via setupChildEnv, so
// the "every daemon child carries the marker" invariant holds tree-wide (finding:
// marker completeness). Unlike the OOB channel (which is best-effort — a missing
// FD or a failed Hello leaves oobChannelActive() false), this env var is set
// unconditionally, so a daemon-owned inner launcher whose OOB channel died still
// recognizes itself as a daemon child and refuses to re-attach. decideAttach
// checks it FIRST as the primary anti-recursion gate (review finding H1);
// oobChannelActive() stays a belt-and-suspenders secondary row.
const envDaemonChild = "OBSERVER_DAEMON_CHILD"

// runningAsDaemonChild reports whether THIS process was spawned by the daemon's
// terminal launcher (the env marker is present). It is the infallible input
// decideAttach consults before the best-effort OOB probe. Reading the env keeps
// decideAttach pure — the launcher passes this bool in like the reachability
// probe.
func runningAsDaemonChild() bool {
	return os.Getenv(envDaemonChild) == "1"
}

// ptyLauncher implements termsvc.Launcher over *termsession.Manager. It
// allocates the trusted OOB control-channel pipe, hands the child the write end
// at fd 3 (via ExtraFiles), keeps + drains the read end on a goroutine, and
// builds the server-derived termsession spec.
type ptyLauncher struct {
	mgr     *termsession.Manager
	binPath string
	feed    *termfeed.Feed
	logger  *slog.Logger
	// correlate is the production run->session correlation seam (P2-1). When the
	// launcher wrapper announces the child's agent session id on the TRUSTED OOB
	// channel (termoob.TypeSession), drainOOB records it here at oob confidence.
	// Wired to termsvc.Service.Correlate in newLaunchManager; nil disables
	// correlation (the drain still runs — it just doesn't establish links).
	correlate func(ctx context.Context, runID, sessionID string, source termrun.Source, at time.Time) error
}

// argvModeTable maps a run KIND onto the termsession argv SHAPE (CLAUDE.md #5 —
// a data table, not an if-ladder). Fresh, attach, AND resume all launch a BARE
// `observer <sub>` base: a fresh launch has no continuation, an attach launch
// carries only routing escape-hatch ExtraArgs, and a resume's native `--resume
// <id>` tail rides in ExtraArgs (so it must NOT get the handoff --continue-from
// prefix — claude/codex reject `--continue-from … --resume …` together, F2).
// Only a handoff launch takes --continue-from.
var argvModeTable = map[termrun.Kind]termsession.ArgvMode{
	termrun.KindFresh:   termsession.ArgvModeFresh,
	termrun.KindAttach:  termsession.ArgvModeFresh,
	termrun.KindResume:  termsession.ArgvModeFresh,
	termrun.KindHandoff: termsession.ArgvModeHandoff,
}

// argvModeForKind resolves a run kind to its argv shape. An unlisted kind falls
// back to ArgvModeHandoff (the zero value), the more restrictive default — it
// requires a SessionID, so a mis-mapped kind fails closed at Create rather than
// silently launching a bare agent.
func argvModeForKind(kind termrun.Kind) termsession.ArgvMode {
	return argvModeTable[kind]
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
		BinPath:    l.binPath,
		Subcommand: req.Subcommand,
		// The run KIND selects the argv SHAPE through argvModeForKind (below): a
		// fresh, attach, OR native-resume launch is a bare `observer <sub>` base
		// (a resume's `--resume <id>` tail rides in ExtraArgs), and only a
		// handoff launch takes the --continue-from argv. Table, not a boolean —
		// a resume is NOT "fresh", it just shares the fresh base argv (F2).
		ArgvMode:    argvModeForKind(req.Kind),
		SessionID:   req.SessionID,
		Carry:       req.Carry,
		FromMessage: req.FromMessage,
		Rows:        req.Rows,
		Cols:        req.Cols,
		Dir:         req.Dir,
		Env:         launchChildEnv(req, authToken),
		ExtraFiles:  []*os.File{oobWrite},
		// Attach launches carry the CLI client's allow-listed argv (routing
		// escape hatch + `--` tool remainder) so the inner `observer <sub>`
		// self-configures exactly like a bare launch (B2/B3). Nil for
		// dashboard fresh/handoff launches → argv unchanged.
		ExtraArgs: req.ExtraArgs,
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
// parent's channel), plus the request's ExtraEnv, plus this launch's fresh OOB
// variables.
//
// The ExtraEnv append is load-bearing for the attach path: LaunchRequest.
// ExtraEnv carries the attach launcher's proxy-routing variables, and dropping
// it here (the pre-2026-07-19 bug) silently voided them. ExtraEnv is layered
// AFTER the inherited env (so an explicit routing var the launcher chose wins
// over a stale inherited one) and BEFORE the OOB vars (which are internal and
// must never be overridden by a caller-supplied entry).
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
	out := make([]string, 0, len(parent)+len(req.ExtraEnv)+6)
	for _, kv := range parent {
		if isInternalChildEnv(kv) {
			continue
		}
		out = append(out, kv)
	}
	// Caller-supplied extra env (attach proxy-routing vars). Defensively drop
	// any OBSERVER_OOB_ / OBSERVER_DAEMON_CHILD entry so the internal channel and
	// the daemon-child marker can't be spoofed in by a caller.
	for _, kv := range req.ExtraEnv {
		if isInternalChildEnv(kv) {
			continue
		}
		out = append(out, kv)
	}
	out = append(
		out,
		// Infallible daemon-child marker (H1): set on EVERY spawned child so an
		// inner launcher never re-attaches even when its OOB channel is dead.
		envDaemonChild+"=1",
		envOOBFD+"=3", // ExtraFiles[0] → fd 3 in the child
		envOOBAuth+"="+authToken,
		envOOBCorr+"="+req.CorrelationToken,
		envOOBTool+"="+req.Tool,
		envOOBRun+"="+req.RunID,
	)
	return out
}

// isInternalChildEnv reports whether an env entry is one the daemon manages for
// its children (the OOB channel vars or the daemon-child marker) and must strip
// from any inherited/caller-supplied env before re-adding its own fresh copy —
// so a nested launch can never inherit a stale channel or spoof the marker.
func isInternalChildEnv(kv string) bool {
	return strings.HasPrefix(kv, "OBSERVER_OOB_") || strings.HasPrefix(kv, envDaemonChild+"=")
}

// setupChildEnv builds the environment for a SETUP session (CreateSetup) — the
// one daemon-child path that does NOT go through termsvc/ptyLauncher, so it
// bypasses launchChildEnv. It is the daemon's own env (a setup command like the
// Tailscale operator grant needs a sane PATH/TERM), with any internal child vars
// stripped (defence against spoofing), PLUS the infallible OBSERVER_DAEMON_CHILD
// marker. A setup session has no OOB channel (no LaunchRequest), so it carries
// ONLY the marker — not the OOB fd/auth vars — which keeps the invariant "every
// daemon child carries the marker" true for the setup path too (finding: marker
// completeness). Recursion via a setup command is currently unreachable (the
// argv is a fixed, server-derived command), so the marker here is belt-and-
// suspenders that makes the invariant claim honest rather than aspirational.
func setupChildEnv() []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		if isInternalChildEnv(kv) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, envDaemonChild+"=1")
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

// oobSessionSourceToTermrun maps a Session frame's optional Source hint onto the
// correlation Source the daemon records (CLAUDE.md #5 — a data table, not an
// if-ladder; #3 — dispatch on the frame's declared capability, never on which
// tool sent it). Only KNOWN mappings live here. An EMPTY hint is the back-compat
// known-id case (the launcher KNEW the id: claude's forced `--session-id`, a
// codex `--resume` short-circuit) → SourceOOB; a value IN this table maps to its
// class; a non-empty value NOT in this table is an evidence class this daemon
// does not know and is REFUSED (never guessed) — see termrunSourceForFrame.
var oobSessionSourceToTermrun = map[string]termrun.Source{
	termoob.SessionSourceDiscovered: termrun.SourceDiscovered,
}

// termrunSourceForFrame resolves a Session frame's Source hint to the
// correlation Source the daemon records, returning ok=false when the hint is an
// UNKNOWN non-empty value. An empty hint is the back-compat known-id echo →
// (SourceOOB, true); a hint in oobSessionSourceToTermrun maps to its class (e.g.
// "discovered" → SourceDiscovered, true); anything else — a weaker evidence
// class a NEWER launcher might emit against an OLDER daemon (in-place upgrade) —
// returns ("", false) so the caller SKIPS the correlation rather than promoting
// unknown evidence to full OOB confidence (R2-6).
func termrunSourceForFrame(frameSource string) (termrun.Source, bool) {
	if frameSource == "" {
		return termrun.SourceOOB, true
	}
	if s, ok := oobSessionSourceToTermrun[frameSource]; ok {
		return s, true
	}
	return "", false
}

// publishOOB turns a trusted OOB frame into a status-feed event (F4 consumes
// it) and, for a session-announce frame, records the run->session correlation
// at the confidence its Source hint implies (P2-1 — the ONE production caller of
// the correlation seam). A KNOWN-id echo (empty hint) records at oob confidence;
// a heuristically-DISCOVERED id (Source="discovered") records at the lower
// SourceDiscovered confidence, so a later known-id OOB echo strictly upgrades
// it. Unknown frame types are ignored (forward-compat).
func (l *ptyLauncher) publishOOB(runID, tool string, frame termoob.Frame) {
	switch frame.Type {
	case termoob.TypeSession:
		// The launcher learned the child's agent session id (by forcing it, or
		// by heuristic discovery) and echoed it on the authenticated channel.
		// Establish the run->session link at the confidence the frame's Source
		// hint implies so Snapshot's SessionForRun can surface it for "Jump in"
		// — the production path that populates termsvc's bySession map.
		if frame.Session == nil || frame.Session.SessionID == "" {
			return
		}
		source, known := termrunSourceForFrame(frame.Session.Source)
		if !known {
			// An unknown, non-empty evidence class (e.g. a newer launcher emitting
			// a weaker source string to an older daemon after an in-place upgrade).
			// SKIP the correlation entirely rather than guess a confidence for a
			// class this daemon doesn't understand — and skip the feed event too,
			// since we can't trust the id's provenance (R2-6).
			if l.logger != nil {
				l.logger.Warn("terminal oob: skipping session correlation for unknown source class",
					"run", runID, "tool", tool, "source", frame.Session.Source)
			}
			return
		}
		if l.correlate != nil {
			if err := l.correlate(context.Background(), runID, frame.Session.SessionID, source, time.Now().UTC()); err != nil && l.logger != nil {
				l.logger.Debug("terminal oob: correlate failed", "run", runID, "err", err)
			}
		}
		if l.feed != nil {
			l.feed.Publish(termfeed.Event{
				Kind: "oob:session", RunID: runID, Tool: tool, SessionID: frame.Session.SessionID,
				Trust: termfeed.TrustTrusted, At: time.Now().UTC(),
			})
		}
		return
	}
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
