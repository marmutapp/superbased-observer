package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// (*Store) must satisfy the processobs.Sink seam.
var (
	_ processobs.Sink      = (*Store)(nil)
	_ processobs.EventSink = (*Store)(nil)
)

// mustProjectAndSession creates the project + session rows the FK-enforced
// process_runs.session_id / project_id columns require for an attributed
// run, returning the session id and project id.
func mustProjectAndSession(t *testing.T, s *Store) (sessionID string, projectID int64) {
	t.Helper()
	ctx := context.Background()
	id, err := s.UpsertProject(ctx, "/proj", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sessionID = "sess-proc-1"
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, ?, ?)`,
		sessionID, id, "claude-code", timestamp(time.Now().UTC())); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return sessionID, id
}

func TestActiveSessionRoots(t *testing.T) {
	t.Parallel()
	s, db := newTestStore(t)
	ctx := context.Background()

	livePID, err := s.UpsertProject(ctx, "/home/dev/live-proj", "")
	if err != nil {
		t.Fatalf("UpsertProject live: %v", err)
	}
	stalePID, err := s.UpsertProject(ctx, "/home/dev/stale-proj", "")
	if err != nil {
		t.Fatalf("UpsertProject stale: %v", err)
	}
	now := time.Now().UTC()

	// A fresh session (started just now) → its root is active.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('live', ?, 'codex', ?)`,
		livePID, timestamp(now)); err != nil {
		t.Fatalf("seed live session: %v", err)
	}
	// A stale session (started 5h ago, no recent activity) → excluded.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('stale', ?, 'codex', ?)`,
		stalePID, timestamp(now.Add(-5*time.Hour))); err != nil {
		t.Fatalf("seed stale session: %v", err)
	}

	roots, err := s.ActiveSessionRoots(ctx, 60)
	if err != nil {
		t.Fatalf("ActiveSessionRoots: %v", err)
	}
	if len(roots) != 1 || roots[0] != "/home/dev/live-proj" {
		t.Fatalf("roots = %v, want [/home/dev/live-proj]", roots)
	}

	// A recent action on the stale session reactivates its root.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
		 VALUES ('stale', ?, ?, 'run_command', 'codex', 'f', 'a1')`,
		stalePID, timestamp(now)); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	roots2, err := s.ActiveSessionRoots(ctx, 60)
	if err != nil {
		t.Fatalf("ActiveSessionRoots 2: %v", err)
	}
	if len(roots2) != 2 {
		t.Fatalf("roots2 = %v, want both project roots", roots2)
	}
}

// TestActiveSessionIDs pins the recent-active session set the daemon's
// background correlation sweep iterates: a fresh session is included, a stale
// one is excluded until a recent action/token row reactivates it (same window
// semantics as ActiveSessionRoots).
func TestActiveSessionIDs(t *testing.T) {
	t.Parallel()
	s, db := newTestStore(t)
	ctx := context.Background()

	pid, err := s.UpsertProject(ctx, "/home/dev/proj", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	now := time.Now().UTC()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('live', ?, 'codex', ?)`,
		pid, timestamp(now)); err != nil {
		t.Fatalf("seed live session: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES ('stale', ?, 'codex', ?)`,
		pid, timestamp(now.Add(-5*time.Hour))); err != nil {
		t.Fatalf("seed stale session: %v", err)
	}

	ids, err := s.ActiveSessionIDs(ctx, 60)
	if err != nil {
		t.Fatalf("ActiveSessionIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "live" {
		t.Fatalf("ids = %v, want [live]", ids)
	}

	// A recent action on the stale session reactivates it.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO actions (session_id, project_id, timestamp, action_type, tool, source_file, source_event_id)
		 VALUES ('stale', ?, ?, 'run_command', 'codex', 'f', 'a1')`,
		pid, timestamp(now)); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	ids2, err := s.ActiveSessionIDs(ctx, 60)
	if err != nil {
		t.Fatalf("ActiveSessionIDs 2: %v", err)
	}
	if len(ids2) != 2 {
		t.Fatalf("ids2 = %v, want both sessions", ids2)
	}
}

func TestPersistProcessNetworkEventWithBody(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sessionID, projectID := mustProjectAndSession(t, s)
	started := t0Proc()
	run := execRun("proc-network", sessionID, projectID, 4242, started)
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{run}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}
	rows, err := s.ProcessRunsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ProcessRunsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("process rows = %d, want 1", len(rows))
	}
	if _, err := s.PersistProcessEvents(ctx, []processobs.ProcessEvent{{
		ProcessRunID: rows[0].ID,
		ProcessKey:   run.ProcessKey,
		Timestamp:    started.Add(time.Second),
		Type:         processobs.EventNetworkConnect,
		Attribution: processobs.Attribution{
			SessionID:  sessionID,
			Source:     processobs.AttrBridge,
			Confidence: processobs.ConfHigh,
		},
		TargetKind: "url",
		Target:     "https://api.example.test/v1/messages",
		Details:    map[string]any{"status_code": 200, "provider": "anthropic"},
		NetworkBody: &processobs.NetworkBodyCapture{
			CaptureSource:         "proxy",
			RequestID:             "req_test",
			Method:                "POST",
			URL:                   "https://api.example.test/v1/messages",
			Host:                  "api.example.test",
			StatusCode:            200,
			DurationMs:            123,
			RequestBody:           `{"model":"claude","messages":[]}`,
			RequestBodySHA256:     "reqhash",
			RequestBodyBytes:      32,
			ResponseBody:          `{"id":"msg_1"}`,
			ResponseBodySHA256:    "resphash",
			ResponseBodyBytes:     14,
			ResponseContentType:   "application/json",
			ResponseBodyTruncated: true,
		},
	}}); err != nil {
		t.Fatalf("PersistProcessEvents: %v", err)
	}

	events, err := s.NetworkEventsForSession(ctx, sessionID, 10)
	if err != nil {
		t.Fatalf("NetworkEventsForSession: %v", err)
	}
	if len(events) != 1 || !events[0].HasBody || events[0].Target == "" {
		t.Fatalf("events = %+v, want one body-backed event", events)
	}
	if events[0].Tool != "claude-code" || events[0].ProjectID != projectID {
		t.Fatalf("events[0] attribution = tool %q project %d, want session-enriched metadata", events[0].Tool, events[0].ProjectID)
	}
	detail, err := s.NetworkEventDetail(ctx, events[0].ID)
	if err != nil {
		t.Fatalf("NetworkEventDetail: %v", err)
	}
	if detail.Body == nil || detail.Body.RequestBody == "" || !detail.Body.ResponseBodyTruncated {
		t.Fatalf("detail body = %+v, want captured request and truncated response flag", detail.Body)
	}
}

// TestNetworkSummaryForSession pins the summary aggregation + the honest
// EVENT-level proxied-vs-OS discriminator: an event is proxied iff its own
// details JSON carries capture_source='proxy' — whether or not it has a body
// row (a bodyless proxied call still counts, contributing 0 bytes). Bytes come
// only from a proxied event's body row; OS-observed events (process_backend
// details) and any non-proxy body row count as os_connections and contribute
// NO bytes.
func TestNetworkSummaryForSession(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sessionID, _ := mustProjectAndSession(t, s)
	started := t0Proc()

	mkProxy := func(key string, reqBytes, respBytes int) processobs.ProcessEvent {
		return processobs.ProcessEvent{
			ProcessKey: key, Timestamp: started.Add(time.Second),
			Type:        processobs.EventNetworkConnect,
			Attribution: processobs.Attribution{SessionID: sessionID, Source: processobs.AttrHeuristic, Confidence: processobs.ConfMedium},
			TargetKind:  "url", Target: "https://api.example.test/v1/messages",
			Details: map[string]any{"capture_source": "proxy"},
			NetworkBody: &processobs.NetworkBodyCapture{
				CaptureSource: "proxy", Method: "POST",
				RequestBodyBytes: reqBytes, ResponseBodyBytes: respBytes,
			},
		}
	}
	// An OS-observed event: capture_source='process_backend', NO body row.
	osEvent := processobs.ProcessEvent{
		ProcessKey: "os-1", Timestamp: started.Add(2 * time.Second),
		Type:        processobs.EventNetworkConnect,
		Attribution: processobs.Attribution{SessionID: sessionID, Source: processobs.AttrHeuristic, Confidence: processobs.ConfMedium},
		TargetKind:  "network_endpoint", Target: "1.2.3.4:443",
		Details: map[string]any{"capture_source": "process_backend"},
	}
	// A body row with a NON-proxy capture_source proves discrimination keys on
	// the EVENT detail (capture_source='proxy'), not mere body presence: this
	// event carries no proxy detail, so its 999 request bytes must NOT be summed
	// and it counts as an OS connection.
	nonProxyBody := processobs.ProcessEvent{
		ProcessKey: "os-2", Timestamp: started.Add(3 * time.Second),
		Type:        processobs.EventNetworkConnect,
		Attribution: processobs.Attribution{SessionID: sessionID, Source: processobs.AttrHeuristic, Confidence: processobs.ConfMedium},
		TargetKind:  "url", Target: "https://other.test/x",
		Details:     map[string]any{"capture_source": "process_backend"},
		NetworkBody: &processobs.NetworkBodyCapture{CaptureSource: "process_backend", RequestBodyBytes: 999, ResponseBodyBytes: 999},
	}
	// A PROXIED event whose body capture was OFF: its details carry
	// capture_source='proxy' and a body_unavailable_reason, but it has NO
	// process_network_bodies row. The event-level discriminator must count it as
	// a proxied call with ZERO byte contribution (the body-row discriminator
	// used to misclassify this as an OS connection — the undercount this fix
	// closes).
	proxyNoBody := processobs.ProcessEvent{
		ProcessKey: "px-3", Timestamp: started.Add(4 * time.Second),
		Type:        processobs.EventNetworkConnect,
		Attribution: processobs.Attribution{SessionID: sessionID, Source: processobs.AttrHeuristic, Confidence: processobs.ConfMedium},
		TargetKind:  "url", Target: "https://api.example.test/v1/messages",
		Details: map[string]any{"capture_source": "proxy", "body_unavailable_reason": "body_capture_disabled"},
	}

	if _, err := s.PersistProcessEvents(ctx, []processobs.ProcessEvent{
		mkProxy("px-1", 100, 200),
		mkProxy("px-2", 50, 300),
		osEvent,
		nonProxyBody,
		proxyNoBody,
	}); err != nil {
		t.Fatalf("PersistProcessEvents: %v", err)
	}

	sum, err := s.NetworkSummaryForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("NetworkSummaryForSession: %v", err)
	}
	if sum.ProxiedCalls != 3 {
		t.Fatalf("proxied_calls = %d, want 3 (two proxy bodies + one bodyless proxy event)", sum.ProxiedCalls)
	}
	if sum.RequestBytes != 150 {
		t.Fatalf("request_bytes = %d, want 150 (only proxy bodies summed; bodyless proxy adds 0)", sum.RequestBytes)
	}
	if sum.ResponseBytes != 500 {
		t.Fatalf("response_bytes = %d, want 500 (only proxy bodies summed; bodyless proxy adds 0)", sum.ResponseBytes)
	}
	if sum.OSConnections != 2 {
		t.Fatalf("os_connections = %d, want 2 (OS event + non-proxy body row)", sum.OSConnections)
	}

	// A session with no network events returns an all-zero summary, not an error.
	zero, err := s.NetworkSummaryForSession(ctx, "sess-with-no-events")
	if err != nil {
		t.Fatalf("NetworkSummaryForSession(empty): %v", err)
	}
	if zero != (NetworkSummary{}) {
		t.Fatalf("empty-session summary = %+v, want all-zero", zero)
	}
}

func t0Proc() time.Time { return time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC) }

func execRun(key, sess string, projectID int64, pid int, started time.Time) processobs.ProcessRun {
	return processobs.ProcessRun{
		ProcessKey:     key,
		BootID:         "boot-1",
		PID:            pid,
		PPID:           1,
		StartTimeTicks: int64(pid) * 1000,
		Attribution: processobs.Attribution{
			SessionID:  sess,
			Tool:       "claude-code",
			ProjectID:  projectID,
			Source:     processobs.AttrBridge,
			Confidence: processobs.ConfHigh,
		},
		ExePath:     "/bin/bash",
		ExeBasename: "bash",
		CWD:         "/proj",
		ArgvPreview: "bash -c npm test",
		ArgvHash:    "sha256:deadbeef",
		ArgvArgc:    3,
		UID:         1000,
		GID:         1000,
		EnvPosture:  map[string]string{"ANTHROPIC_BASE_URL_present": "true"},
		StartedAt:   started,
		LastSeenAt:  started,
	}
}

// unattributedRun builds a process run as the bridge stores Windows captures:
// no session (AttrNone/ConfNone), Windows-shaped exe/cwd, a stable process_key.
func unattributedRun(key, parent, basename, cwd string, pid int, started time.Time) processobs.ProcessRun {
	return processobs.ProcessRun{
		ProcessKey:       key,
		BootID:           "win-boot",
		PID:              pid,
		PPID:             1,
		StartTimeTicks:   int64(pid) * 1000,
		ParentProcessKey: parent,
		Attribution:      processobs.Attribution{Source: processobs.AttrNone, Confidence: processobs.ConfNone},
		ExePath:          `C:\bin\` + basename,
		ExeBasename:      basename,
		CWD:              cwd,
		StartedAt:        started,
		LastSeenAt:       started,
	}
}

// TestCorrelateCrossOS pins the §5.5 store seam: an UNATTRIBUTED Windows tree
// (claude.exe root in the project root + children) is joined to a claude-code
// session by cwd/tool/time and its whole subtree attributed; a process outside
// the tree stays unattributed; a second pass is a no-op.
func TestCorrelateCrossOS(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	projID, err := s.UpsertProject(ctx, `C:\proj`, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sessStart := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, ?, ?)`,
		"sx", projID, "claude-code", timestamp(sessStart)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// claude.exe root (cwd == project root) -> node child (different cwd) ->
	// git grandchild; plus unrelated noise that must not be attributed.
	root := unattributedRun("w_root", "", "claude.exe", `C:\proj`, 1001, sessStart.Add(2*time.Second))
	child := unattributedRun("w_child", "w_root", "node.exe", `C:\proj\pkg`, 1002, sessStart.Add(5*time.Second))
	gc := unattributedRun("w_gc", "w_child", "git.exe", `C:\proj\pkg`, 1003, sessStart.Add(7*time.Second))
	noise := unattributedRun("w_noise", "", "explorer.exe", `C:\Windows`, 1004, sessStart)
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{root, child, gc, noise}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	n, err := s.CorrelateCrossOS(ctx, "sx")
	if err != nil {
		t.Fatalf("CorrelateCrossOS: %v", err)
	}
	if n != 3 {
		t.Fatalf("attributed %d rows, want 3 (root + child + grandchild)", n)
	}

	runs, err := s.ProcessRunsForSession(ctx, "sx")
	if err != nil {
		t.Fatalf("ProcessRunsForSession: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("session has %d runs, want 3 (noise stays unattributed)", len(runs))
	}
	for _, r := range runs {
		if r.AttributionSource != string(processobs.AttrCrossOSCorrelation) || r.AttributionConfidence != string(processobs.ConfMedium) {
			t.Errorf("%s attribution = %s/%s, want cross_os_correlation/medium", r.ProcessKey, r.AttributionSource, r.AttributionConfidence)
		}
		if r.Tool != "claude-code" {
			t.Errorf("%s tool = %q, want claude-code", r.ProcessKey, r.Tool)
		}
	}

	if n2, err := s.CorrelateCrossOS(ctx, "sx"); err != nil || n2 != 0 {
		t.Errorf("second pass = %d, %v; want 0 (idempotent)", n2, err)
	}
}

// TestCorrelateCrossOSLateDescendant pins the windowed candidate load (B2): the
// anchor is found via the start-time window, but a descendant spawned long
// AFTER the window must still be attributed — the recursive CTE walks the
// subtree down via parent_process_key regardless of the child's own start time
// (§9.2.2). Far-future noise with no AI ancestor stays unattributed and is never
// even loaded.
func TestCorrelateCrossOSLateDescendant(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	projID, err := s.UpsertProject(ctx, `C:\proj`, "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	sessStart := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, ?, ?)`,
		"sl", projID, "claude-code", timestamp(sessStart)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Anchor in-window; a child spawned 30 min later (far OUTSIDE the ±window);
	// plus far-future noise that must NOT be attributed.
	root := unattributedRun("l_root", "", "claude.exe", `C:\proj`, 2001, sessStart.Add(3*time.Second))
	late := unattributedRun("l_late", "l_root", "node.exe", `C:\proj\srv`, 2002, sessStart.Add(30*time.Minute))
	noise := unattributedRun("l_noise", "", "explorer.exe", `C:\Windows`, 2003, sessStart.Add(45*time.Minute))
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{root, late, noise}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	n, err := s.CorrelateCrossOS(ctx, "sl")
	if err != nil {
		t.Fatalf("CorrelateCrossOS: %v", err)
	}
	if n != 2 {
		t.Fatalf("attributed %d rows, want 2 (anchor + late descendant via subtree walk)", n)
	}
	runs, err := s.ProcessRunsForSession(ctx, "sl")
	if err != nil {
		t.Fatalf("ProcessRunsForSession: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("session has %d runs, want 2 (far-future noise stays unattributed)", len(runs))
	}
}

func TestPersistRunsRoundTrip(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, pid0 := mustProjectAndSession(t, s)

	run := execRun("key-aaa", sess, pid0, 200, t0Proc())
	n, err := s.PersistRuns(ctx, []processobs.ProcessRun{run})
	if err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("persisted %d, want 1", n)
	}

	got, err := s.ProcessRunsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("ProcessRunsForSession: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d runs, want 1", len(got))
	}
	r := got[0]
	if r.ProcessKey != "key-aaa" || r.PID != 200 || r.Tool != "claude-code" {
		t.Errorf("roundtrip mismatch: %+v", r)
	}
	if r.AttributionSource != string(processobs.AttrBridge) || r.AttributionConfidence != string(processobs.ConfHigh) {
		t.Errorf("attribution not persisted: %s/%s", r.AttributionSource, r.AttributionConfidence)
	}
	if r.ArgvPreview != "bash -c npm test" || r.ArgvArgc != 3 {
		t.Errorf("argv not persisted: %q argc=%d", r.ArgvPreview, r.ArgvArgc)
	}
	if r.EnvPostureJSON == "" {
		t.Error("env posture not persisted")
	}
	if r.Exited {
		t.Error("a fresh exec run must not be marked exited")
	}
}

// TestPersistRunsPersistsPosture pins the P4 security/isolation columns:
// they survive the write and read back through ProcessRunsForSession.
func TestPersistRunsPersistsPosture(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)

	run := execRun("k_posture", sess, projID, 200, t0Proc())
	run.SeccompMode = "filter"
	run.CapabilitiesEff = "00000000a80425fb"
	run.AppArmorLabel = "unconfined"
	run.ContainerID = "abc123def456"
	run.CgroupHash = "sha256:deadbeef"
	run.PIDNamespace = "4026531836"
	run.NetNamespace = "4026531992"
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{run}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	got, err := s.ProcessRunsForSession(ctx, sess)
	if err != nil || len(got) != 1 {
		t.Fatalf("read = %d rows, %v", len(got), err)
	}
	r := got[0]
	if r.SeccompMode != "filter" || r.CapabilitiesEff != "00000000a80425fb" {
		t.Errorf("seccomp/caps not persisted: %+v", r)
	}
	if r.AppArmorLabel != "unconfined" || r.ContainerID != "abc123def456" || r.CgroupHash != "sha256:deadbeef" {
		t.Errorf("apparmor/container/cgroup not persisted: %+v", r)
	}
	if r.PIDNamespace != "4026531836" || r.NetNamespace != "4026531992" {
		t.Errorf("namespaces not persisted: %+v", r)
	}
}

// TestPersistRunsUpsertExecThenExit pins the create-at-exec / update-at-exit
// contract: the same process_key persisted twice yields ONE row that gains
// the exit fields on the second write (spec §8).
func TestPersistRunsUpsertExecThenExit(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, pid0 := mustProjectAndSession(t, s)

	exec := execRun("key-bbb", sess, pid0, 300, t0Proc())
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{exec}); err != nil {
		t.Fatalf("persist exec: %v", err)
	}

	// Exit snapshot: same key, now finalized.
	exit := exec
	exit.Exited = true
	exit.ExitedAt = t0Proc().Add(5 * time.Second)
	exit.LastSeenAt = exit.ExitedAt
	exit.ExitCode = 7
	exit.DurationMs = 5000
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{exit}); err != nil {
		t.Fatalf("persist exit: %v", err)
	}

	got, err := s.ProcessRunsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("upsert produced %d rows, want 1", len(got))
	}
	r := got[0]
	if !r.Exited || r.ExitCode != 7 || r.DurationMs != 5000 {
		t.Errorf("exit fields not applied on upsert: exited=%v code=%d dur=%d", r.Exited, r.ExitCode, r.DurationMs)
	}

	cnt, err := s.CountProcessRuns(ctx)
	if err != nil || cnt != 1 {
		t.Fatalf("CountProcessRuns = %d, %v; want 1", cnt, err)
	}
}

