package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
)

// NOTE: every test in this file is spend-free — no claude/codex binary is ever
// launched against a real API. Drive tests use a stub shell script; Preflight
// and invocation-shape tests are pure.

// --- Preflight: the isolated-daemon gate (re-spike §6.3) ---

func TestClaudeCodePreflight(t *testing.T) {
	t.Parallel()
	def, err := defaultObserverDBPath()
	if err != nil {
		t.Fatalf("defaultObserverDBPath: %v", err)
	}

	dir := t.TempDir()
	smallDB := filepath.Join(dir, "isolated.db")
	if err := os.WriteFile(smallDB, []byte("tiny isolated benchmark db"), 0o600); err != nil {
		t.Fatalf("write small db: %v", err)
	}
	bigDB := filepath.Join(dir, "big.db")
	if err := os.WriteFile(bigDB, make([]byte, 2048), 0o600); err != nil {
		t.Fatalf("write big db: %v", err)
	}

	tests := []struct {
		name       string
		dbPath     string
		maxDBBytes int64 // 0 → default ceiling
		wantErr    bool
		wantSubstr string
	}{
		{
			name:       "default production DB path rejected",
			dbPath:     def,
			wantErr:    true,
			wantSubstr: "operator default DB",
		},
		{
			name:       "default path rejected even with trailing separator noise",
			dbPath:     def + string(filepath.Separator),
			wantErr:    true,
			wantSubstr: "operator default DB",
		},
		{
			name:       "empty DB path rejected",
			dbPath:     "",
			wantErr:    true,
			wantSubstr: "no observer DB path",
		},
		{
			name:       "oversized DB rejected",
			dbPath:     bigDB,
			maxDBBytes: 1024, // 2048-byte file > 1024-byte ceiling
			wantErr:    true,
			wantSubstr: "ceiling",
		},
		{
			name:   "small isolated DB accepted",
			dbPath: smallDB,
		},
		{
			name:   "not-yet-created DB accepted (fresh isolated daemon)",
			dbPath: filepath.Join(dir, "does-not-exist-yet.db"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := newClaudeCodeDriver("", tc.dbPath)
			if tc.maxDBBytes > 0 {
				d.maxDBBytes = tc.maxDBBytes
			}
			err := d.Preflight()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Preflight() = nil, want ErrClaudeCodeNotIsolated")
				}
				if !strings.Contains(err.Error(), ErrClaudeCodeNotIsolated.Error()) {
					t.Errorf("error %q does not wrap ErrClaudeCodeNotIsolated", err)
				}
				if tc.wantSubstr != "" && !strings.Contains(err.Error(), tc.wantSubstr) {
					t.Errorf("error %q missing %q", err, tc.wantSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Preflight() = %v, want nil", err)
			}
		})
	}
}

// --- Command construction (re-spike §6.1 invocation shape) ---

func TestClaudeInvocationShape(t *testing.T) {
	t.Parallel()
	req := DriveRequest{
		Prompt:       "write PING to hello.txt",
		Model:        "claude-haiku-4-5",
		WorkspaceDir: "/scratch/ws/conc1",
		HomeDir:      "/scratch/home1",
		ProxyURL:     "http://127.0.0.1:18820",
	}
	args, env := claudeInvocation(req, "11111111-2222-4333-8444-555555555555")

	wantArgs := []string{
		"-p", "write PING to hello.txt",
		"--model", "claude-haiku-4-5",
		"--output-format", "json",
		"--session-id", "11111111-2222-4333-8444-555555555555",
		"--dangerously-skip-permissions",
	}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], wantArgs[i])
		}
	}

	wantEnv := map[string]bool{
		"CLAUDE_CONFIG_DIR=" + filepath.Join("/scratch/home1", ".claude"): false,
		"ANTHROPIC_BASE_URL=http://127.0.0.1:18820":                       false,
		"ENABLE_TOOL_SEARCH=true":                                         false,
	}
	for _, kv := range env {
		if _, ok := wantEnv[kv]; ok {
			wantEnv[kv] = true
		}
	}
	for kv, seen := range wantEnv {
		if !seen {
			t.Errorf("env missing %q (got %v)", kv, env)
		}
	}
}

func TestNewBenchmarkSessionIDIsV4UUID(t *testing.T) {
	t.Parallel()
	v4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := newBenchmarkSessionID()
		if !v4.MatchString(id) {
			t.Fatalf("minted id %q is not an RFC-4122 v4 uuid", id)
		}
		if seen[id] {
			t.Fatalf("minted id %q repeated", id)
		}
		seen[id] = true
	}
}

// --- Drive: argv/env threading + result parsing via a stub binary ---

