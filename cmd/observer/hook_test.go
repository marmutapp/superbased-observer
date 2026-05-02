package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
)

func TestDecidePreToolRewrite(t *testing.T) {
	bin := "/opt/observer"
	shellOn := config.Default()
	shellOn.Compression.Shell.Enabled = true
	shellOn.Compression.Shell.ExcludeCommands = []string{"curl"}

	shellOff := config.Default()
	shellOff.Compression.Shell.Enabled = false

	cases := []struct {
		name       string
		body       string
		cfg        config.Config
		cfgErr     error
		binary     string
		binErr     error
		wantRW     bool
		wantCmd    string
		wantReason string
	}{
		{
			name:       "bash rewrite",
			body:       `{"tool_name":"Bash","tool_input":{"command":"git status"}}`,
			cfg:        shellOn,
			binary:     bin,
			wantRW:     true,
			wantCmd:    bin + " run -- git status",
			wantReason: "ok",
		},
		{
			name:       "non-bash tool ignored",
			body:       `{"tool_name":"Read","tool_input":{"file_path":"foo.go"}}`,
			cfg:        shellOn,
			binary:     bin,
			wantRW:     false,
			wantReason: "",
		},
		{
			name:       "empty command ignored",
			body:       `{"tool_name":"Bash","tool_input":{"command":""}}`,
			cfg:        shellOn,
			binary:     bin,
			wantRW:     false,
			wantReason: "",
		},
		{
			name:       "excluded command passthrough",
			body:       `{"tool_name":"Bash","tool_input":{"command":"curl example.com"}}`,
			cfg:        shellOn,
			binary:     bin,
			wantRW:     false,
			wantReason: "not-rewritable",
		},
		{
			name:       "piped command passthrough",
			body:       `{"tool_name":"Bash","tool_input":{"command":"git log | head -5"}}`,
			cfg:        shellOn,
			binary:     bin,
			wantRW:     false,
			wantReason: "not-rewritable",
		},
		{
			name:       "shell disabled passthrough",
			body:       `{"tool_name":"Bash","tool_input":{"command":"git status"}}`,
			cfg:        shellOff,
			binary:     bin,
			wantRW:     false,
			wantReason: "shell-disabled",
		},
		{
			name:       "config error passthrough",
			body:       `{"tool_name":"Bash","tool_input":{"command":"git status"}}`,
			cfg:        config.Config{},
			cfgErr:     errors.New("load boom"),
			binary:     bin,
			wantRW:     false,
			wantReason: "config-error",
		},
		{
			name:       "binary lookup error passthrough",
			body:       `{"tool_name":"Bash","tool_input":{"command":"git status"}}`,
			cfg:        shellOn,
			binary:     "",
			binErr:     errors.New("no binary"),
			wantRW:     false,
			wantReason: "binary-lookup-error",
		},
		{
			name:       "garbage payload tolerated",
			body:       `not json`,
			cfg:        shellOn,
			binary:     bin,
			wantRW:     false,
			wantReason: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rw, cmd, reason := decidePreToolRewrite([]byte(tc.body), tc.cfg, tc.cfgErr, tc.binary, tc.binErr)
			if rw != tc.wantRW {
				t.Fatalf("rewrite: got %v want %v", rw, tc.wantRW)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason: got %q want %q", reason, tc.wantReason)
			}
			if rw && cmd != tc.wantCmd {
				t.Fatalf("command: got %q want %q", cmd, tc.wantCmd)
			}
		})
	}
}

