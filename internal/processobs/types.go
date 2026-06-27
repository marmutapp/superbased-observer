package processobs

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// EventType is the kind of an OS process event. The first three are the
// MVP taxonomy (spec §7.1); the rest are later high-signal events (§7.2),
// declared now so the vocabulary is stable and a forward-written backend
// round-trips.
type EventType string

const (
	EventFork EventType = "process_fork"
	EventExec EventType = "process_exec"
	EventExit EventType = "process_exit"

	// EventMetrics refreshes a LIVE process's resource counters (CPU / memory /
	// disk / threads / handles) without re-resolving attribution — emitted each
	// poll for the attributed AI subtree so the dashboard shows current usage
	// and a sparkline (docs/plans/process-obs-dashboard-enhancements-2026-06-17.md).
	EventMetrics EventType = "process_metrics"

	EventNetworkConnect    EventType = "network_connect"
	EventFileWrite         EventType = "file_write"
	EventFileOpenSensitive EventType = "file_open_sensitive"
	EventPrivilegeChange   EventType = "privilege_change"
	EventNamespaceChange   EventType = "namespace_change"
)

// AttributionSource records HOW a run was attributed to a session (spec
// §9.2). Stored as data, never branched on as control flow — it lets the
// dashboard/CLI show confidence honestly rather than hiding an assumption.
type AttributionSource string

const (
	AttrNone               AttributionSource = "none"                 // unattributed
	AttrBridge             AttributionSource = "bridge"               // §9.2.1 direct pidbridge hit (the AI-tool root pid)
	AttrInherited          AttributionSource = "inherited"            // §9.2.2 nearest attributed ancestor
	AttrAdapterPID         AttributionSource = "adapter_pid"          // §9.2.3 pid+start_time from an adapter session source
	AttrActionCorrelation  AttributionSource = "action_correlation"   // §9.2.4 action-derived command row: synthesized from the tool's run_command exec record (pid 0, message-linked, no OS metrics) — see derive.go
	AttrHeuristic          AttributionSource = "heuristic"            // §9.2.5 process-name/cwd signature (low confidence only)
	AttrCrossOSCorrelation AttributionSource = "cross_os_correlation" // §5.5 Windows root matched to a session by cwd/tool/time (deferred, medium)
	AttrEnvToken           AttributionSource = "env_token"            // §5.5 P-B6: a tree-inherited session-id env var resolves to an existing session (high, namespace-independent)
)

// Confidence is the qualitative trust in an attribution (spec §9.2). It
// travels with every run so consumers never present a guess as a fact.
type Confidence string

const (
	ConfNone   Confidence = "none"
	ConfHigh   Confidence = "high"
	ConfMedium Confidence = "medium"
	ConfLow    Confidence = "low"
)