// writeStubClaude writes a shell script that records its argv + the sandbox env
// vars into the workspace, then emits claude-shaped print-mode JSON. No network,
// no spend.
func writeStubClaude(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "claude-stub.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil { //nolint:gosec // test stub must be executable
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestClaudeCodeDriveThreadsSessionIDAndEnv(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("stub driver script requires sh")
	}
	ws := t.TempDir()
	home := t.TempDir()
	stub := writeStubClaude(t, t.TempDir(),
		`printf '%s\n' "$@" > args.txt
printf '%s\n' "$CLAUDE_CONFIG_DIR" "$ANTHROPIC_BASE_URL" "$ENABLE_TOOL_SEARCH" > env.txt
printf '{"type":"result","result":"PONG","session_id":"echoed-back","is_error":false}\n'
`)

	minted := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	d := newClaudeCodeDriver(stub, filepath.Join(t.TempDir(), "isolated.db"))
	d.mintID = func() string { return minted }

	res, err := d.Drive(context.Background(), DriveRequest{
		Prompt: "say PONG", Model: "claude-haiku-4-5",
		WorkspaceDir: ws, HomeDir: home, ProxyURL: "http://127.0.0.1:18820",
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("res = %+v, want exit 0 / no timeout", res)
	}
	// The MINTED id is the correlation key — echoed output never replaces it.
	if len(res.SessionIDs) != 1 || res.SessionIDs[0] != minted {
		t.Errorf("SessionIDs = %v, want [%s]", res.SessionIDs, minted)
	}
	if res.FinalAnswer != "PONG" {
		t.Errorf("FinalAnswer = %q, want PONG", res.FinalAnswer)
	}

	argv, err := os.ReadFile(filepath.Join(ws, "args.txt"))
	if err != nil {
		t.Fatalf("stub did not run in the workspace dir: %v", err)
	}
	got := string(argv)
	for _, want := range []string{"-p", "say PONG", "--model", "claude-haiku-4-5", "--output-format", "json", "--session-id", minted, "--dangerously-skip-permissions"} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}

	envOut, err := os.ReadFile(filepath.Join(ws, "env.txt"))
	if err != nil {
		t.Fatalf("read env.txt: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(envOut)), "\n")
	if len(lines) != 3 {
		t.Fatalf("env.txt = %q, want 3 lines", envOut)
	}
	if lines[0] != filepath.Join(home, ".claude") {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want %q", lines[0], filepath.Join(home, ".claude"))
	}
	if lines[1] != "http://127.0.0.1:18820" {
		t.Errorf("ANTHROPIC_BASE_URL = %q", lines[1])
	}
	if lines[2] != "true" {
		t.Errorf("ENABLE_TOOL_SEARCH = %q", lines[2])
	}
}

func TestClaudeCodeDriveNonZeroExitIsOutcomeNotError(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("stub driver script requires sh")
	}
	stub := writeStubClaude(t, t.TempDir(),
		`printf '{"type":"result","result":"partial","is_error":true}\n'
exit 3
`)
	d := newClaudeCodeDriver(stub, filepath.Join(t.TempDir(), "isolated.db"))
	res, err := d.Drive(context.Background(), DriveRequest{
		Prompt: "p", Model: "m", WorkspaceDir: t.TempDir(), HomeDir: t.TempDir(), ProxyURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("Drive returned error for non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.FinalAnswer != "partial" {
		t.Errorf("FinalAnswer = %q, want partial", res.FinalAnswer)
	}
	if len(res.SessionIDs) != 1 {
		t.Errorf("minted session id must survive a failed turn: %v", res.SessionIDs)
	}
}

func TestClaudeCodeDriveTimeoutKillsAndFlags(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("stub driver script requires sh")
	}
	stub := writeStubClaude(t, t.TempDir(), "sleep 30\n")
	d := newClaudeCodeDriver(stub, filepath.Join(t.TempDir(), "isolated.db"))
	start := time.Now()
	res, err := d.Drive(context.Background(), DriveRequest{
		Prompt: "p", Model: "m", WorkspaceDir: t.TempDir(), HomeDir: t.TempDir(),
		ProxyURL: "http://127.0.0.1:1", TimeoutSec: 1,
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("TimedOut = false, want true (res %+v)", res)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("timeout did not kill the process group promptly (%v)", elapsed)
	}
}

func TestClaudeCodeDriveMissingBinaryIsError(t *testing.T) {
	t.Parallel()
	d := newClaudeCodeDriver(filepath.Join(t.TempDir(), "no-such-claude"), filepath.Join(t.TempDir(), "x.db"))
	// A configured-but-missing binary surfaces at exec as exit -1; an
	// unconfigured binary missing from PATH surfaces as a Drive error. Either
	// way no spend happens. Here: configured path that doesn't exist.
	res, err := d.Drive(context.Background(), DriveRequest{
		Prompt: "p", Model: "m", WorkspaceDir: t.TempDir(), HomeDir: t.TempDir(), ProxyURL: "http://127.0.0.1:1",
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatalf("expected a failure for a missing binary, got %+v", res)
	}
}

// --- Final-answer parsing ---

func TestParseClaudeResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		out    string
		want   string
		wantOK bool
	}{
		{
			name:   "single result object",
			out:    `{"type":"result","subtype":"success","result":"PING","session_id":"abc","is_error":false}`,
			want:   "PING",
			wantOK: true,
		},
		{
			name: "result line after interleaved stderr noise",
			out: "some stderr warning\n" +
				`{"type":"system","subtype":"init"}` + "\n" +
				`{"type":"result","result":"DONE","session_id":"abc"}` + "\n",
			want:   "DONE",
			wantOK: true,
		},
		{
			name:   "no result recoverable",
			out:    "claude: fatal: something broke\n",
			wantOK: false,
		},
		{
			name:   "empty output",
			out:    "",
			wantOK: false,
		},
		{
			name:   "error result still recovered (scorer decides)",
			out:    `{"type":"result","result":"I could not finish","is_error":true}`,
			want:   "I could not finish",
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseClaudeResult([]byte(tc.out))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("result = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Correlation seam against fixture rows (re-spike §"Correlation seam") ---

// TestClaudeCorrelationSeamFixture pins the codex-identical seam on fixture
// rows: the minted --session-id lands verbatim as sessions.id (hook ingestion),
// resolves through the sessions row to the unique per-attempt workspace root,
// and an unknown id stays unresolved.
func TestClaudeCorrelationSeamFixture(t *testing.T) {
	t.Parallel()
	st, database := newRunnerStore(t)
	ctx := context.Background()

	minted := newBenchmarkSessionID()
	workspace := filepath.Join(t.TempDir(), "ws", "conc1")
	now := time.Now().UTC().Format(time.RFC3339)

	if _, err := database.ExecContext(ctx,
		`INSERT INTO projects (id, root_path, created_at) VALUES (NULL, ?, ?)`, workspace, now); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	var pid int64
	if err := database.QueryRowContext(ctx, `SELECT id FROM projects WHERE root_path = ?`, workspace).Scan(&pid); err != nil {
		t.Fatalf("project id: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO sessions (id, project_id, tool, model, started_at) VALUES (?, ?, 'claude-code', 'claude-haiku-4-5', ?)`,
		minted, pid, now); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := database.ExecContext(ctx,
		`INSERT INTO api_turns (session_id, timestamp, provider, model, input_tokens, output_tokens, cache_read_tokens, cost_usd, http_status)
		 VALUES (?, ?, 'anthropic', 'claude-haiku-4-5', 900, 120, 400, 0.0311, NULL)`, minted, now); err != nil {
		t.Fatalf("seed turn: %v", err)
	}

	fact, ok, err := st.LoadSessionCorrelation(ctx, minted)
	if err != nil || !ok {
		t.Fatalf("LoadSessionCorrelation(%q): ok=%v err=%v", minted, ok, err)
	}
	if fact.RootPath != workspace {
		t.Errorf("RootPath = %q, want %q", fact.RootPath, workspace)
	}
	if fact.Tool != "claude-code" || fact.Model != "claude-haiku-4-5" {
		t.Errorf("fact = %+v", fact)
	}
	if !rootMatches(fact.RootPath, workspace) {
		t.Errorf("rootMatches(%q, %q) = false", fact.RootPath, workspace)
	}

	bill, err := st.LoadSessionBilling(ctx, minted)
	if err != nil {
		t.Fatalf("LoadSessionBilling: %v", err)
	}
	if bill.Turns != 1 || bill.CostUSD < 0.031 || bill.CostUSD > 0.032 {
		t.Errorf("billing = %+v, want 1 turn ~$0.0311", bill)
	}

	if _, ok, _ := st.LoadSessionCorrelation(ctx, newBenchmarkSessionID()); ok {
		t.Error("an unknown minted id must not resolve")
	}
}

// TestRunnerClaudeCodeEndToEnd drives the RUNNER with a claude-shaped sim
// (minted uuid == sessions.id, unique workspace root) and asserts the attempt
// resolves ok with billed cost attached — the full §3.3 seam, no live session.
func TestRunnerClaudeCodeEndToEnd(t *testing.T) {
	t.Parallel()
	st, database := newRunnerStore(t)
	runner := mkRunner(st, database, simDriver{name: "claude-code", db: database, cost: 0.04}, fakeScorer{pass: true})
	spec := specTOML(t, `
name = "cc-e2e"
repeats = 2
[budget]
  max_total_usd = 100.0
  max_cell_usd = 10.0
[analysis]
  baseline_config = "cc"
  noninferiority_margin = 0.1
  min_sample = 1
[[tasks]]
  id = "t1"
  repo = "https://x/y.git"
  ref = "abc123"
  prompt = "do the thing"
  [tasks.success]
    scorer = "tests_pass"
    cmd = "true"
[[configs]]
  id = "cc"
  harness = "claude-code"
  model = "claude-haiku-4-5"
`)
	runID, err := runner.Run(context.Background(), spec, RunOptions{
		Confirmed: true, RootDir: t.TempDir(), ObserverVersion: "test",
		ResolveAttempts: 3, ResolveDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	facts, err := st.LoadBenchmarkFacts(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadBenchmarkFacts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 attempts, got %d", len(facts))
	}
	for _, f := range facts {
		if f.Status != benchmark.StatusOK {
			t.Errorf("status = %q, want ok", f.Status)
		}
		if f.Sessions != 1 {
			t.Errorf("sessions = %d, want 1", f.Sessions)
		}
	}
}
