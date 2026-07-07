package processobs

import "time"

// Seed is a session attribution resolved from an external source (the
// pidbridge, or an adapter-specific session DB). The caller injects the
// lookup; this package never imports internal/pidbridge or internal/store.
type Seed struct {
	SessionID  string
	Tool       string
	ProjectID  int64
	Source     AttributionSource
	Confidence Confidence
}

// SeedLookup resolves a pid to its session seed. The bridged pid is the
// AI-tool ROOT process (spec §9.1.1); a clean miss returns ok=false. It
// must be cheap and side-effect free from this package's view.
type SeedLookup func(pid int) (Seed, bool)

// TokenLookup resolves a session-id env token (a value the capturer recovered
// from SessionTokenEnvKeys) to its session seed — the §5.5 P-B6 env-token (EV)
// path. A hit means a session with that id exists; the seed carries its
// tool/project so the attribution is complete. Unlike SeedLookup it is keyed
// on the session id directly, so it is namespace-independent — it resolves a
// Windows-captured process to a WSL session without any pid plumbing. A clean
// miss returns ok=false. The caller injects it (the store's SessionSeedByID);
// this package never imports internal/store.
type TokenLookup func(token string) (Seed, bool)

// Change is what Observe did to the tracked tree, telling the Observer
// whether (and how) to persist.
type Change int

const (
	// ChangeNone: tree bookkeeping only, nothing to persist (e.g. a fork
	// before exec, or an event for an unknown process).
	ChangeNone Change = iota
	// ChangeCreated: a process reached exec — persist it (initial upsert).
	ChangeCreated
	// ChangeUpdated: a tracked process exited — persist the update.
	ChangeUpdated
)

// DefaultBoundaryBasenames are the executable basenames treated as
// attribution boundaries (spec §9.2.6): attribution never flows through
// them. pid 1 is always a boundary regardless of name. Table-driven so the
// set is data, not a conditional ladder (CLAUDE.md rule 5).
var DefaultBoundaryBasenames = map[string]bool{
	"init":         true, // sysv / WSL2 /init (also pid 1)
	"systemd":      true,
	"systemd-init": true,
	"wsl":          true, // wsl.exe relay
	"wslhost":      true,
	"wslrelay":     true,
	"wslservice":   true,
	"relay":        true,
	"login":        true,
	"sshd":         true,
}

// Attributor maintains the in-memory process tree and resolves the §9.2
// attribution rules. It is pure: the only external knowledge it consults
// is the injected SeedLookup. Not safe for concurrent use — the Observer
// drives it from a single goroutine.
type Attributor struct {
	seed       SeedLookup
	tokenSeed  TokenLookup
	scrub      *FieldScrubber
	boundaries map[string]bool

	runs    map[string]*ProcessRun // processKey -> tracked run
	livePID map[int]string         // pid -> current processKey occupying it
}

// NewAttributor builds an Attributor. seed may be nil (everything is then
// unattributed unless inherited, which also yields none). scrub may be nil
// (a default no-cap, no-redact scrubber is used — not recommended for
// production). boundaries defaults to DefaultBoundaryBasenames when nil.
func NewAttributor(seed SeedLookup, scrub *FieldScrubber, boundaries map[string]bool) *Attributor {
	if scrub == nil {
		scrub = &FieldScrubber{ArgvMode: "preview"}
	}
	if boundaries == nil {
		boundaries = DefaultBoundaryBasenames
	}
	return &Attributor{
		seed:       seed,
		scrub:      scrub,
		boundaries: boundaries,
		runs:       make(map[string]*ProcessRun),
		livePID:    make(map[int]string),
	}
}

// SetTokenLookup installs the §5.5 P-B6 env-token (EV) resolution seam: a
// process whose captured session-token env var resolves to an existing session
// is attributed at HIGH confidence, source AttrEnvToken. nil (the default)
// disables EV — the Attributor then behaves exactly as before, so the existing
// callers are unaffected (CLAUDE.md rule 6: additive, not invasive).
func (a *Attributor) SetTokenLookup(fn TokenLookup) { a.tokenSeed = fn }

// Tracked reports how many live process runs are currently held — used by
// the Observer to cap memory and by tests.
func (a *Attributor) Tracked() int { return len(a.runs) }