// TestPersistRunsUnattributedLandsNullFKs verifies an unattributed run
// persists with NULL session/project (FK-safe) — the capture_unattributed
// path must not violate the foreign keys.
func TestPersistRunsUnattributedLandsNullFKs(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	run := processobs.ProcessRun{
		ProcessKey:     "key-unattr",
		BootID:         "boot-1",
		PID:            999,
		StartTimeTicks: 999000,
		Attribution:    processobs.Attribution{Source: processobs.AttrNone, Confidence: processobs.ConfNone},
		ExeBasename:    "mystery",
		StartedAt:      t0Proc(),
		LastSeenAt:     t0Proc(),
	}
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{run}); err != nil {
		t.Fatalf("PersistRuns (unattributed): %v", err)
	}
	cnt, err := s.CountProcessRuns(ctx)
	if err != nil || cnt != 1 {
		t.Fatalf("CountProcessRuns = %d, %v; want 1", cnt, err)
	}
	// It must NOT show up under any session query (session_id is NULL).
	got, err := s.ProcessRunsForSession(ctx, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("unattributed run matched a session query: %d", len(got))
	}
}

// TestCorrelateProcessActions pins the §9.2.4 store seam: a run_command
// action links to the process subtree it spawned (anchor by exe match +
// propagation), filling action_id/turn_index — and a second pass is a no-op.
func TestCorrelateProcessActions(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)

	base := t0Proc()
	// A run_command action "npm test" at turn 4.
	if _, err := s.Ingest(ctx, []models.ToolEvent{{
		SourceFile: "cc.jsonl", SourceEventID: "a1", SessionID: sess,
		ProjectRoot: "/proj", Timestamp: base, Tool: models.ToolClaudeCode,
		ActionType: models.ActionRunCommand, Target: "npm test", TurnIndex: 4, Success: true,
	}}, nil, IngestOptions{}); err != nil {
		t.Fatalf("Ingest action: %v", err)
	}

	// Process tree spawned ~1s later: sh -c → npm → node.
	sh := execRun("k_sh", sess, projID, 100, base.Add(time.Second))
	sh.ExeBasename, sh.ArgvPreview = "bash", "bash -c npm test"
	sh.ParentProcessKey = ""
	npm := execRun("k_npm", sess, projID, 200, base.Add(time.Second))
	npm.ExeBasename, npm.ArgvPreview, npm.ParentProcessKey = "npm", "npm test", "k_sh"
	node := execRun("k_node", sess, projID, 300, base.Add(2*time.Second))
	node.ExeBasename, node.ArgvPreview, node.ParentProcessKey = "node", "node x.js", "k_npm"
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{sh, npm, node}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	n, err := s.CorrelateProcessActions(ctx, sess)
	if err != nil {
		t.Fatalf("CorrelateProcessActions: %v", err)
	}
	if n != 3 {
		t.Fatalf("linked %d runs, want 3 (whole subtree)", n)
	}

	runs, err := s.ProcessRunsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(runs))
	}
	for _, r := range runs {
		if r.ActionID == nil {
			t.Errorf("%s not linked to an action", r.ProcessKey)
			continue
		}
		if r.TurnIndex == nil || *r.TurnIndex != 4 {
			t.Errorf("%s turn_index = %v, want 4", r.ProcessKey, r.TurnIndex)
		}
	}

	// Idempotent: a second pass links nothing new.
	if n2, err := s.CorrelateProcessActions(ctx, sess); err != nil || n2 != 0 {
		t.Errorf("second correlation pass = %d, %v; want 0 (idempotent)", n2, err)
	}
}