// RawEvent is the minimal fact a backend emits, before userspace
// enrichment. The kernel/event-source side keeps this tiny and versioned
// (spec §5.1); the Enricher fills the OS-specific remainder.
//
// Type discriminates which fields are meaningful: Fork uses PID/PPID/Start;
// Exec adds Exe/Argv/CWD/uids; Exit adds the exit + resource fields.
type RawEvent struct {
	Type      EventType
	Timestamp time.Time

	// Identity (all events).
	BootID         string
	PID            int
	PPID           int
	StartTimeTicks int64
	// HasStartTime is false when the backend could not read the process
	// start time. Per §9.3 such an event is counted for health but never
	// persisted as a fresh attributed run (no stable ProcessKey).
	HasStartTime bool

	// Exec fields.
	ExePath string
	Argv    []string
	CWD     string
	UID     int
	GID     int
	EUID    int
	EGID    int

	// SessionToken is an allowlisted, tree-inherited session-id env var value
	// the capturer recovered from the process environment (§5.5 P-B6 env-token
	// / EV). It is ONLY the value of a key in SessionTokenEnvKeys — never the
	// rest of the environment, which holds secrets and never leaves the
	// capturer. The Attributor resolves it to a session at HIGH confidence. It
	// round-trips through the bridge NDJSON codec unchanged (additive field).
	SessionToken string

	// Exit fields.
	ExitCode   int
	ExitSignal int

	// Resource accounting (spec §8 Resources). Best-effort, nullable
	// downstream. Captured at exec, refreshed each poll for the attributed
	// subtree (EventMetrics), and read once more best-effort at exit. CPU is
	// cumulative ms; MaxRSSBytes is the peak working set; WorkingSetBytes is the
	// current resident set; Read/WriteBytes + Read/WriteOps are cumulative disk
	// I/O; ThreadCount/HandleCount are the current compute footprint.
	HasMetrics      bool
	CPUUserMs       int64
	CPUSystemMs     int64
	MaxRSSBytes     int64
	WorkingSetBytes int64
	ReadBytes       int64
	WriteBytes      int64
	ReadOps         int64
	WriteOps        int64
	ThreadCount     int32
	HandleCount     int32

	// Security / isolation posture (P4) — userspace-enriched at exec.
	// Compact identifiers only (spec §8 Security + Isolation groups). The
	// Attributor copies these onto the run; CgroupPath is hashed at that
	// boundary (the raw path is never stored).
	SeccompMode     string
	CapabilitiesEff string
	AppArmorLabel   string
	SELinuxLabel    string
	CgroupPath      string
	ContainerID     string
	PIDNamespace    string
	MountNamespace  string
	NetNamespace    string
}

// Attribution is the answer to "which AI session (and action) owns this
// process?" — populated by the Attributor.
type Attribution struct {
	SessionID  string
	Tool       string
	ProjectID  int64
	Source     AttributionSource
	Confidence Confidence

	// ActionID / TurnIndex are the §9.2.4 message/action refinement. nil
	// until the deferred run_command -> process_exec correlation pass
	// (P3) resolves them; a run attributed only to a session is valid.
	ActionID  *int64
	TurnIndex *int
}

// ProcessRun is the userspace-enriched envelope attached to one process,
// captured at exec and updated at exit (spec §8). It is the domain type
// the Sink consumes; the store translates it into its own SQL row at the
// boundary.
//
// P1 populates Identity, Attribution, Executable, Command, Working
// directory, User, and Runtime. The Security/Isolation/Resource/Env-posture
// groups are declared now (so the type and migration are stable) and filled
// from P4 onward — they are zero/empty until then.
type ProcessRun struct {
	// Identity (§8).
	ProcessKey       string
	BootID           string
	PID              int
	PPID             int
	StartTimeTicks   int64
	ParentProcessKey string

	Attribution Attribution

	// Executable.
	ExePath     string
	ExeBasename string
	ExeDevice   string
	ExeInode    string
	ExeHash     string

	// Command (scrubbed/capped — see scrub.go).
	CWD         string
	ArgvPreview string
	ArgvHash    string
	ArgvArgc    int

	// User.
	UID      int
	GID      int
	EUID     int
	EGID     int
	Username string

	// Isolation / Security (P4).
	CgroupHash      string
	ContainerID     string
	PIDNamespace    string
	MountNamespace  string
	NetNamespace    string
	SeccompMode     string
	AppArmorLabel   string
	SELinuxLabel    string
	CapabilitiesEff string

	// Environment posture (P4) — allowlisted presence/hashes only, never
	// full env. Serialized to env_posture_json at the store boundary.
	EnvPosture map[string]string

	// Runtime.
	StartedAt  time.Time
	LastSeenAt time.Time
	ExitedAt   time.Time // zero until exit observed
	Exited     bool
	ExitCode   int
	ExitSignal int
	DurationMs int64

	// Resources (best-effort). CPU cumulative ms; MaxRSSBytes peak working set;
	// WorkingSetBytes current RSS; Read/WriteBytes + Ops cumulative disk I/O;
	// Thread/HandleCount current compute footprint. Refreshed each poll for the
	// attributed subtree via EventMetrics; the store also folds the current
	// sample into a capped in-row sparkline ring buffer.
	CPUUserMs       int64
	CPUSystemMs     int64
	MaxRSSBytes     int64
	WorkingSetBytes int64
	ReadBytes       int64
	WriteBytes      int64
	ReadOps         int64
	WriteOps        int64
	ThreadCount     int32
	HandleCount     int32

	// MetricSamples is a capped, time-throttled ring buffer of recent resource
	// readings driving the per-process sparkline. Appended on exec + each
	// EventMetrics refresh (≥ metricSampleInterval apart), capped at
	// maxMetricSamples (oldest dropped). The store serializes it to the
	// metric_samples_json column; it resets if the daemon restarts (in-memory).
	MetricSamples []MetricSample

	// IsBoundary marks init/systemd/WSL-relay processes (§9.2.6): they are
	// attribution boundaries and never propagate inheritance to children.
	IsBoundary bool
}