// InAISubtree reports whether run is, or descends from, a distinctive AI-tool
// launcher (IsAIToolLauncher) in the live tree. The Observer consults it to
// capture UNATTRIBUTED AI subtrees on a native host — codex/cursor/… that get
// no pid-seed (no pidbridge) and no env-token hit — so the deferred
// CorrelateCrossOS cwd pass can later join them to a session, WITHOUT
// persisting the whole unattributed process table (that volume is the reason
// CaptureUnattributed stays off by default). Bounded by a hop cap so a cyclic
// parent chain can never spin. Pure tree-read; no I/O.
func (a *Attributor) InAISubtree(run *ProcessRun) bool {
	if run == nil {
		return false
	}
	if IsAIToolLauncher(run.ExePath) {
		return true
	}
	const maxHops = 64
	key := run.ParentProcessKey
	for hops := 0; hops < maxHops && key != ""; hops++ {
		p := a.runs[key]
		if p == nil {
			return false
		}
		if IsAIToolLauncher(p.ExePath) {
			return true
		}
		key = p.ParentProcessKey
	}
	return false
}

// Observe folds one (already-enriched) RawEvent into the tree and returns
// the affected run plus what changed. env is the process environment for
// posture capture (may be nil); it is only consulted on exec.
func (a *Attributor) Observe(ev RawEvent, env map[string]string) (*ProcessRun, Change) {
	switch ev.Type {
	case EventFork:
		return a.fork(ev)
	case EventExec:
		return a.exec(ev, env)
	case EventExit:
		return a.exit(ev)
	case EventMetrics:
		return a.metrics(ev)
	default:
		return nil, ChangeNone
	}
}

// metricSampleInterval throttles sparkline ring-buffer appends; maxMetricSamples
// caps the buffer (oldest dropped). ~15s × 60 = ~15 min of trend at a glance.
const (
	metricSampleInterval = 15 * time.Second
	maxMetricSamples     = 60
)

// metrics folds an EventMetrics refresh into an already-tracked run: it updates
// the resource counters and appends a sparkline sample WITHOUT re-resolving
// attribution (the run keeps its session/source). A no-op (ChangeNone) when the
// process isn't tracked — e.g. evicted, or one we never saw exec for.
func (a *Attributor) metrics(ev RawEvent) (*ProcessRun, Change) {
	if !ev.HasStartTime {
		return nil, ChangeNone
	}
	run := a.runs[ProcessKey(ev.BootID, ev.PID, ev.StartTimeTicks)]
	if run == nil {
		return nil, ChangeNone
	}
	if !ev.HasMetrics {
		return nil, ChangeNone
	}
	applyMetrics(run, &ev)
	appendMetricSample(run, &ev)
	run.LastSeenAt = ev.Timestamp
	return run, ChangeUpdated
}

// applyMetrics copies resource counters from a (metrics-bearing) event onto a
// run. Cumulative counters (CPU, disk bytes/ops) take the latest reading — they
// only grow; MaxRSSBytes keeps the peak; working set / threads / handles take
// the current value. The caller guarantees ev.HasMetrics.
func applyMetrics(run *ProcessRun, ev *RawEvent) {
	run.CPUUserMs = ev.CPUUserMs
	run.CPUSystemMs = ev.CPUSystemMs
	run.WorkingSetBytes = ev.WorkingSetBytes
	if ev.MaxRSSBytes > run.MaxRSSBytes {
		run.MaxRSSBytes = ev.MaxRSSBytes
	}
	run.ReadBytes = ev.ReadBytes
	run.WriteBytes = ev.WriteBytes
	run.ReadOps = ev.ReadOps
	run.WriteOps = ev.WriteOps
	run.ThreadCount = ev.ThreadCount
	run.HandleCount = ev.HandleCount
}

// appendMetricSample appends a throttled sparkline point to the run's ring
// buffer: a fresh point only when ≥ metricSampleInterval since the last (always
// for the first), else it refreshes the last point in place so "current" stays
// live without growing the buffer. Capped at maxMetricSamples (oldest dropped).
func appendMetricSample(run *ProcessRun, ev *RawEvent) {
	s := MetricSample{
		T:          ev.Timestamp,
		CPUMs:      ev.CPUUserMs + ev.CPUSystemMs,
		WorkingSet: ev.WorkingSetBytes,
		ReadBytes:  ev.ReadBytes,
		WriteBytes: ev.WriteBytes,
	}
	if n := len(run.MetricSamples); n > 0 {
		last := run.MetricSamples[n-1].T
		if !ev.Timestamp.IsZero() && !last.IsZero() && ev.Timestamp.Sub(last) < metricSampleInterval {
			// Refresh the current bucket's values in place but KEEP its start
			// timestamp, so the throttle measures from the last APPENDED point —
			// otherwise advancing T each poll resets the interval and a second
			// point never accrues.
			s.T = last
			run.MetricSamples[n-1] = s
			return
		}
	}
	run.MetricSamples = append(run.MetricSamples, s)
	if len(run.MetricSamples) > maxMetricSamples {
		run.MetricSamples = run.MetricSamples[len(run.MetricSamples)-maxMetricSamples:]
	}
}

