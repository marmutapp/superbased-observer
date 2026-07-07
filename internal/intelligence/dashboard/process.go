package dashboard

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// SessionProcessResponse is the /api/session/<id>/processes payload — the
// session process tree captured by Process Observability
// (docs/process-observability.md §13.1). Drives the session-detail Processes
// drawer; the React component lives in the (external) webapp source and
// consumes this shape.
type SessionProcessResponse struct {
	SessionID string        `json:"session_id"`
	Total     int           `json:"total"`
	Roots     []ProcessNode `json:"roots"`
	// Findings are the §14 observe-only side-effect flags for this session's
	// processes (privileged exec, exec-from-tmp), derived from the envelope.
	Findings []store.ProcessFindingRow `json:"findings"`
}

// ProcessNode is one process in the tree, with its children nested.
type ProcessNode struct {
	ProcessKey  string `json:"process_key"`
	PID         int    `json:"pid"`
	PPID        int    `json:"ppid"`
	Exe         string `json:"exe"`
	ArgvPreview string `json:"argv_preview,omitempty"`
	CWD         string `json:"cwd,omitempty"`
	Source      string `json:"attribution_source"`
	Confidence  string `json:"attribution_confidence"`
	Exited      bool   `json:"exited"`
	ExitCode    int    `json:"exit_code"`
	ExitSignal  int    `json:"exit_signal,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	// StartedAt is the wall-clock instant the process was first observed
	// (process_runs.started_at, RFC3339 UTC). The drawer shows the actual
	// capture time alongside the elapsed/duration figures. Empty (zero)
	// when unknown.
	StartedAt  string `json:"started_at,omitempty"`
	IsBoundary bool   `json:"is_boundary,omitempty"`
	ActionID   *int64 `json:"action_id,omitempty"`
	TurnIndex  *int   `json:"turn_index,omitempty"`
	// Command is the run_command target that spawned this subtree (§9.2.4),
	// empty when the process wasn't correlated to an action.
	Command string `json:"command,omitempty"`
	// MessageID is the assistant message that issued the spawning command
	// (actions.message_id, e.g. an Anthropic "msg_…" id) — the message→OS-side-
	// effect join (§9.2.4 / D8). Empty when the process wasn't correlated to an
	// action (e.g. session-infrastructure processes, or a short-lived command the
	// poll backend missed).
	MessageID string `json:"message_id,omitempty"`
	// Security / isolation posture (P4) — present only when captured.
	SeccompMode     string `json:"seccomp_mode,omitempty"`
	CapabilitiesEff string `json:"capabilities_eff,omitempty"`
	AppArmorLabel   string `json:"apparmor_label,omitempty"`
	SELinuxLabel    string `json:"selinux_label,omitempty"`
	ContainerID     string `json:"container_id,omitempty"`

	// Resource metrics (migration 045) — present only when captured (Windows
	// poll capturer today). CPUMs is cumulative user+system; WorkingSetBytes is
	// current RSS, PeakRSSBytes the lifetime peak; Read/WriteBytes cumulative
	// disk I/O. MetricSamples is the sparkline ring buffer. Network is absent
	// (needs ETW, deferred).
	CPUMs           int64                     `json:"cpu_ms,omitempty"`
	WorkingSetBytes int64                     `json:"working_set_bytes,omitempty"`
	PeakRSSBytes    int64                     `json:"peak_rss_bytes,omitempty"`
	ReadBytes       int64                     `json:"read_bytes,omitempty"`
	WriteBytes      int64                     `json:"write_bytes,omitempty"`
	ThreadCount     int32                     `json:"thread_count,omitempty"`
	HandleCount     int32                     `json:"handle_count,omitempty"`
	MetricSamples   []processobs.MetricSample `json:"metric_samples,omitempty"`

	Children []ProcessNode `json:"children"`
}

// correlateMinInterval debounces the per-session correlation passes run from
// handleSessionProcesses (see Server.lastCorrelate). The Processes drawer polls
// that endpoint every few seconds while open; re-running the cross-OS + action-
// link WRITE passes on every poll scaled with the unattributed-row backlog and
// slowed the UI. ~30s freshness is plenty (a newly ingested action/session links
// on the next eligible poll) and keeps the writes off the hot poll path.
const correlateMinInterval = 30 * time.Second

// shouldCorrelate reports whether enough time has passed since the last
// correlation pass for this session to run another, recording "now" when it
// returns true. Concurrency-safe — the drawer may have several polls in flight.
func (s *Server) shouldCorrelate(sessionID string) bool {
	s.correlateMu.Lock()
	defer s.correlateMu.Unlock()
	now := s.now()
	if last, ok := s.lastCorrelate[sessionID]; ok && now.Sub(last) < correlateMinInterval {
		return false
	}
	s.lastCorrelate[sessionID] = now
	return true
}

// handleSessionProcesses serves /api/session/<id>/processes. It refreshes the
// §9.2.4 action links lazily (so a process shows the command that spawned it
// even if the action was ingested after the process event), then returns the
// attributed process tree. Empty result is a valid empty tree, not an error.
func (s *Server) handleSessionProcesses(w http.ResponseWriter, r *http.Request, sessionID string) {
	if sessionID == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	st := store.New(s.db())

	// Correlation refresh — debounced per session (the drawer polls this every
	// few seconds; these are WRITE passes). Cross-OS first (§5.5: attribute the
	// Windows bridge subtree to this session), then the §9.2.4 action links on
	// the now-attributed rows. The read below is always served fresh, so a poll
	// that skips correlation still returns current data.
	if s.shouldCorrelate(sessionID) {
		_, _ = st.CorrelateCrossOS(ctx, sessionID)
		_, _ = st.CorrelateProcessActions(ctx, sessionID)
		// Materialize action-derived command rows for the commands the OS
		// poll backend missed (sub-interval, born-and-died-between-ticks) —
		// AFTER the OS-link pass so a captured process always wins and a
		// derived row is only synthesized where no real process exists
		// (docs/process-observability.md §9.2.4).
		_, _ = st.DeriveProcessRunsFromActions(ctx, sessionID)
	}

	runs, err := st.ProcessRunsForSession(ctx, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load process runs: %v", err), http.StatusInternalServerError)
		return
	}
	cmds, err := st.ActionCommandsForSession(ctx, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load action commands: %v", err), http.StatusInternalServerError)
		return
	}
	msgIDs, err := st.ActionMessageIDsForSession(ctx, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load action message ids: %v", err), http.StatusInternalServerError)
		return
	}
	// Observe-only side-effect flags for the drawer (§13.1 / §14). Best-effort
	// — a findings error never blocks the tree. Default to an empty slice so
	// the JSON shape stays `"findings": []`.
	findings, _ := st.ProcessFindingsForSession(ctx, sessionID)
	if findings == nil {
		findings = []store.ProcessFindingRow{}
	}

	writeJSON(w, SessionProcessResponse{
		SessionID: sessionID,
		Total:     len(runs),
		Roots:     buildProcessTree(runs, cmds, msgIDs),
		Findings:  findings,
	})
}

// ProcessFindingsResponse is the /api/process/findings payload — recent
// observe-only process findings (§14) for the Security/Observability tab,
// with per-rule and per-severity rollups. NODE-LOCAL; never leaves the box.
type ProcessFindingsResponse struct {
	WindowHours int                       `json:"window_hours"`
	Total       int                       `json:"total"`
	ByRule      map[string]int            `json:"by_rule"`
	BySeverity  map[string]int            `json:"by_severity"`
	Findings    []store.ProcessFindingRow `json:"findings"`
}

// handleProcessFindings serves GET /api/process/findings?hours=N — the recent
// cross-session process-finding rollup. Findings are derived on read from the
// process_runs envelope, so this reflects current data with no write path.
func (s *Server) handleProcessFindings(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			hours = n
		}
	}
	findings, err := store.New(s.db()).RecentProcessFindings(r.Context(), time.Duration(hours)*time.Hour)
	if err != nil {
		http.Error(w, fmt.Sprintf("load process findings: %v", err), http.StatusInternalServerError)
		return
	}
	byRule := map[string]int{}
	bySeverity := map[string]int{}
	for _, f := range findings {
		byRule[f.RuleID]++
		bySeverity[f.Severity]++
	}
	if findings == nil {
		findings = []store.ProcessFindingRow{}
	}
	writeJSON(w, ProcessFindingsResponse{
		WindowHours: hours,
		Total:       len(findings),
		ByRule:      byRule,
		BySeverity:  bySeverity,
		Findings:    findings,
	})
}

// buildProcessTree assembles the nested ProcessNode forest from the flat
// run rows, ordering siblings by start time then pid. A run whose parent
// wasn't captured is promoted to a root so nothing is dropped. Always
// returns a non-nil slice so the JSON shape stays `"roots": []`.
func buildProcessTree(runs []store.ProcessRunRow, cmds, msgIDs map[int64]string) []ProcessNode {
	roots := []ProcessNode{}
	if len(runs) == 0 {
		return roots
	}
	byKey := make(map[string]store.ProcessRunRow, len(runs))
	childrenOf := make(map[string][]string)
	for _, r := range runs {
		byKey[r.ProcessKey] = r
	}
	var rootKeys []string
	for _, r := range runs {
		if r.ParentProcessKey != "" {
			if _, ok := byKey[r.ParentProcessKey]; ok {
				childrenOf[r.ParentProcessKey] = append(childrenOf[r.ParentProcessKey], r.ProcessKey)
				continue
			}
		}
		rootKeys = append(rootKeys, r.ProcessKey)
	}

	less := func(a, b string) bool {
		ra, rb := byKey[a], byKey[b]
		if !ra.StartedAt.Equal(rb.StartedAt) {
			return ra.StartedAt.Before(rb.StartedAt)
		}
		return ra.PID < rb.PID
	}
	sort.SliceStable(rootKeys, func(i, j int) bool { return less(rootKeys[i], rootKeys[j]) })

	// Guard against a parent_process_key cycle (should never happen — keys
	// are append-only — but a malformed import must not infinite-loop).
	seen := make(map[string]bool, len(runs))
	var build func(key string) ProcessNode
	build = func(key string) ProcessNode {
		r := byKey[key]
		node := nodeFromRow(r, cmds, msgIDs)
		if seen[key] {
			return node
		}
		seen[key] = true
		ch := childrenOf[key]
		sort.SliceStable(ch, func(i, j int) bool { return less(ch[i], ch[j]) })
		for _, c := range ch {
			node.Children = append(node.Children, build(c))
		}
		return node
	}
	for _, rk := range rootKeys {
		roots = append(roots, build(rk))
	}
	return roots
}

// startedAtISO renders a process run's start instant as RFC3339 UTC for the
// drawer, or "" when the time is zero (unknown).
func startedAtISO(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func nodeFromRow(r store.ProcessRunRow, cmds, msgIDs map[int64]string) ProcessNode {
	n := ProcessNode{
		ProcessKey:      r.ProcessKey,
		PID:             r.PID,
		PPID:            r.PPID,
		Exe:             r.ExeBasename,
		ArgvPreview:     r.ArgvPreview,
		CWD:             r.CWD,
		Source:          r.AttributionSource,
		Confidence:      r.AttributionConfidence,
		Exited:          r.Exited,
		ExitCode:        r.ExitCode,
		ExitSignal:      r.ExitSignal,
		DurationMs:      r.DurationMs,
		StartedAt:       startedAtISO(r.StartedAt),
		IsBoundary:      r.IsBoundary,
		ActionID:        r.ActionID,
		TurnIndex:       r.TurnIndex,
		SeccompMode:     r.SeccompMode,
		CapabilitiesEff: r.CapabilitiesEff,
		AppArmorLabel:   r.AppArmorLabel,
		SELinuxLabel:    r.SELinuxLabel,
		ContainerID:     r.ContainerID,
		CPUMs:           r.CPUUserMs + r.CPUSystemMs,
		WorkingSetBytes: r.WorkingSetBytes,
		PeakRSSBytes:    r.MaxRSSBytes,
		ReadBytes:       r.ReadBytes,
		WriteBytes:      r.WriteBytes,
		ThreadCount:     r.ThreadCount,
		HandleCount:     r.HandleCount,
		MetricSamples:   r.MetricSamples,
		Children:        []ProcessNode{},
	}
	if r.ActionID != nil {
		n.Command = cmds[*r.ActionID]
		n.MessageID = msgIDs[*r.ActionID]
	}
	return n
}