func TestHandleClaudeCodePreToolAlwaysApproves(t *testing.T) {
	// Even with bogus JSON the hook must reply approve so the host doesn't
	// hang. This exercises the full handler wiring via config.Load against
	// the real (possibly missing) config file — safe because Load returns
	// defaults when the file doesn't exist.
	stdin := strings.NewReader(`{"tool_name":"Read","tool_input":{"file_path":"x"}}`)
	var stdout, stderr bytes.Buffer
	handleClaudeCodePreTool(stdin, &stdout, &stderr, "claude-code:pre-tool")

	var reply preToolReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("reply not JSON: %v — %q", err, stdout.String())
	}
	if reply.Decision != "approve" {
		t.Fatalf("decision: got %q want approve", reply.Decision)
	}
	if !reply.Continue {
		t.Fatal("continue should be true")
	}
	if reply.HookSpecificOutput != nil {
		t.Fatal("Read tool should not carry an updatedInput")
	}
}

func TestHandleClaudeCodePreToolEmptyPayload(t *testing.T) {
	var stdout, stderr bytes.Buffer
	handleClaudeCodePreTool(strings.NewReader(""), &stdout, &stderr, "claude-code:pre-tool")
	var reply preToolReply
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("reply not JSON: %v", err)
	}
	if reply.Decision != "approve" {
		t.Fatal("empty payload must still approve")
	}
}

// ancestorsList returns a fake ancestors function that always yields
// the given PIDs. Used by tests to avoid touching /proc.
func ancestorsList(pids ...int) ancestorsFunc {
	return func(int) []int { return pids }
}

func TestHandleClaudeCodeSessionStart_WritesBridge(t *testing.T) {
	payload := `{"session_id":"s-123","cwd":"/repo","hook_event_name":"SessionStart","source":"startup"}`
	var stdout, stderr bytes.Buffer
	var captured pidbridge.Entry
	var called int
	writer := func(_ context.Context, e pidbridge.Entry) error {
		called++
		captured = e
		return nil
	}
	handleClaudeCodeSessionStart(context.Background(), 4242, ancestorsList(4242),
		strings.NewReader(payload), &stdout, &stderr,
		"claude-code:session-start", writer)

	// Must always approve.
	var reply map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("reply not JSON: %v — %q", err, stdout.String())
	}
	if reply["decision"] != "approve" {
		t.Fatalf("decision: %v", reply["decision"])
	}
	if called != 1 {
		t.Fatalf("writer called %d times, want 1", called)
	}
	if captured.PID != 4242 || captured.SessionID != "s-123" || captured.CWD != "/repo" || captured.Tool != "claude-code" {
		t.Fatalf("entry: %+v", captured)
	}
}

func TestHandleClaudeCodeSessionStart_WritesAllAncestors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var got []int
	writer := func(_ context.Context, e pidbridge.Entry) error {
		got = append(got, e.PID)
		if e.SessionID != "s-multi" {
			t.Errorf("session_id on pid=%d: %q", e.PID, e.SessionID)
		}
		return nil
	}
	// Simulates hook spawned off a short-lived worker (worker=999,
	// claude-main=100). Both should land in the bridge so the
	// resolver still finds claude-main after the worker exits.
	handleClaudeCodeSessionStart(context.Background(), 999, ancestorsList(999, 100),
		strings.NewReader(`{"session_id":"s-multi","cwd":"/repo"}`),
		&stdout, &stderr, "claude-code:session-start", writer)
	if len(got) != 2 || got[0] != 999 || got[1] != 100 {
		t.Errorf("registered pids = %v, want [999 100]", got)
	}
}

func TestHandleClaudeCodeSessionStart_NoAncestorsWarns(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var called int
	writer := func(_ context.Context, _ pidbridge.Entry) error {
		called++
		return nil
	}
	// Empty ancestor list simulates the "immediate parent is a shell"
	// case — we never register shells, so there's nothing to do.
	handleClaudeCodeSessionStart(context.Background(), 4242, ancestorsList(),
		strings.NewReader(`{"session_id":"s"}`), &stdout, &stderr,
		"claude-code:session-start", writer)
	if called != 0 {
		t.Errorf("writer called %d times, want 0 when ancestors is empty", called)
	}
	if !strings.Contains(stderr.String(), "no ancestor pids") {
		t.Errorf("stderr should explain the empty ancestors: %q", stderr.String())
	}
}