// fork records a child process. We do NOT persist at fork (spec §8: the
// envelope is captured at exec) — we only seed the tree so the child can
// inherit attribution and so exec can find the node. Requires a start time
// to build a stable key (§9.3); without one the event is dropped upstream.
func (a *Attributor) fork(ev RawEvent) (*ProcessRun, Change) {
	if !ev.HasStartTime {
		return nil, ChangeNone
	}
	childKey := ProcessKey(ev.BootID, ev.PID, ev.StartTimeTicks)
	run := &ProcessRun{
		ProcessKey:     childKey,
		BootID:         ev.BootID,
		PID:            ev.PID,
		PPID:           ev.PPID,
		StartTimeTicks: ev.StartTimeTicks,
		StartedAt:      ev.Timestamp,
		LastSeenAt:     ev.Timestamp,
	}
	if parentKey, ok := a.livePID[ev.PPID]; ok {
		run.ParentProcessKey = parentKey
		if parent := a.runs[parentKey]; parent != nil {
			run.Attribution = inherit(parent)
		}
	}
	a.runs[childKey] = run
	a.livePID[ev.PID] = childKey
	return run, ChangeNone
}

// exec enriches a process with its executable/command/identity and
// resolves attribution. This is the persist point (ChangeCreated).
func (a *Attributor) exec(ev RawEvent, env map[string]string) (*ProcessRun, Change) {
	if !ev.HasStartTime {
		return nil, ChangeNone
	}
	key := ProcessKey(ev.BootID, ev.PID, ev.StartTimeTicks)
	run := a.runs[key]
	if run == nil {
		// exec without a prior fork (process root, or we started mid-stream).
		run = &ProcessRun{
			ProcessKey:     key,
			BootID:         ev.BootID,
			PID:            ev.PID,
			PPID:           ev.PPID,
			StartTimeTicks: ev.StartTimeTicks,
			StartedAt:      ev.Timestamp,
		}
		if parentKey, ok := a.livePID[ev.PPID]; ok {
			run.ParentProcessKey = parentKey
			if parent := a.runs[parentKey]; parent != nil {
				run.Attribution = inherit(parent)
			}
		}
		a.runs[key] = run
		a.livePID[ev.PID] = key
	}
	run.LastSeenAt = ev.Timestamp

	// Executable + command (scrubbed/capped/hashed).
	run.ExePath = a.scrub.ScrubPath(ev.ExePath)
	run.ExeBasename = basename(ev.ExePath)
	run.CWD = a.scrub.ScrubPath(ev.CWD)
	run.ArgvPreview, run.ArgvHash, run.ArgvArgc = a.scrub.ScrubArgv(ev.Argv)
	run.UID, run.GID, run.EUID, run.EGID = ev.UID, ev.GID, ev.EUID, ev.EGID
	if env != nil {
		run.EnvPosture = a.scrub.EnvPosture(env)
	}

	// Security / isolation posture (P4) — compact identifiers copied as-is;
	// the cgroup path is reduced to a hash so a long raw path never lands
	// (spec §8 Isolation).
	run.SeccompMode = ev.SeccompMode
	run.CapabilitiesEff = ev.CapabilitiesEff
	run.AppArmorLabel = ev.AppArmorLabel
	run.SELinuxLabel = ev.SELinuxLabel
	run.ContainerID = ev.ContainerID
	run.PIDNamespace = ev.PIDNamespace
	run.MountNamespace = ev.MountNamespace
	run.NetNamespace = ev.NetNamespace
	if ev.CgroupPath != "" {
		run.CgroupHash = HashString(ev.CgroupPath)
	}

	if ev.HasMetrics {
		applyMetrics(run, &ev)
		appendMetricSample(run, &ev)
	}

	a.resolveAttribution(run, ev.SessionToken)
	return run, ChangeCreated
}

