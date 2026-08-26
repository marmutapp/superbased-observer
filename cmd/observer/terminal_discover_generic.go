// terminal_discover_generic.go — generic post-launch session discovery for
// every launchable adapter, dispatched on adapter SHAPE (WatchPaths / an
// optional CursorSemantics declaration), never on tool name (CLAUDE.md
// module-boundary rule #3).
//
// Background: `observer codex` cannot force a session id (codex has no
// `--session-id`), so codex_discover.go snapshots its rollout directory
// before the child starts and polls for the one new rollout file the child
// itself writes, announcing its session id on the trusted OOB channel at
// SourceDiscovered confidence (below claude-code's forced-id SourceOOB, but
// far better than the daemon's passive 10s cwd sweep in
// terminal_discover.go, which is the fallback for every tool that gets no
// active discovery). This file generalizes that mechanism: instead of
// reading codex's rollout envelope format directly, it asks the tool's own
// adapter — via the SAME internal/adapter.Adapter methods the watcher
// itself uses (WatchPaths / IsSessionFile / ParseSessionFile) — which new
// file appeared and what session id it carries. No adapter needs to change;
// no per-tool code lives here.
//
// Hook points: the two shared exec helpers every simple launcher already
// funnels through — runSeedOnlyLaunchSeeded (qwen.go) and runEnvLauncher
// (launch.go) — call maybeStartGenericDiscovery right after child.Start()
// and cancel it right after child.Wait() returns, mirroring codex.go's own
// F1-fixed goroutine placement exactly. Both the manually-invoked CLI path
// and the dashboard's "New Terminal"/attach PTY path exec the identical
// `observer <tool>` subcommand (see terminal_launch.go's argvModeTable), so
// hooking these two shared helpers covers both without touching
// terminal_launch.go, attach_launcher.go, or resume_launcher.go: the
// trusted OOB pipe (oob_emit_unix.go) and its daemon-side consumer
// (terminal_launch.go's drainOOB / oobSessionSourceToTermrun) are already
// fully generic — termoob.SessionSourceDiscovered maps to
// termrun.SourceDiscovered for ANY tool, and termsvc.Service.Correlate
// (internal/termsvc/termsvc.go) applies strict MAX-upgrade semantics, so a
// discovery announcement can never downgrade a stronger link a resume's
// known-id echo already established — see maybeStartGenericDiscovery's doc
// comment for why that means this hook does not need to special-case resume
// or --continue-from launches.
package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	adapterdefaults "github.com/marmutapp/superbased-observer/internal/adapter/defaults"
)

// genericDiscoverConfig tunes the poll loop. Injectable so tests can use
// tiny durations. Mirrors codexDiscoverConfig exactly.
type genericDiscoverConfig struct {
	// window bounds the total watch time after the child starts.
	window time.Duration
	// poll is the interval between watch-root scans.
	poll time.Duration
}

// defaultGenericDiscoverConfig matches codex's production timing: most
// adapters write their first session-identifying record within the first
// second or two, so the window mostly covers a slow first flush.
func defaultGenericDiscoverConfig() genericDiscoverConfig {
	return genericDiscoverConfig{window: 30 * time.Second, poll: 750 * time.Millisecond}
}

// genericDiscoverModTimeSkew is how far before the child-start stamp a
// candidate file's ModTime may fall and still be considered "new". Absorbs
// coarse filesystem timestamp granularity; the pre-start name/path snapshot
// is the primary "new file" signal, this guard only rejects clearly-stale
// (path-recycled) files. Mirrors codexRolloutModTimeSkew.
const genericDiscoverModTimeSkew = 5 * time.Second

// genericAdapterRegistry is the process-wide, memoized set of production
// adapters (the same list cmd/observer/main.go's buildWatcher registers),
// built once via adapterdefaults.Adapters(). A launcher process is not the
// daemon and has no live watcher.Registry to reuse, but the adapter set
// itself is pure/stateless, so building a local one here is cheap and safe.
var genericAdapterRegistry = sync.OnceValue(func() *adapter.Registry {
	reg := adapter.NewRegistry()
	for _, a := range adapterdefaults.Adapters() {
		reg.Register(a)
	}
	return reg
})

