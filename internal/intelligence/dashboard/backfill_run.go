package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// allowlistedBackfillModes is every flag the dashboard's Run-Now button
// is allowed to invoke. The set mirrors the modes surfaced on
// /api/backfill/status — file-walking + SQL-checkable. Locked down to
// prevent shell injection or arbitrary mode strings landing in
// `os/exec`. "all" is the umbrella that runs every backfill.
var allowlistedBackfillModes = map[string]string{
	"all":                     "--all",
	"is-sidechain":            "--is-sidechain",
	"cache-tier":              "--cache-tier",
	"message-id":              "--message-id",
	"opencode-message-id":     "--opencode-message-id",
	"opencode-parts":          "--opencode-parts",
	"opencode-tokens":         "--opencode-tokens",
	"openclaw-action-types":   "--openclaw-action-types",
	"openclaw-model":          "--openclaw-model",
	"openclaw-reasoning":      "--openclaw-reasoning",
	"codex-reasoning":         "--codex-reasoning",
	"cursor-model":            "--cursor-model",
	"copilot-message-id":      "--copilot-message-id",
	"pi-message-id":           "--pi-message-id",
	"claudecode-user-prompts": "--claudecode-user-prompts",
	"claudecode-api-errors":   "--claudecode-api-errors",
}

// backfillJob is one in-flight or completed run kicked from the
// dashboard. Stored in Server.backfillJobs keyed by its Id; the
// registry is in-memory, so a daemon restart drops history.
type backfillJob struct {
	ID        string    `json:"id"`
	Mode      string    `json:"mode"`
	Status    string    `json:"status"` // running | done | failed
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	ExitCode  int       `json:"exit_code,omitempty"`
	Output    string    `json:"output"` // captured stdout+stderr
	Error     string    `json:"error,omitempty"`
}

// backfillExecFn spawns a backfill subprocess. Args are passed verbatim
// to os/exec — caller is responsible for sanitization. onChunk is
// invoked whenever a chunk of stdout/stderr becomes available so the
// caller can stream output into the job registry; it may be called
// zero times (e.g. silent successful run) or many. Returns the exit
// code + any process-level error after the child terminates. Tests
// inject a fake.
type backfillExecFn func(ctx context.Context, args []string, onChunk func([]byte)) (int, error)

// realExecBackfill resolves the running observer binary via
// os.Executable() and invokes `<binary> backfill <args...>`. The child
// inherits the parent's env so OBSERVER_* overrides + $HOME are
// preserved.
//
// Streaming: stdout and stderr are piped (rather than CombinedOutput's
// all-at-once buffering) so onChunk can deliver chunks as they're
// produced. The dashboard poll endpoint then surfaces incremental
// progress — long-running file-walk backfills feel responsive rather
// than appearing dead until completion.
func realExecBackfill(ctx context.Context, args []string, onChunk func([]byte)) (int, error) {
	binary, err := osExecutable()
	if err != nil {
		return 0, fmt.Errorf("locate observer binary: %w", err)
	}
	cmd := exec.CommandContext(ctx, binary, append([]string{"backfill"}, args...)...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start: %w", err)
	}

	// Drain both pipes concurrently. 4 KiB read buffer is small enough
	// to surface progress promptly but big enough not to syscall on
	// every line. onChunk is single-threaded across stdout+stderr via
	// a tiny mutex so chunks land in the order they arrive without
	// interleaving across pipes.
	var pipeMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(r interface{ Read([]byte) (int, error) }) {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, readErr := r.Read(buf)
			if n > 0 && onChunk != nil {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				pipeMu.Lock()
				onChunk(chunk)
				pipeMu.Unlock()
			}
			if readErr != nil {
				return
			}
		}
	}
	go pipe(stdout)
	go pipe(stderr)
	wg.Wait()

	runErr := cmd.Wait()
	exit := 0
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	return exit, runErr
}

