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
	if len(got.args) < 3 {
		t.Fatalf("subprocess args: got %v want at least [--all --config <path>]", got.args)
	}
	if got.args[0] != "--all" {
		t.Errorf("first arg: got %q want --all", got.args[0])
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

// jsonEscape produces a JSON-quoted string for embedding in inline
// test bodies. Tests use unusual characters in mode names so a naive
// concatenation would break.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