// resolveAttribution applies the §9.2 ordered rules to a run at exec time.
// Boundary first (resets + stops inheritance), then the strongest direct
// identity wins, otherwise whatever was inherited at fork survives. Order of
// the direct-identity checks:
//
//  1. env-token (§5.5 P-B6) — a session-id env var that resolves to an existing
//     session. Checked FIRST because it is namespace-independent: it identifies
//     the session directly, so it must preempt a (possibly colliding) pid seed
//     across the OS boundary, where a Windows-captured pid can numerically
//     collide with an unrelated WSL pidbridge entry. On a native host it agrees
//     with the pid seed (same session), so order is harmless there.
//  2. pid seed (§9.2.1/3) — a direct pidbridge / adapter hit on the root pid.
//
// sessionToken is the value the capturer recovered from SessionTokenEnvKeys
// (empty for most processes / non-EV tools).
func (a *Attributor) resolveAttribution(run *ProcessRun, sessionToken string) {
	if run.PID == 1 || a.boundaries[run.ExeBasename] {
		run.IsBoundary = true
		run.Attribution = Attribution{Source: AttrNone, Confidence: ConfNone}
		// A boundary is not the end of the story only if it is itself a
		// directly-identified AI-tool root — fall through to the direct checks,
		// which (pathologically) would re-attribute and clear the boundary.
	}
	if a.tokenSeed != nil && sessionToken != "" {
		if s, ok := a.tokenSeed(sessionToken); ok && s.SessionID != "" {
			run.IsBoundary = false
			run.Attribution = Attribution{
				SessionID:  s.SessionID,
				Tool:       s.Tool,
				ProjectID:  s.ProjectID,
				Source:     orDefault(s.Source, AttrEnvToken),
				Confidence: orDefaultConf(s.Confidence, ConfHigh),
			}
			return
		}
	}
	if a.seed != nil {
		if s, ok := a.seed(run.PID); ok && s.SessionID != "" {
			run.IsBoundary = false
			run.Attribution = Attribution{
				SessionID:  s.SessionID,
				Tool:       s.Tool,
				ProjectID:  s.ProjectID,
				Source:     orDefault(s.Source, AttrBridge),
				Confidence: orDefaultConf(s.Confidence, ConfHigh),
			}
			return
		}
	}
	// No direct identity: keep the inherited attribution (set at fork), unless
	// this is a boundary (already reset above).
}

// exit finalizes a process and detaches it from the tree (bounding memory
// to live processes). Returns the finished run for the update upsert, or
// ChangeNone if we never tracked it (e.g. exit before any fork/exec we saw).
func (a *Attributor) exit(ev RawEvent) (*ProcessRun, Change) {
	key := ""
	if ev.HasStartTime {
		key = ProcessKey(ev.BootID, ev.PID, ev.StartTimeTicks)
	}
	if key == "" || a.runs[key] == nil {
		if lk, ok := a.livePID[ev.PID]; ok {
			key = lk
		}
	}
	run := a.runs[key]
	if run == nil {
		return nil, ChangeNone
	}
	run.Exited = true
	run.ExitedAt = ev.Timestamp
	run.LastSeenAt = ev.Timestamp
	run.ExitCode = ev.ExitCode
	run.ExitSignal = ev.ExitSignal
	if !run.StartedAt.IsZero() && !ev.Timestamp.IsZero() {
		run.DurationMs = ev.Timestamp.Sub(run.StartedAt).Milliseconds()
	}
	if ev.HasMetrics {
		applyMetrics(run, &ev)
		appendMetricSample(run, &ev)
	}

	// Detach: free the pid and stop tracking. The returned pointer stays
	// valid for the Observer to persist; we just no longer hold it.
	delete(a.runs, key)
	if a.livePID[ev.PID] == key {
		delete(a.livePID, ev.PID)
	}
	return run, ChangeUpdated
}

// EvictOldestLive drops the oldest-started tracked run when the tree
// exceeds max, returning the number evicted. A bound for never-exiting
// processes (spec §15 high-volume handling); the Observer calls it after
// each batch. max <= 0 disables eviction.
func (a *Attributor) EvictOldestLive(max int) int {
	if max <= 0 || len(a.runs) <= max {
		return 0
	}
	evicted := 0
	for len(a.runs) > max {
		var oldestKey string
		var oldest time.Time
		for k, r := range a.runs {
			if oldestKey == "" || r.StartedAt.Before(oldest) {
				oldestKey, oldest = k, r.StartedAt
			}
		}
		if oldestKey == "" {
			break
		}
		r := a.runs[oldestKey]
		delete(a.runs, oldestKey)
		if a.livePID[r.PID] == oldestKey {
			delete(a.livePID, r.PID)
		}
		evicted++
	}
	return evicted
}

// inherit copies a parent's attribution to a child as AttrInherited,
// preserving confidence but never inheriting from a boundary (§9.2.6) or
// an unattributed parent.
func inherit(parent *ProcessRun) Attribution {
	if parent.IsBoundary || parent.Attribution.SessionID == "" {
		return Attribution{Source: AttrNone, Confidence: ConfNone}
	}
	return Attribution{
		SessionID:  parent.Attribution.SessionID,
		Tool:       parent.Attribution.Tool,
		ProjectID:  parent.Attribution.ProjectID,
		Source:     AttrInherited,
		Confidence: parent.Attribution.Confidence,
	}
}

func orDefault(s, def AttributionSource) AttributionSource {
	if s == "" {
		return def
	}
	return s
}

func orDefaultConf(c, def Confidence) Confidence {
	if c == "" {
		return def
	}
	return c
}

// basename returns the final path element of an executable path, handling
// both '/' and '\\' separators without importing path/filepath semantics
// that differ by host OS (process paths are remote-OS shaped).
func basename(p string) string {
	if p == "" {
		return ""
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
