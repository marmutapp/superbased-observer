package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
)

// fakeExecResult drives backfillExecFn injection for tests so we don't
// have to spawn the real observer binary (which isn't always findable
// in the test process's $PATH and would be flaky to depend on).
type fakeExecResult struct {
	out     []byte
	exit    int
	err     error
	delay   time.Duration
	sawArgs []string
}

func newFakeExec(r *fakeExecResult) backfillExecFn {
	return func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		r.sawArgs = append([]string(nil), args...)
		if r.delay > 0 {
			select {
			case <-time.After(r.delay):
			case <-ctx.Done():
				return -1, ctx.Err()
			}
		}
		// Stream the configured output via the callback so the streaming
		// path is exercised end-to-end. A nil onChunk (during eager
		// validation paths) is tolerated so tests covering the rejected-
		// allowlist case never hit this branch.
		if len(r.out) > 0 && onChunk != nil {
			onChunk(r.out)
		}
		return r.exit, r.err
	}
}

func newServerWithFakeExec(t *testing.T, fake backfillExecFn) *Server {
	t.Helper()
	tdir := t.TempDir()
	database, err := db.Open(context.Background(), db.Options{Path: filepath.Join(tdir, "d.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	server, err := New(Options{DB: database, ConfigPath: filepath.Join(tdir, "config.toml")})
	if err != nil {
		t.Fatal(err)
	}
	server.execBackfill = fake
	return server
}

// TestBackfillRun_AllowlistRejectsBogus guards against arbitrary mode
// strings landing in os/exec — only the explicit allowlist values
// (mirroring /api/backfill/status) are accepted.
func TestBackfillRun_AllowlistRejectsBogus(t *testing.T) {
	var called atomic.Bool
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		called.Store(true)
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	for _, bogus := range []string{"bogus-mode", "", "; rm -rf /", "../../etc/passwd"} {
		body := `{"mode":` + jsonEscape(bogus) + `}`
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("mode=%q: status %d want 400", bogus, rr.Code)
		}
	}
	if called.Load() {
		t.Errorf("execBackfill should NOT be called for any rejected mode")
	}
}

// TestBackfillRun_HappyPathDoneStatus verifies the flow: POST /run →
// returns running + a job id; goroutine completes; GET /jobs/<id>
// reflects done + exit_code 0 + captured output.
func TestBackfillRun_HappyPathDoneStatus(t *testing.T) {
	fake := newFakeExec(&fakeExecResult{
		out:  []byte("backfill --message-id: 42 rows updated\n"),
		exit: 0,
	})
	server := newServerWithFakeExec(t, fake)

	// Kick the run.
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"message-id"}`)))
	if rr.Code != 200 {
		t.Fatalf("POST status: %d body=%s", rr.Code, rr.Body.String())
	}
	var runResp struct {
		JobID  string `json:"job_id"`
		Mode   string `json:"mode"`
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&runResp); err != nil {
		t.Fatal(err)
	}
	if runResp.JobID == "" || runResp.Mode != "message-id" || runResp.Status != "running" {
		t.Errorf("run response: %+v", runResp)
	}

	// Poll until done — fake returns immediately so a couple of polls
	// is plenty. Cap at ~2 seconds in case the goroutine is delayed.
	var pollResp backfillJob
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+runResp.JobID, nil))
		if rr.Code != 200 {
			t.Fatalf("GET status: %d", rr.Code)
		}
		if err := json.NewDecoder(rr.Body).Decode(&pollResp); err != nil {
			t.Fatal(err)
		}
		if pollResp.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pollResp.Status != "done" {
		t.Errorf("final status: got %q want done · err=%q output=%q",
			pollResp.Status, pollResp.Error, pollResp.Output)
	}
	if pollResp.ExitCode != 0 {
		t.Errorf("exit code: got %d want 0", pollResp.ExitCode)
	}
	if !strings.Contains(pollResp.Output, "42 rows updated") {
		t.Errorf("output not captured: %q", pollResp.Output)
	}
}

// TestBackfillRun_NonZeroExitMarksFailed pins the failure path: a
// child process that exits non-zero leaves the job in status=failed
// with the exit code and error message preserved.
func TestBackfillRun_NonZeroExitMarksFailed(t *testing.T) {
	fake := newFakeExec(&fakeExecResult{
		out:  []byte("error: claude projects dir not found\n"),
		exit: 1,
	})
	server := newServerWithFakeExec(t, fake)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"is-sidechain"}`)))
	var runResp struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&runResp)

	// Spin until terminal status.
	var got backfillJob
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+runResp.JobID, nil))
		_ = json.NewDecoder(rr.Body).Decode(&got)
		if got.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != "failed" {
		t.Errorf("status: got %q want failed", got.Status)
	}
	if got.ExitCode != 1 {
		t.Errorf("exit code: got %d want 1", got.ExitCode)
	}
	if !strings.Contains(got.Error, "exited with code 1") {
		t.Errorf("error message: %q", got.Error)
	}
}