// TestLongRunningChildRuns pins the §14 detection: a still-alive process
// whose session ended past the threshold is flagged; an exited sibling and
// a process under a not-yet-ended session are not.
func TestLongRunningChildRuns(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	projID, err := s.UpsertProject(ctx, "/proj", "")
	if err != nil {
		t.Fatalf("UpsertProject: %v", err)
	}
	endedAt := time.Now().UTC().Add(-2 * time.Hour) // session ended 2h ago
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, started_at, ended_at) VALUES (?, ?, ?, ?, ?)`,
		"sess-ended", projID, "claude-code", timestamp(endedAt.Add(-time.Hour)), timestamp(endedAt)); err != nil {
		t.Fatalf("seed ended session: %v", err)
	}

	alive := execRun("k_alive", "sess-ended", projID, 100, endedAt.Add(-30*time.Minute)) // exited=false
	dead := execRun("k_dead", "sess-ended", projID, 101, endedAt.Add(-30*time.Minute))
	dead.Exited, dead.ExitedAt = true, endedAt
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{alive, dead}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	got, err := s.LongRunningChildRuns(ctx, time.Hour)
	if err != nil {
		t.Fatalf("LongRunningChildRuns: %v", err)
	}
	if len(got) != 1 || got[0].PID != 100 {
		t.Fatalf("got %d rows %+v, want only the alive pid 100", len(got), got)
	}

	// A huge threshold (session ended only 2h ago) excludes it.
	if recent, _ := s.LongRunningChildRuns(ctx, 72*time.Hour); len(recent) != 0 {
		t.Errorf("session ended within threshold should not flag: %d", len(recent))
	}
}

// TestProcessStatsAndActionCommands pins the two CLI-facing read helpers.
func TestProcessStatsAndActionCommands(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)

	attributed := execRun("k_a", sess, projID, 100, t0Proc())
	unattributed := processobs.ProcessRun{
		ProcessKey: "k_u", BootID: "b", PID: 999, StartTimeTicks: 999,
		Attribution: processobs.Attribution{Source: processobs.AttrNone, Confidence: processobs.ConfNone},
		ExeBasename: "mystery", StartedAt: t0Proc(), LastSeenAt: t0Proc(),
	}
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{attributed, unattributed}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	stats, err := s.ProcessStats(ctx)
	if err != nil {
		t.Fatalf("ProcessStats: %v", err)
	}
	if stats.Total != 2 || stats.Attributed != 1 || stats.Unattributed != 1 {
		t.Errorf("stats = %+v, want total2 attr1 unattr1", stats)
	}
	if stats.ByTool["claude-code"] != 1 {
		t.Errorf("by_tool = %+v, want claude-code:1", stats.ByTool)
	}

	if _, err := s.Ingest(ctx, []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e1", SessionID: sess, ProjectRoot: "/proj",
		Timestamp: t0Proc(), Tool: models.ToolClaudeCode,
		ActionType: models.ActionRunCommand, Target: "go test ./...", Success: true,
	}}, nil, IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	cmds, err := s.ActionCommandsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("ActionCommandsForSession: %v", err)
	}
	found := false
	for _, c := range cmds {
		if c == "go test ./..." {
			found = true
		}
	}
	if !found {
		t.Errorf("action commands = %v, want the run_command target", cmds)
	}
}

// TestActionMessageIDsForSession pins the §9.2.4 message link: a run_command
// action's message_id is returned keyed by action id (the join that lets a
// process_run name the assistant message that spawned it).
func TestActionMessageIDsForSession(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, _ := mustProjectAndSession(t, s)

	if _, err := s.Ingest(ctx, []models.ToolEvent{{
		SourceFile: "f", SourceEventID: "e-msg", SessionID: sess, ProjectRoot: "/proj",
		Timestamp: t0Proc(), Tool: models.ToolClaudeCode,
		ActionType: models.ActionRunCommand, Target: "go build ./...", Success: true,
		MessageID: "msg_01TestAbc",
	}}, nil, IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	msgIDs, err := s.ActionMessageIDsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("ActionMessageIDsForSession: %v", err)
	}
	found := false
	for _, m := range msgIDs {
		if m == "msg_01TestAbc" {
			found = true
		}
	}
	if !found {
		t.Errorf("message ids = %v, want msg_01TestAbc present", msgIDs)
	}
}

// TestPruneProcessRowsAndNoop pins the retention sweep + idempotency.
func TestPruneProcessRowsAndNoop(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, pid0 := mustProjectAndSession(t, s)

	old := execRun("key-old", sess, pid0, 100, t0Proc().Add(-100*24*time.Hour)) // 100 days old
	fresh := execRun("key-fresh", sess, pid0, 101, time.Now().UTC())
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{old, fresh}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// retention_days <= 0 is a no-op.
	if n, err := s.PruneProcessRows(ctx, 0); err != nil || n != 0 {
		t.Fatalf("disabled prune = %d, %v; want 0", n, err)
	}

	// 30-day horizon removes the 100-day-old row, keeps the fresh one.
	n, err := s.PruneProcessRows(ctx, 30)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	cnt, _ := s.CountProcessRuns(ctx)
	if cnt != 1 {
		t.Errorf("remaining %d, want 1 (the fresh run)", cnt)
	}

	// Second run within the same horizon is a no-op.
	if n, err := s.PruneProcessRows(ctx, 30); err != nil || n != 0 {
		t.Errorf("second prune = %d, %v; want 0 (idempotent)", n, err)
	}
}

// findingRun builds an attributed run with explicit exe + uid/euid (execRun
// leaves EUID at 0, which would itself trip privileged_exec).
func findingRun(key, sess string, projID int64, pid int, exe, base string, uid, euid int, started time.Time) processobs.ProcessRun {
	return processobs.ProcessRun{
		ProcessKey: key, BootID: "b", PID: pid, StartTimeTicks: int64(pid) * 1000,
		Attribution: processobs.Attribution{
			SessionID: sess, Tool: "claude-code", ProjectID: projID,
			Source: processobs.AttrBridge, Confidence: processobs.ConfHigh,
		},
		ExePath: exe, ExeBasename: base, UID: uid, EUID: euid,
		StartedAt: started, LastSeenAt: started,
	}
}

func ruleIDs(findings []ProcessFindingRow) map[string]bool {
	m := make(map[string]bool, len(findings))
	for _, f := range findings {
		m[f.RuleID] = true
	}
	return m
}

// TestProcessFindingsForSession pins the §14 derive-on-read findings: a
// setuid-root run trips privileged_exec, a /tmp executable trips
// executable_from_tmp, a plain non-root run trips nothing.
func TestProcessFindingsForSession(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)

	runs := []processobs.ProcessRun{
		findingRun("f_norm", sess, projID, 100, "/usr/bin/node", "node", 1000, 1000, t0Proc()),
		findingRun("f_priv", sess, projID, 101, "/usr/bin/sudo", "sudo", 1000, 0, t0Proc()),
		findingRun("f_tmp", sess, projID, 102, "/tmp/build/payload", "payload", 1000, 1000, t0Proc()),
	}
	if _, err := s.PersistRuns(ctx, runs); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	got, err := s.ProcessFindingsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("ProcessFindingsForSession: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d findings %+v, want 2", len(got), got)
	}
	ids := ruleIDs(got)
	if !ids[string(processobs.FindingPrivilegedExec)] || !ids[string(processobs.FindingExecutableFromTmp)] {
		t.Errorf("findings = %+v, want privileged_exec + executable_from_tmp", got)
	}
	for _, f := range got {
		if f.ProcessKey == "f_norm" {
			t.Errorf("plain run f_norm should not produce a finding: %+v", f)
		}
		if f.Tool != "claude-code" || f.Severity == "" || f.Timestamp.IsZero() {
			t.Errorf("finding missing display meta: %+v", f)
		}
	}
}

// TestRecentProcessFindings pins the cross-session window: a fresh setuid run
// is in the rollup; one started before the window is excluded.
func TestRecentProcessFindings(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)

	now := time.Now().UTC()
	fresh := findingRun("r_fresh", sess, projID, 200, "/usr/bin/sudo", "sudo", 1000, 0, now.Add(-5*time.Minute))
	old := findingRun("r_old", sess, projID, 201, "/usr/bin/sudo", "sudo", 1000, 0, now.Add(-100*24*time.Hour))
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{fresh, old}); err != nil {
		t.Fatalf("PersistRuns: %v", err)
	}

	got, err := s.RecentProcessFindings(ctx, time.Hour)
	if err != nil {
		t.Fatalf("RecentProcessFindings: %v", err)
	}
	if len(got) != 1 || got[0].ProcessKey != "r_fresh" {
		t.Fatalf("got %+v, want only the fresh setuid run", got)
	}
}

// TestSessionSeedByID pins the §5.5 P-B6 env-token store seam: an existing
// session id resolves to a seed carrying its tool + project at env_token/high
// (direct equality), and an unknown or empty id is a clean miss (no error).
func TestSessionSeedByID(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)

	seed, ok, err := s.SessionSeedByID(ctx, sess)
	if err != nil {
		t.Fatalf("SessionSeedByID: %v", err)
	}
	if !ok {
		t.Fatal("expected a hit for an existing session id")
	}
	if seed.SessionID != sess || seed.Tool != "claude-code" || seed.ProjectID != projID {
		t.Errorf("seed = %+v (want sess=%s tool=claude-code proj=%d)", seed, sess, projID)
	}
	if seed.Source != processobs.AttrEnvToken || seed.Confidence != processobs.ConfHigh {
		t.Errorf("seed source/conf = %s/%s, want env_token/high", seed.Source, seed.Confidence)
	}

	if _, ok, err := s.SessionSeedByID(ctx, "no-such-session"); err != nil || ok {
		t.Errorf("unknown id: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, ok, err := s.SessionSeedByID(ctx, ""); err != nil || ok {
		t.Errorf("empty id: ok=%v err=%v, want false/nil", ok, err)
	}
}

// TestDeriveProcessRunsFromActions pins the action-derived command rows: a fast
// command the OS-capture backend missed still gets a process_runs row carrying
// its deterministic message/action link, and that row is reaped once a real OS
// process is later captured and anchored to the same action.
func TestDeriveProcessRunsFromActions(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()
	sess, projID := mustProjectAndSession(t, s)
	base := t0Proc()

	// Two run_command actions the poll backend never saw (no process_runs).
	for _, a := range []struct {
		eid, cmd string
		turn     int
		ok       bool
	}{
		{"a_sed", "sed -n '1,5p' README.md", 1, true},
		{"a_rg", "rg --files", 1, false},
	} {
		if _, err := s.Ingest(ctx, []models.ToolEvent{{
			SourceFile: "cdx.jsonl", SourceEventID: a.eid, SessionID: sess,
			ProjectRoot: "/proj", Timestamp: base, Tool: models.ToolClaudeCode,
			ActionType: models.ActionRunCommand, Target: a.cmd, TurnIndex: a.turn, Success: a.ok,
		}}, nil, IngestOptions{}); err != nil {
			t.Fatalf("Ingest %s: %v", a.eid, err)
		}
	}

	n, err := s.DeriveProcessRunsFromActions(ctx, sess)
	if err != nil {
		t.Fatalf("DeriveProcessRunsFromActions: %v", err)
	}
	if n != 2 {
		t.Fatalf("derived %d rows, want 2", n)
	}

	runs, err := s.ProcessRunsForSession(ctx, sess)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d process rows, want 2 (both derived)", len(runs))
	}
	for _, r := range runs {
		if r.AttributionSource != string(processobs.AttrActionCorrelation) {
			t.Errorf("%s source = %q, want action_correlation", r.ProcessKey, r.AttributionSource)
		}
		if r.PID != 0 {
			t.Errorf("%s pid = %d, want 0 (derived)", r.ProcessKey, r.PID)
		}
		if r.ActionID == nil {
			t.Errorf("%s has no action link (the whole point)", r.ProcessKey)
		}
	}

	// Idempotent: re-deriving upserts the same rows, never duplicates.
	if _, err := s.DeriveProcessRunsFromActions(ctx, sess); err != nil {
		t.Fatalf("second derive: %v", err)
	}
	if runs2, _ := s.ProcessRunsForSession(ctx, sess); len(runs2) != 2 {
		t.Fatalf("after re-derive got %d rows, want 2 (idempotent)", len(runs2))
	}

	// Now a real OS process is captured for the "sed" command and anchored to
	// its action. The derived row for that action must be reaped; the "rg"
	// derived row stays.
	var sedActionID int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM actions WHERE session_id = ? AND target LIKE 'sed%'`, sess).Scan(&sedActionID); err != nil {
		t.Fatalf("find sed action: %v", err)
	}
	os := execRun("k_sed_os", sess, projID, 4242, base.Add(500*time.Millisecond))
	os.ExeBasename, os.ArgvPreview = "sed", "sed -n 1,5p README.md"
	if _, err := s.PersistRuns(ctx, []processobs.ProcessRun{os}); err != nil {
		t.Fatalf("PersistRuns OS: %v", err)
	}
	if _, err := s.CorrelateProcessActions(ctx, sess); err != nil {
		t.Fatalf("CorrelateProcessActions: %v", err)
	}
	if _, err := s.DeriveProcessRunsFromActions(ctx, sess); err != nil {
		t.Fatalf("derive after capture: %v", err)
	}

	runs3, _ := s.ProcessRunsForSession(ctx, sess)
	var derived, osRows int
	for _, r := range runs3 {
		if r.AttributionSource == string(processobs.AttrActionCorrelation) {
			derived++
		} else {
			osRows++
		}
	}
	if osRows != 1 {
		t.Errorf("want 1 OS row, got %d", osRows)
	}
	if derived != 1 {
		t.Errorf("want 1 surviving derived row (rg), got %d — superseded sed row not reaped?", derived)
	}
}