func TestHandleClaudeCodeSessionStart_NoSessionID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var called int
	writer := func(_ context.Context, _ pidbridge.Entry) error {
		called++
		return nil
	}
	handleClaudeCodeSessionStart(context.Background(), 4242, ancestorsList(4242),
		strings.NewReader(`{"cwd":"/x"}`), &stdout, &stderr,
		"claude-code:session-start", writer)

	var reply map[string]any
	_ = json.Unmarshal(stdout.Bytes(), &reply)
	if reply["decision"] != "approve" {
		t.Fatal("must still approve without session_id")
	}
	if called != 0 {
		t.Fatalf("writer called %d times, want 0 on missing session_id", called)
	}
	if !strings.Contains(stderr.String(), "no session_id") {
		t.Errorf("stderr should mention missing session_id: %q", stderr.String())
	}
}

func TestHandleClaudeCodeSessionStart_RefusesInitPID(t *testing.T) {
	var stdout, stderr bytes.Buffer
	var called int
	writer := func(_ context.Context, _ pidbridge.Entry) error {
		called++
		return nil
	}
	// ppid=1 means we were reparented to init; don't register.
	handleClaudeCodeSessionStart(context.Background(), 1, ancestorsList(1),
		strings.NewReader(`{"session_id":"s"}`), &stdout, &stderr,
		"claude-code:session-start", writer)
	if called != 0 {
		t.Fatalf("writer called %d times, want 0 for pid 1", called)
	}
}

func TestHandleClaudeCodeSessionStart_WriterErrorStillApproves(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writer := func(_ context.Context, _ pidbridge.Entry) error {
		return errors.New("boom")
	}
	handleClaudeCodeSessionStart(context.Background(), 4242, ancestorsList(4242),
		strings.NewReader(`{"session_id":"s"}`), &stdout, &stderr,
		"claude-code:session-start", writer)

	var reply map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("reply not JSON: %v", err)
	}
	if reply["decision"] != "approve" {
		t.Fatal("writer error must not block approval")
	}
	if !strings.Contains(stderr.String(), "pidbridge pid=4242: boom") {
		t.Errorf("stderr should log writer error with pid: %q", stderr.String())
	}
}