// TestBackfillRun_ConfigPathPropagated verifies the `--config <path>`
// arg is appended when the dashboard was started with a config path.
// Lets the spawned subprocess find the same TOML the dashboard loaded.
//
// Synchronization: the fake exec signals via a channel rather than
// busy-polling sawArgs — under -race, polling deadlines can be too
// tight if the test runner's scheduler doesn't run the goroutine
// promptly.
func TestBackfillRun_ConfigPathPropagated(t *testing.T) {
	type sawArgs struct{ args []string }
	seen := make(chan sawArgs, 1)
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		seen <- sawArgs{args: append([]string(nil), args...)}
		if onChunk != nil {
			onChunk([]byte("ok\n"))
		}
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"all"}`)))
	if rr.Code != 200 {
		t.Fatalf("POST status: %d", rr.Code)
	}

	var got sawArgs
	select {
	case got = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess never invoked")
	}
	if len(got.args) < 4 {
		t.Fatalf("subprocess args: got %v want at least [backfill --all --config <path>]", got.args)
	}
	if got.args[0] != "backfill" {
		t.Errorf("subcommand: got %q want backfill (exec fn takes the full vector since P1.10)", got.args[0])
	}
	if got.args[1] != "--all" {
		t.Errorf("flag arg: got %q want --all", got.args[1])
	}
	hasConfigFlag := false
	for i, a := range got.args {
		if a == "--config" && i+1 < len(got.args) {
			hasConfigFlag = true
		}
	}
	if !hasConfigFlag {
		t.Errorf("--config flag not propagated: %v", got.args)
	}
}

// TestPruneRun_SpawnsPruneSubprocess — POST /api/prune/run (P1.10)
// reuses the job registry with the "prune" subcommand and surfaces the
// job via the shared polling endpoints.
func TestPruneRun_SpawnsPruneSubprocess(t *testing.T) {
	type sawArgs struct{ args []string }
	seen := make(chan sawArgs, 1)
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		seen <- sawArgs{args: append([]string(nil), args...)}
		onChunk([]byte("retention: 0 rows pruned\n"))
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/prune/run", nil))
	if rr.Code != 200 {
		t.Fatalf("POST status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		JobID string `json:"job_id"`
		Mode  string `json:"mode"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Mode != "prune" || resp.JobID == "" {
		t.Fatalf("response: %+v", resp)
	}

	var pruneArgs sawArgs
	select {
	case pruneArgs = <-seen:
	case <-time.After(5 * time.Second):
		t.Fatal("subprocess never invoked")
	}
	if len(pruneArgs.args) == 0 || pruneArgs.args[0] != "prune" {
		t.Errorf("args: got %v want [prune ...]", pruneArgs.args)
	}

	// The job must be pollable through the shared jobs endpoint.
	deadline := time.Now().Add(5 * time.Second)
	for {
		jr := httptest.NewRecorder()
		server.Handler().ServeHTTP(jr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+resp.JobID, nil))
		if jr.Code != 200 {
			t.Fatalf("job poll: %d", jr.Code)
		}
		var job struct {
			Status string `json:"status"`
			Output string `json:"output"`
		}
		if err := json.NewDecoder(jr.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		if job.Status == "done" {
			if !strings.Contains(job.Output, "retention") {
				t.Errorf("output not captured: %q", job.Output)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never finished: %+v", job)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestBackfillRun_StreamsOutputBeforeExit pins the streaming behaviour
// added in the polish round: a subprocess that emits output across
// multiple chunks shows partial output in /api/backfill/jobs/<id>
// before the child exits. The fake exec sends an early chunk, blocks
// on a channel until the test signals it to finish, then sends a
// final chunk. The test polls between the two and asserts the partial
// chunk is visible while the job is still "running".
func TestBackfillRun_StreamsOutputBeforeExit(t *testing.T) {
	release := make(chan struct{})
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		onChunk([]byte("phase 1: 100 rows scanned\n"))
		<-release // simulate long-running work
		onChunk([]byte("phase 2: done\n"))
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"message-id"}`)))
	if rr.Code != 200 {
		t.Fatalf("POST status: %d", rr.Code)
	}
	var runResp struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&runResp)

	// Poll the running job — phase 1 should be visible before we
	// release the fake.
	deadline := time.Now().Add(2 * time.Second)
	var pollResp backfillJob
	sawPartial := false
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+runResp.JobID, nil))
		_ = json.NewDecoder(rr.Body).Decode(&pollResp)
		if pollResp.Status == "running" && strings.Contains(pollResp.Output, "phase 1") {
			sawPartial = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawPartial {
		t.Errorf("expected partial output streamed during running state · final job=%+v", pollResp)
	}

	// Release the fake; assert phase 2 lands.
	close(release)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+runResp.JobID, nil))
		_ = json.NewDecoder(rr.Body).Decode(&pollResp)
		if pollResp.Status == "done" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pollResp.Status != "done" {
		t.Errorf("final status: got %q want done", pollResp.Status)
	}
	if !strings.Contains(pollResp.Output, "phase 1") || !strings.Contains(pollResp.Output, "phase 2") {
		t.Errorf("final output missing chunks: %q", pollResp.Output)
	}
}

// TestBackfillRun_OutputCappedAt1MiB guards memory growth on
// pathologically chatty backfills. The fake streams a 2 MiB chunk;
// the registry truncates at 1 MiB and appends a marker.
func TestBackfillRun_OutputCappedAt1MiB(t *testing.T) {
	big := make([]byte, 2<<20) // 2 MiB
	for i := range big {
		big[i] = 'X'
	}
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		onChunk(big)
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"all"}`)))
	var runResp struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&runResp)

	deadline := time.Now().Add(3 * time.Second)
	var got backfillJob
	for time.Now().Before(deadline) {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/"+runResp.JobID, nil))
		_ = json.NewDecoder(rr.Body).Decode(&got)
		if got.Status != "running" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	const cap = 1 << 20
	if len(got.Output) < cap {
		t.Errorf("output should reach the 1MiB cap: got %d", len(got.Output))
	}
	if len(got.Output) > cap+200 {
		t.Errorf("output exceeded cap by more than the truncation marker: got %d", len(got.Output))
	}
	if !strings.Contains(got.Output, "output truncated at 1 MiB") {
		t.Errorf("missing truncation marker in output")
	}
}