// resolveDiscoverableAdapter returns the adapter registered under tool, or
// nil when tool is empty, unknown, or declares no session-file watch roots
// at all (e.g. the browser-capture rail's *-web adapters, which receive
// data over a native-messaging bridge rather than a session file — there is
// nothing on disk to poll for).
func resolveDiscoverableAdapter(tool string) adapter.Adapter {
	if tool == "" {
		return nil
	}
	a := genericAdapterRegistry().Get(tool)
	if a == nil || len(a.WatchPaths()) == 0 {
		return nil
	}
	return a
}

// genericDiscoverCandidate is a newly discovered session file plus the
// identity read from parsing it.
type genericDiscoverCandidate struct {
	path        string
	sessionID   string
	projectRoot string // "" when the adapter's ParseResult didn't carry one
}

// snapshotGenericSessionFiles records the set of paths a.IsSessionFile
// currently accepts under a's watch roots. Taken BEFORE the child starts so
// the child's own new session file — necessarily absent from this set — is
// detectable without relying on wall clocks. A watch root may be a
// directory or a single file (e.g. aider's per-repo transcript path);
// filepath.WalkDir handles both. Best-effort: unreadable roots are skipped.
func snapshotGenericSessionFiles(a adapter.Adapter) map[string]struct{} {
	existing := make(map[string]struct{})
	for _, root := range a.WatchPaths() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // skip unreadable entries, keep walking
			}
			if a.IsSessionFile(path) {
				existing[path] = struct{}{}
			}
			return nil
		})
	}
	return existing
}

// scanNewGenericSessionFiles returns session-file paths that appeared AFTER
// the pre-start snapshot: a path not in preexisting, whose ModTime is at or
// after startedAt (within genericDiscoverModTimeSkew). Mirrors
// scanNewCodexRollouts, generalized to any adapter's IsSessionFile.
func scanNewGenericSessionFiles(a adapter.Adapter, preexisting map[string]struct{}, startedAt time.Time) []string {
	var out []string
	for _, root := range a.WatchPaths() {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // skip unreadable entries, keep walking
			}
			if !a.IsSessionFile(path) {
				return nil
			}
			if _, seen := preexisting[path]; seen {
				return nil // pre-existing file (appended-to), not this run's
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			if info.ModTime().Before(startedAt.Add(-genericDiscoverModTimeSkew)) {
				return nil
			}
			out = append(out, path)
			return nil
		})
	}
	return out
}

// resolveGenericCandidate turns a newly discovered path into a candidate, or
// reports ok=false when it isn't (yet, or ever) usable.
//
// This is the ONE per-adapter-shape branch in the engine, and it dispatches
// on the adapter.CursorSemantics capability (a declared FileCursorSemantics
// kind), never on tool name. Two kinds are excluded:
//
//   - CursorWatermark: an opaque high-water mark over a monolithic store
//     (SQLite row id, `MAX(time_updated)`) shared by EVERY session that
//     tool has ever run. A "new file" appearing under such a root proves
//     nothing about a new session — the file usually already existed
//     (excluded by the pre-start snapshot already) and even a genuinely
//     fresh one holds every session that tool will ever record, not just
//     this run's.
//   - CursorEncrypted: a file the adapter tracks but cannot decode on this
//     host (e.g. Antigravity desktop's OSCrypt-gated `.pb` store) — no
//     session id can be read from it without adapter-internal key
//     material this engine does not have.
//
// An adapter that doesn't implement CursorSemantics, or reports any other
// kind for path, is byte-offset shaped by default (the historical
// assumption every pre-CursorSemantics adapter still gets) — eligible.
func resolveGenericCandidate(ctx context.Context, a adapter.Adapter, path string) (genericDiscoverCandidate, bool) {
	if cs, ok := a.(adapter.CursorSemantics); ok {
		switch cs.CursorSemanticsFor(path).Kind {
		case adapter.CursorWatermark, adapter.CursorEncrypted:
			return genericDiscoverCandidate{}, false
		}
	}
	res, err := a.ParseSessionFile(ctx, path, 0)
	if err != nil {
		return genericDiscoverCandidate{}, false
	}
	sessionID, projectRoot := firstSessionIdentity(res)
	if sessionID == "" {
		return genericDiscoverCandidate{}, false
	}
	return genericDiscoverCandidate{path: path, sessionID: sessionID, projectRoot: projectRoot}, true
}