// osExecutable is a var so tests can override. Default delegates to
// os.Executable.
var osExecutable = func() (string, error) {
	// Lazy import: declared at package scope below to avoid an unused
	// import warning when tests stub this out.
	return osExecutableImpl()
}

// handleBackfillRun spawns a subprocess to run `observer backfill
// --<mode>` with the dashboard's resolved config path. Returns the
// generated job id immediately; the caller polls /api/backfill/jobs/<id>
// until status flips off "running".
//
// The job goroutine uses context.Background() so it survives the
// originating HTTP request — important for long-running file-walk
// modes the user kicked from the UI then closed the tab.
func (s *Server) handleBackfillRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	flag, ok := allowlistedBackfillModes[req.Mode]
	if !ok {
		http.Error(w, "unknown mode "+req.Mode+" — see /api/backfill/status for valid values",
			http.StatusBadRequest)
		return
	}

	jobID, err := newJobID()
	if err != nil {
		writeErr(w, fmt.Errorf("generate job id: %w", err))
		return
	}
	job := &backfillJob{
		ID:        jobID,
		Mode:      req.Mode,
		Status:    "running",
		StartedAt: time.Now().UTC(),
	}
	s.backfillMu.Lock()
	s.backfillJobs[jobID] = job
	s.backfillMu.Unlock()

	args := []string{flag}
	if s.opts.ConfigPath != "" {
		args = append(args, "--config", s.opts.ConfigPath)
	}

	go s.runBackfillJob(jobID, args)

	writeJSON(w, map[string]any{
		"job_id":     jobID,
		"mode":       req.Mode,
		"status":     "running",
		"started_at": job.StartedAt.Format(time.RFC3339),
	})
}

func (s *Server) runBackfillJob(id string, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// onChunk appends to job.Output under the registry mutex so
	// concurrent /api/backfill/jobs/<id> polls see partial output as
	// it accumulates. Cap the buffer at 1 MiB to keep memory bounded
	// — pathological backfills that print megabytes of debug get
	// truncated with a one-line note.
	const outputCap = 1 << 20 // 1 MiB
	onChunk := func(chunk []byte) {
		s.backfillMu.Lock()
		defer s.backfillMu.Unlock()
		job, ok := s.backfillJobs[id]
		if !ok {
			return
		}
		// Truncated already? Skip; once capped, ignore further chunks
		// so the truncation marker stays at the bottom.
		if len(job.Output) >= outputCap {
			return
		}
		room := outputCap - len(job.Output)
		if len(chunk) > room {
			job.Output += string(chunk[:room]) + "\n…(output truncated at 1 MiB)…\n"
		} else {
			job.Output += string(chunk)
		}
	}

	exit, err := s.execBackfill(ctx, args, onChunk)

	s.backfillMu.Lock()
	defer s.backfillMu.Unlock()
	job, ok := s.backfillJobs[id]
	if !ok {
		return
	}
	job.EndedAt = time.Now().UTC()
	job.ExitCode = exit
	if err != nil {
		job.Status = "failed"
		job.Error = err.Error()
		return
	}
	if exit != 0 {
		job.Status = "failed"
		job.Error = fmt.Sprintf("backfill exited with code %d", exit)
		return
	}
	job.Status = "done"
}

// handleBackfillJob serves GET /api/backfill/jobs/<id>. Returns the
// snapshot of the job — running jobs report partial output as it
// accumulates (well, only on completion in the current model — the
// child process runs to finish before output is captured. Streaming
// progress is a follow-up).
func (s *Server) handleBackfillJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/backfill/jobs/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "job id required", http.StatusBadRequest)
		return
	}
	s.backfillMu.Lock()
	job, ok := s.backfillJobs[id]
	if !ok {
		s.backfillMu.Unlock()
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	// Snapshot under the lock so the caller doesn't see a partially-mutated
	// job (the goroutine writes to it atomically under the same mutex).
	snap := *job
	s.backfillMu.Unlock()
	writeJSON(w, snap)
}

func newJobID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