// TestBackfillJobs_Unknown404 — polling a nonexistent job id surfaces
// 404, not 500. Lets the UI distinguish "job actually finished and got
// pruned" (future, not yet implemented) from "job id was bogus."
func TestBackfillJobs_Unknown404(t *testing.T) {
	server := newServerWithFakeExec(t, newFakeExec(&fakeExecResult{}))
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/backfill/jobs/deadbeef", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("status: got %d want 404", rr.Code)
	}
}

// TestBackfillJobsList_Empty — fresh dashboard with no kicks ever
// returns an empty jobs array (not null / missing field). Frontend
// safely tolerates either, but the contract is "always present, may
// be empty."
func TestBackfillJobsList_Empty(t *testing.T) {
	server := newServerWithFakeExec(t, newFakeExec(&fakeExecResult{}))
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/backfill/jobs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	var got struct {
		Jobs []backfillJob `json:"jobs"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Jobs == nil {
		t.Errorf("jobs field must be present (empty slice), got nil")
	}
	if len(got.Jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(got.Jobs))
	}
}

// TestBackfillJobsList_ReturnsRunningAndCompleted — after firing two
// successive jobs (one running, one done), the list endpoint returns
// both with the running entry first (newest started_at first).
// This is the wire backing the BackfillSection mount-time restore
// fix for the operator-flagged "running indicator disappears on
// navigation" UX bug: pre-fix the frontend's local jobs map reset
// to {} on remount; post-fix it pulls this list and re-keys by mode.
func TestBackfillJobsList_ReturnsRunningAndCompleted(t *testing.T) {
	// Mode-aware fake: --all takes 200ms (stays "running"); everything
	// else (including --message-id) returns immediately. One fake
	// installed at construction handles BOTH kicks — avoids mutating
	// server.execBackfill mid-test, which the prior version did and
	// which the race detector flagged because the first kick's
	// goroutine reads the field concurrently with the swap.
	fake := func(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
		isAll := false
		for _, a := range args {
			if a == "--all" {
				isAll = true
				break
			}
		}
		if isAll {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return -1, ctx.Err()
			}
		}
		return 0, nil
	}
	server := newServerWithFakeExec(t, fake)

	// Kick the long-running one (--all).
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"all"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("first run: %d %s", rr.Code, rr.Body.String())
	}

	// Kick the fast one (--message-id) — same fake, but the if-isAll
	// branch above lets it return immediately so this job reaches
	// status="done" while the first is still sleeping.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr,
		httptest.NewRequest(http.MethodPost, "/api/backfill/run", strings.NewReader(`{"mode":"message-id"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("second run: %d %s", rr.Code, rr.Body.String())
	}

	// Wait until the second job flips to done. Bounded so a regression
	// doesn't spin forever.
	deadline := time.Now().Add(2 * time.Second)
	var list struct {
		Jobs []backfillJob `json:"jobs"`
	}
	for time.Now().Before(deadline) {
		rr = httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(http.MethodGet, "/api/backfill/jobs", nil))
		if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		var doneCount, runningCount int
		for _, j := range list.Jobs {
			switch j.Status {
			case "done":
				doneCount++
			case "running":
				runningCount++
			}
		}
		// One running (the delayed first), one done (the fast second).
		if doneCount == 1 && runningCount == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(list.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(list.Jobs))
	}
	// Newest-first ordering: the second kick started later than the
	// first, so it must appear at index 0.
	if list.Jobs[0].Mode != "message-id" {
		t.Errorf("ordering: expected 'message-id' first (newer), got %q (older=%q)",
			list.Jobs[0].Mode, list.Jobs[1].Mode)
	}
	if list.Jobs[1].Mode != "all" {
		t.Errorf("ordering: expected 'all' second (older), got %q", list.Jobs[1].Mode)
	}
}

// TestBackfillJobsList_RejectsNonGet — POST/PUT/DELETE all yield 405.
// The list endpoint is read-only; mutations should go through
// POST /api/backfill/run.
func TestBackfillJobsList_RejectsNonGet(t *testing.T) {
	server := newServerWithFakeExec(t, newFakeExec(&fakeExecResult{}))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		server.Handler().ServeHTTP(rr,
			httptest.NewRequest(method, "/api/backfill/jobs", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/backfill/jobs: got %d want 405", method, rr.Code)
		}
	}
}

// jsonEscape produces a JSON-quoted string for embedding in inline
// test bodies. Tests use unusual characters in mode names so a naive
// concatenation would break.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