// TestPruneProcessRowsDeletesNetworkBodies proves the process retention sweep
// cascades to process_network_bodies (the 389MB body table on the live DB):
// an over-horizon network event's body row is deleted, a fresh one survives.
func TestPruneProcessRowsDeletesNetworkBodies(t *testing.T) {
	t.Parallel()
	s, database := newTestStore(t)
	ctx := context.Background()
	sess, _ := mustProjectAndSession(t, s)

	mkEvent := func(key string, ts time.Time) processobs.ProcessEvent {
		return processobs.ProcessEvent{
			ProcessKey: key, Timestamp: ts, Type: processobs.EventNetworkConnect,
			Attribution: processobs.Attribution{SessionID: sess},
			TargetKind:  "domain", Target: "api.example.com",
			NetworkBody: &processobs.NetworkBodyCapture{
				CaptureSource: "proxy", Method: "POST", URL: "https://api.example.com/v1",
				Host: "api.example.com", StatusCode: 200,
				ResponseBody: "hello", ResponseBodyBytes: 5,
			},
		}
	}
	if _, err := s.PersistProcessEvents(ctx, []processobs.ProcessEvent{
		mkEvent("net-old", time.Now().UTC().Add(-100*24*time.Hour)),
		mkEvent("net-fresh", time.Now().UTC()),
	}); err != nil {
		t.Fatalf("PersistProcessEvents: %v", err)
	}

	countBodies := func() int {
		var n int
		if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM process_network_bodies`).Scan(&n); err != nil {
			t.Fatalf("count bodies: %v", err)
		}
		return n
	}
	if got := countBodies(); got != 2 {
		t.Fatalf("seeded bodies = %d, want 2", got)
	}

	if _, err := s.PruneProcessRows(ctx, 30); err != nil {
		t.Fatalf("PruneProcessRows: %v", err)
	}
	if got := countBodies(); got != 1 {
		t.Errorf("bodies after prune = %d, want 1 (old body deleted, fresh kept)", got)
	}
}

// TestNetworkSummaryForSession_MalformedDetailsJSON proves Fix 5: a single
// malformed (non-NULL, invalid) details_json row must NOT abort the aggregate.
// Before the json_valid guard, json_extract raised "malformed JSON" and failed
// the whole query; now the row yields a NULL capture_source and classifies as
// OS-observed (non-proxy). PersistProcessEvents always marshals valid JSON, so
// the bad row is inserted directly to bypass that path.
func TestNetworkSummaryForSession_MalformedDetailsJSON(t *testing.T) {
	t.Parallel()
	s, database := newTestStore(t)
	ctx := context.Background()
	sessionID, _ := mustProjectAndSession(t, s)
	started := t0Proc()

	// One valid proxy event with a body so the aggregate has a proxied call +
	// byte totals to preserve alongside the malformed row.
	if _, err := s.PersistProcessEvents(ctx, []processobs.ProcessEvent{{
		ProcessKey: "px-1", Timestamp: started.Add(time.Second),
		Type:        processobs.EventNetworkConnect,
		Attribution: processobs.Attribution{SessionID: sessionID, Source: processobs.AttrHeuristic, Confidence: processobs.ConfMedium},
		TargetKind:  "url", Target: "https://api.example.test/v1/messages",
		Details:     map[string]any{"capture_source": "proxy"},
		NetworkBody: &processobs.NetworkBodyCapture{CaptureSource: "proxy", RequestBodyBytes: 100, ResponseBodyBytes: 200},
	}}); err != nil {
		t.Fatalf("PersistProcessEvents: %v", err)
	}

	// A row whose details_json is present but NOT valid JSON.
	if _, err := database.ExecContext(ctx, `
		INSERT INTO process_events (process_key, timestamp, event_type, session_id, target_kind, target, details_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"bad-1", timestamp(started.Add(2*time.Second)), string(processobs.EventNetworkConnect),
		sessionID, "url", "https://bad.test/x", "{not valid json"); err != nil {
		t.Fatalf("insert malformed row: %v", err)
	}

	sum, err := s.NetworkSummaryForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("NetworkSummaryForSession must not error on a malformed row: %v", err)
	}
	if sum.ProxiedCalls != 1 {
		t.Fatalf("proxied_calls = %d, want 1 (malformed row is not proxy)", sum.ProxiedCalls)
	}
	if sum.OSConnections != 1 {
		t.Fatalf("os_connections = %d, want 1 (malformed row classifies as OS-observed)", sum.OSConnections)
	}
	if sum.RequestBytes != 100 || sum.ResponseBytes != 200 {
		t.Fatalf("bytes = %d/%d, want 100/200 (proxy body preserved)", sum.RequestBytes, sum.ResponseBytes)
	}
}