// writeFakeProc creates /proc/<pid>/{comm,status,cmdline} under base
// so tests can exercise collectClaudeCodeAncestors without touching
// /proc. cmdline is NUL-separated argv; empty cmdline omits the file
// (mirrors a kernel thread).
func writeFakeProc(t *testing.T, base string, pid int, comm string, ppid int, cmdline ...string) {
	t.Helper()
	d := filepath.Join(base, strconv.Itoa(pid))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "status"), []byte(fmt.Sprintf("Name:\t%s\nPPid:\t%d\n", comm, ppid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(cmdline) > 0 {
		var buf []byte
		for _, a := range cmdline {
			buf = append(buf, a...)
			buf = append(buf, 0)
		}
		if err := os.WriteFile(filepath.Join(d, "cmdline"), buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCollectClaudeCodeAncestors_CrossesBashDashCWrapper reproduces
// the observed Claude Code hook-spawn shape: `bash -c 'observer hook
// ...'`. The walker must skip the bash wrapper and reach the real
// long-lived claude parent behind it.
func TestCollectClaudeCodeAncestors_CrossesBashDashCWrapper(t *testing.T) {
	procDir := t.TempDir()
	// bash -c (wrapper, 200) -> claude (100) -> bash login shell (50)
	writeFakeProc(t, procDir, 200, "bash", 100, "/bin/bash", "-c", "/path/to/observer hook claude-code session-start")
	writeFakeProc(t, procDir, 100, "claude", 50, "claude")
	writeFakeProc(t, procDir, 50, "bash", 1, "-bash")

	got := collectClaudeCodeAncestors(200, procDir, 10)
	want := []int{100}
	if !equalInts(got, want) {
		t.Errorf("got %v want %v (bash -c should be skipped, interactive bash should stop)", got, want)
	}
}

// TestCollectClaudeCodeAncestors_InteractiveShellStartStops verifies
// the manual-invocation case (user types the hook command directly in
// their shell): we refuse to register an interactive shell.
func TestCollectClaudeCodeAncestors_InteractiveShellStartStops(t *testing.T) {
	procDir := t.TempDir()
	writeFakeProc(t, procDir, 300, "bash", 1, "bash")
	if got := collectClaudeCodeAncestors(300, procDir, 10); len(got) != 0 {
		t.Errorf("interactive shell at start should return empty, got %v", got)
	}
}

// TestCollectClaudeCodeAncestors_DeadStartPID verifies that when the
// immediate parent has vanished (no /proc/<pid>/comm), we still
// best-effort register startPID so we preserve the pre-fix floor
// behaviour.
func TestCollectClaudeCodeAncestors_DeadStartPID(t *testing.T) {
	procDir := t.TempDir()
	// no /proc entry for 999 — simulates a zombie reaped before we read.
	got := collectClaudeCodeAncestors(999, procDir, 10)
	want := []int{999}
	if !equalInts(got, want) {
		t.Errorf("got %v want %v (dead parent should still be registered)", got, want)
	}
}

func TestCollectClaudeCodeAncestors_StopsAtShell(t *testing.T) {
	procDir := t.TempDir()
	// hook parent=200 (node worker) -> 100 (claude) -> 50 (bash)
	writeFakeProc(t, procDir, 200, "node", 100)
	writeFakeProc(t, procDir, 100, "claude", 50)
	writeFakeProc(t, procDir, 50, "bash", 42)

	got := collectClaudeCodeAncestors(200, procDir, 10)
	want := []int{200, 100}
	if !equalInts(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestCollectClaudeCodeAncestors_StopsAtInit(t *testing.T) {
	procDir := t.TempDir()
	writeFakeProc(t, procDir, 300, "node", 200)
	writeFakeProc(t, procDir, 200, "node", 100)
	writeFakeProc(t, procDir, 100, "claude", 1)

	got := collectClaudeCodeAncestors(300, procDir, 10)
	want := []int{300, 200, 100}
	if !equalInts(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestCollectClaudeCodeAncestors_CapsAtMaxDepth(t *testing.T) {
	procDir := t.TempDir()
	for i := 10; i > 0; i-- {
		writeFakeProc(t, procDir, 1000+i, "node", 1000+i-1)
	}
	writeFakeProc(t, procDir, 1000, "node", 1)

	got := collectClaudeCodeAncestors(1010, procDir, 3)
	if len(got) != 3 {
		t.Fatalf("expected len 3 got %d: %v", len(got), got)
	}
}

func TestCollectClaudeCodeAncestors_MissingProcEntry(t *testing.T) {
	procDir := t.TempDir()
	writeFakeProc(t, procDir, 200, "node", 100)
	// PID 100 intentionally absent — simulates the observed bug:
	// immediate parent recorded, grandparent already dead.
	got := collectClaudeCodeAncestors(200, procDir, 10)
	want := []int{200}
	if !equalInts(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestCollectClaudeCodeAncestors_StartPIDIsShell(t *testing.T) {
	procDir := t.TempDir()
	writeFakeProc(t, procDir, 80, "zsh", 1)
	if got := collectClaudeCodeAncestors(80, procDir, 10); len(got) != 0 {
		t.Errorf("shell at start should return empty, got %v", got)
	}
}

func TestCollectClaudeCodeAncestors_InitOrBadStart(t *testing.T) {
	if got := collectClaudeCodeAncestors(0, "/nonexistent", 10); len(got) != 0 {
		t.Errorf("pid=0 should return empty, got %v", got)
	}
	if got := collectClaudeCodeAncestors(1, "/nonexistent", 10); len(got) != 0 {
		t.Errorf("pid=1 should return empty, got %v", got)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