// MetricSample is one timestamped resource reading for the sparkline ring
// buffer (CPU cumulative ms, current working set, cumulative disk bytes).
type MetricSample struct {
	T          time.Time `json:"t"`
	CPUMs      int64     `json:"cpu_ms"`
	WorkingSet int64     `json:"ws"`
	ReadBytes  int64     `json:"rb"`
	WriteBytes int64     `json:"wb"`
}

// Attributed reports whether the run carries a session attribution.
func (r *ProcessRun) Attributed() bool {
	return r.Attribution.SessionID != "" && r.Attribution.Source != AttrNone
}

// DropReason names why an event or run was discarded; it labels the
// dropped-event health counter (spec §15 metrics).
type DropReason string

const (
	DropNoStartTime    DropReason = "no_start_time"    // §9.3: unkeyable event
	DropUnattributed   DropReason = "unattributed"     // §9.2.7: no session and capture_unattributed=false
	DropQueueOverflow  DropReason = "queue_overflow"   // §15: newest-dropped under backpressure
	DropEnrichFailed   DropReason = "enrich_failed"    // enrichment returned an error
	DropExitBeforeExec DropReason = "exit_before_exec" // exit for a pid we never saw exec/fork for
	DropSelfExcluded   DropReason = "self_excluded"    // the observer daemon's own binary — never an AI-tool worker (Options.ExcludeOwnBasenames)
)

// SessionTokenEnvKeys is the allowlist of process-environment variable names
// that carry a tool's session id, inherited by the whole process subtree
// (§5.5 P-B6 env-token). The capturer extracts ONLY these keys' values from a
// process environment — the rest of the env (which holds secrets) is never
// read out, shipped, or stored. The Attributor resolves a recovered value to a
// session by direct equality against sessions.id, attributing at HIGH
// confidence (verified 2026-06-17: CLAUDE_CODE_SESSION_ID == sessions.id).
//
// Table-driven (CLAUDE.md rule 5): add a key here per tool that exposes a
// tree-inherited env var byte-equal to the session id observer stores. The
// per-adapter discovery matrix (P-B6.0) found Claude Code is the only such
// tool today — every other tool keeps its session id in a store DB, or (Kilo's
// KILO_RUN_ID) exposes an id on a different scheme than observer's.
var SessionTokenEnvKeys = []string{"CLAUDE_CODE_SESSION_ID"}

// ProcessKey derives the stable, PID-reuse-proof key for a process
// (spec §9.3): sha256(boot_id ":" pid ":" start_time_ticks). Callers must
// only build a key when the start time is known.
func ProcessKey(bootID string, pid int, startTimeTicks int64) string {
	h := sha256.Sum256([]byte(bootID + ":" + strconv.Itoa(pid) + ":" + strconv.FormatInt(startTimeTicks, 10)))
	return hex.EncodeToString(h[:])
}