// firstSessionIdentity reads the session id + project root off the first
// ToolEvent or TokenEvent in res that carries a non-empty SessionID. Every
// adapter's ParseResult carries these per-event fields regardless of the
// tool's on-disk storage shape (JSONL, structured JSON, whatever) — this is
// the generic, adapter-agnostic identity seam the engine relies on so it
// never needs tool-specific parsing of its own.
func firstSessionIdentity(res adapter.ParseResult) (sessionID, projectRoot string) {
	for _, e := range res.ToolEvents {
		if e.SessionID != "" {
			return e.SessionID, e.ProjectRoot
		}
	}
	for _, e := range res.TokenEvents {
		if e.SessionID != "" {
			return e.SessionID, e.ProjectRoot
		}
	}
	return "", ""
}

// cwdUnderProjectRoot reports whether cwd denotes projectRoot itself, or a
// directory under it. Unknown information (either side empty) never
// excludes — the candidate still counts toward the ambiguity check, the
// same "can't exclude, so it still counts" rule codex's cwd corroboration
// uses for a candidate with no cwd at all.
//
// Unlike codex's exact-match cwd comparison (codex's own session_meta
// always carries the raw process cwd), a generic adapter's ParseResult
// ProjectRoot is frequently git-root-resolved (internal/git) — for a launch
// from a project subdirectory that would never equal the raw cwd exactly,
// so exact match here would wrongly exclude the right candidate. Symlink
// resolution is attempted on both sides; a resolution failure falls back to
// the cleaned-path comparison.
func cwdUnderProjectRoot(cwd, projectRoot string) bool {
	if cwd == "" || projectRoot == "" {
		return true
	}
	cc, rc := filepath.Clean(cwd), filepath.Clean(projectRoot)
	if resolved, err := filepath.EvalSymlinks(cc); err == nil {
		cc = filepath.Clean(resolved)
	}
	if resolved, err := filepath.EvalSymlinks(rc); err == nil {
		rc = filepath.Clean(resolved)
	}
	if cc == rc {
		return true
	}
	return strings.HasPrefix(cc, rc+string(filepath.Separator))
}

// selectDiscoveredGenericSession applies the same never-guess abstention
// policy as selectDiscoveredCodexSession: exactly one surviving candidate
// after cwd corroboration → its id; zero → nothing yet; two or more → the
// caller abstains rather than picking.
func selectDiscoveredGenericSession(cands []genericDiscoverCandidate, targetCwd string) (string, int) {
	kept := make([]genericDiscoverCandidate, 0, len(cands))
	for _, c := range cands {
		if c.sessionID == "" {
			continue
		}
		if !cwdUnderProjectRoot(targetCwd, c.projectRoot) {
			continue
		}
		kept = append(kept, c)
	}
	if len(kept) == 1 {
		return kept[0].sessionID, 1
	}
	return "", len(kept)
}

