package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/processobs"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestShouldCorrelateDebounce pins the per-session correlation debounce (A1):
// the Processes drawer polls every few seconds, so the WRITE passes must run at
// most once per correlateMinInterval per session, independently per session.
func TestShouldCorrelateDebounce(t *testing.T) {
	now := time.Now().UTC()
	s := &Server{
		lastCorrelate: map[string]time.Time{},
		now:           func() time.Time { return now },
	}
	if !s.shouldCorrelate("a") {
		t.Fatal("first call for a session should correlate")
	}
	if s.shouldCorrelate("a") {
		t.Error("second call within the interval should be debounced")
	}
	if !s.shouldCorrelate("b") {
		t.Error("a different session should correlate independently")
	}
	now = now.Add(correlateMinInterval + time.Second)
	if !s.shouldCorrelate("a") {
		t.Error("after the interval elapses, the session should correlate again")
	}
}

func dashProcRun(key, parent, sess string, pid int, exe, argv string, started time.Time) processobs.ProcessRun {
	return processobs.ProcessRun{
		ProcessKey:       key,
		ParentProcessKey: parent,
		BootID:           "boot",
		PID:              pid,
		StartTimeTicks:   int64(pid) * 100,
		Attribution: processobs.Attribution{
			SessionID: sess, Tool: "claude-code",
			Source: processobs.AttrInherited, Confidence: processobs.ConfHigh,
		},
		ExeBasename: exe,
		ArgvPreview: argv,
		StartedAt:   started,
		LastSeenAt:  started,
		Exited:      true,
		DurationMs:  1200,
	}
}

// TestHandleSessionProcesses pins the /api/session/<id>/processes drawer
// payload: the attributed tree assembles correctly AND the lazy §9.2.4
// correlation labels the subtree with the spawning command.
func TestHandleSessionProcesses(t *testing.T) {
	s, _ := newTestServer(t) // seeds session "sA"
	ctx := context.Background()
	st := store.New(s.db())

	base := time.Now().UTC().Add(-30 * time.Minute)
	// A run_command action "npm test" at turn 3.
	if _, err := st.Ingest(ctx, []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "run1", SessionID: "sA",
		ProjectRoot: t.TempDir(), Timestamp: base, Tool: models.ToolClaudeCode,
		ActionType: models.ActionRunCommand, Target: "npm test", TurnIndex: 3, Success: true,
	}}, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest run_command: %v", err)
	}

	// Process tree sh → npm → node, spawned ~1s after the action.
	runs := []processobs.ProcessRun{
		dashProcRun("k_sh", "", "sA", 100, "bash", "bash -c npm test", base.Add(time.Second)),
		dashProcRun("k_npm", "k_sh", "sA", 200, "npm", "npm test", base.Add(time.Second)),
		dashProcRun("k_node", "k_npm", "sA", 300, "node", "node x.js", base.Add(2*time.Second)),
	}
	if _, err := st.PersistRuns(ctx, runs); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA/processes", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET processes: %d — %s", rr.Code, rr.Body.String())
	}

	var resp SessionProcessResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v — %s", err, rr.Body.String())
	}
	if resp.SessionID != "sA" || resp.Total != 3 {
		t.Fatalf("resp = %+v, want sA/3", resp)
	}
	if len(resp.Roots) != 1 {
		t.Fatalf("roots = %d, want 1 (bash root)", len(resp.Roots))
	}
	root := resp.Roots[0]
	if root.Exe != "bash" || len(root.Children) != 1 {
		t.Fatalf("root = %+v, want bash with 1 child", root)
	}
	npm := root.Children[0]
	if npm.Exe != "npm" || len(npm.Children) != 1 || npm.Children[0].Exe != "node" {
		t.Fatalf("npm node mis-nested: %+v", npm)
	}
	// The §9.2.4 correlation labeled the subtree with the spawning command.
	if root.Command != "npm test" {
		t.Errorf("root.Command = %q, want 'npm test' (correlated)", root.Command)
	}
	if root.ActionID == nil || root.TurnIndex == nil || *root.TurnIndex != 3 {
		t.Errorf("root action link = id:%v turn:%v, want set/turn 3", root.ActionID, root.TurnIndex)
	}
}

// TestHandleProcessFindings pins the §14 findings surfaces: the recent
// cross-session rollup (/api/process/findings) and the per-session drawer
// flags both derive privileged_exec + executable_from_tmp from the envelope.
func TestHandleProcessFindings(t *testing.T) {
	s, _ := newTestServer(t) // seeds session "sA"
	ctx := context.Background()
	st := store.New(s.db())

	now := time.Now().UTC().Add(-10 * time.Minute)
	mk := func(key string, pid int, exe, base string, uid, euid int) processobs.ProcessRun {
		return processobs.ProcessRun{
			ProcessKey: key, BootID: "boot", PID: pid, StartTimeTicks: int64(pid) * 100,
			Attribution: processobs.Attribution{
				SessionID: "sA", Tool: "claude-code",
				Source: processobs.AttrBridge, Confidence: processobs.ConfHigh,
			},
			ExePath: exe, ExeBasename: base, UID: uid, EUID: euid,
			StartedAt: now, LastSeenAt: now,
		}
	}
	runs := []processobs.ProcessRun{
		mk("p_norm", 100, "/usr/bin/node", "node", 1000, 1000),
		mk("p_priv", 101, "/usr/bin/sudo", "sudo", 1000, 0),
		mk("p_tmp", 102, "/tmp/x/payload", "payload", 1000, 1000),
	}
	if _, err := st.PersistRuns(ctx, runs); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	// Recent rollup endpoint.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/process/findings?hours=24", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET findings: %d — %s", rr.Code, rr.Body.String())
	}
	var resp ProcessFindingsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v — %s", err, rr.Body.String())
	}
	if resp.Total != 2 {
		t.Fatalf("total findings = %d, want 2: %+v", resp.Total, resp.Findings)
	}
	if resp.ByRule[string(processobs.FindingPrivilegedExec)] != 1 || resp.ByRule[string(processobs.FindingExecutableFromTmp)] != 1 {
		t.Errorf("by_rule = %+v, want one each", resp.ByRule)
	}
	if resp.BySeverity[processobs.SeverityHigh] != 1 || resp.BySeverity[processobs.SeverityWarn] != 1 {
		t.Errorf("by_severity = %+v, want high:1 warn:1", resp.BySeverity)
	}

	// The same findings appear as side-effect flags on the session drawer.
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/api/session/sA/processes", nil)
	s.Handler().ServeHTTP(rr2, req2)
	var sresp SessionProcessResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &sresp); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if len(sresp.Findings) != 2 {
		t.Errorf("session drawer findings = %d, want 2: %+v", len(sresp.Findings), sresp.Findings)
	}
}

// TestHandleSessionProcessesEmpty pins the empty-but-valid shape.
func TestHandleSessionProcessesEmpty(t *testing.T) {
	s, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sA/processes", nil)
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var resp SessionProcessResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 0 || resp.Roots == nil {
		t.Errorf("empty response should have total 0 + non-nil roots: %+v", resp)
	}
}