// runGenericDiscovery watches a's watch roots for this run's new session
// file across the FULL window and announces its session id via announce
// (wired to announceDiscoveredOOBSession in production) ONLY at window
// close, and ONLY when exactly one candidate survived cwd corroboration
// over the whole window. Structurally identical to runCodexDiscovery — see
// that function's doc comment for the full R2-1 / F1 reasoning (an
// unrelated concurrent same-cwd session racing ahead, and why the decision
// is deferred to window close rather than made mid-window).
//
// Candidates already resolved (a path whose ParseSessionFile succeeded) are
// cached by path so a slow-growing file is parsed at most once per run;
// paths that aren't yet resolvable, or are shape-excluded, are retried
// (cheaply — a CursorSemantics check plus a WalkDir pass) on every poll,
// since a shape-excluded file could in principle transition (an adapter
// implementing CursorSemantics per-path, not per-adapter) and a not-yet-
// written file may become parseable moments later.
func runGenericDiscovery(ctx context.Context, a adapter.Adapter, preexisting map[string]struct{}, startedAt time.Time, targetCwd string, cfg genericDiscoverConfig, announce func(string)) {
	deadline := time.Now().Add(cfg.window)
	resolved := make(map[string]genericDiscoverCandidate)
	for {
		if ctx.Err() != nil {
			return // child exited: forgo discovery rather than risk a cut-short guess
		}
		for _, path := range scanNewGenericSessionFiles(a, preexisting, startedAt) {
			if _, ok := resolved[path]; ok {
				continue
			}
			if cand, ok := resolveGenericCandidate(ctx, a, path); ok {
				resolved[path] = cand
			}
		}
		if time.Now().After(deadline) {
			break
		}
		timer := time.NewTimer(cfg.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	// F1: cancel wins over a completed final scan — see runCodexDiscovery's
	// identical re-check for the full reasoning.
	if ctx.Err() != nil {
		return
	}
	cands := make([]genericDiscoverCandidate, 0, len(resolved))
	for _, c := range resolved {
		cands = append(cands, c)
	}
	if id, count := selectDiscoveredGenericSession(cands, targetCwd); count == 1 {
		announce(id)
	}
}

// maybeStartGenericDiscovery starts best-effort generic session discovery
// for a freshly-launched tool and returns the CancelFunc to call the instant
// the child exits (mirrors codex.go's discCancel — call it right after
// child.Wait() returns, before any other post-flight work, so a window cut
// short by child exit never announces a candidate that only looked unique
// because the scan stopped early). Returns nil when discovery did not start
// — the caller must nil-check before calling it.
//
// Discovery starts only when the trusted OOB channel is live (a daemon-
// spawned launch — oobChannelActive(); a bare manual invocation has no pipe
// to announce on, so starting the goroutine would be pure waste) and tool
// resolves to an adapter with at least one session-file watch root
// (resolveDiscoverableAdapter). Every other reason discovery might not
// produce anything — no new file within the window, an ambiguous cwd, an
// adapter shape resolveGenericCandidate excludes — is handled inside
// runGenericDiscovery by silently not announcing; the daemon's passive
// terminal_discover.go sweep remains the fallback in every one of those
// cases, for every tool, exactly as it was before this file existed.
//
// It deliberately does NOT special-case a native --resume or
// --continue-from launch the way codex.go's discoverSession gate does.
// Both runSeedOnlyLaunchSeeded and runEnvLauncher are reached identically
// whether or not the caller already resolved a KNOWN session id and echoed
// it via announceOOBSession (resume_launcher.go's applyLauncherResume does
// this in caller scope, before either shared helper runs) — and neither
// helper's signature carries resume/continueFrom state this function could
// consult even if it wanted to skip. That's safe, not just tolerated:
// internal/termsvc/termsvc.go's Correlate applies strict MAX-upgrade
// semantics ("a weaker later observation never downgrades a stronger
// established link"), so a SourceDiscovered announcement arriving after an
// already-recorded SourceOOB known-id link is a guaranteed no-op, not a
// downgrade risk. The cost is a genuinely wasted ~30s poll loop on a resume
// launch — a real but small inefficiency, traded for not having to thread
// resume/continueFrom state through two shared helpers whose callers
// (13+ launcher files, several outside this work-stream's territory) must
// not change.
//
// dir is the child's working directory, using the same "" -> caller's own
// cwd convention as runSeedOnlyLaunchSeeded's / envLauncherSpec's dir field.
func maybeStartGenericDiscovery(ctx context.Context, tool, dir string) context.CancelFunc {
	if !oobChannelActive() || tool == "" {
		return nil
	}
	a := resolveDiscoverableAdapter(tool)
	if a == nil {
		return nil
	}
	preexisting := snapshotGenericSessionFiles(a)
	targetCwd := dir
	if targetCwd == "" {
		if wd, err := os.Getwd(); err == nil {
			targetCwd = wd
		}
	}
	startedAt := time.Now()
	discCtx, cancel := context.WithCancel(ctx)
	go runGenericDiscovery(discCtx, a, preexisting, startedAt, targetCwd, defaultGenericDiscoverConfig(), announceDiscoveredOOBSession)
	return cancel
}
