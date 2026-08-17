//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/termsvc"
)

// bwrapUsableForTest reports whether bwrap resolves AND its user-namespace
// canary passes on this host — the same gate the runtime's Probe applies. The
// integration test t.Skip's unless both hold, so it never fails on a CI box
// without unprivileged user namespaces.
func bwrapUsableForTest(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bwrap not on PATH — skipping sandbox integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, path,
		"--ro-bind", "/", "/", "--tmpfs", "/tmp", "--die-with-parent", "--", "true").Run(); err != nil {
		t.Skipf("bwrap user-namespace canary failed (%v) — skipping sandbox integration test", err)
	}
	return path
}

// prepLiveWrapArgv builds a real sandboxRuntime over temp dirs and runs Prepare
// for a live-source claude-code workspace, returning the composed WrapArgv plus
// the temp home / workspace / observer dirs the assertions bind-check.
func prepLiveWrapArgv(t *testing.T) (wrap []string, home, workspace, observerDir string) {
	t.Helper()

	home = t.TempDir()
	// Plant a credential that must be BLINDED by the home tmpfs.
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ssh", "secret"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	observerDir = t.TempDir()
	workspace = t.TempDir()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newSandboxRuntime(
		config.TerminalSandboxConfig{Enabled: true, HomeMode: "tmpfs"},
		nil, observerDir, exe, nil,
	)
	if err != nil {
		t.Fatalf("newSandboxRuntime: %v", err)
	}

	res, err := rt.Prepare(context.Background(), termsvc.PrepareRequest{
		Tool:            "claude-code",
		ProjectRoot:     workspace,
		WorkspaceSource: "live",
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if res.Dir != workspace {
		t.Fatalf("live Dir = %q, want the project root %q", res.Dir, workspace)
	}
	if len(res.WrapArgv) < 3 {
		t.Fatalf("WrapArgv too short: %v", res.WrapArgv)
	}
	if res.WrapArgv[len(res.WrapArgv)-1] != "--" {
		t.Fatalf("WrapArgv must end in \"--\", got %v", res.WrapArgv)
	}
	// The runtime canonicalizes the observer dir via EvalSymlinks; mirror that
	// so the bind-writability assertion targets the same path bwrap bound.
	observerDir = rt.observerDir
	return res.WrapArgv, home, workspace, observerDir
}

// TestBwrapIntegrationBoundary is the plan §8 integration list, run for real on
// a host with a working bwrap: it wraps `bash -c <asserts>` in the Prepare-
// produced WrapArgv and checks the whole isolation contract at once — /usr
// read-only, the workspace + observer dir writable, the planted ~/.ssh/secret
// blinded by the home tmpfs, an inherited fd 3 surviving the exec, and a
// parent-opened 127.0.0.1 listener reachable inside (the shared-netns proof).
func TestBwrapIntegrationBoundary(t *testing.T) {
	bwrapUsableForTest(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH (needed for /dev/tcp) — skipping")
	}

	wrap, home, workspace, observerDir := prepLiveWrapArgv(t)

	// Parent-opened loopback listener: reachable inside iff the sandbox shares
	// the host network namespace (we deliberately do NOT --unshare-net).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_, _ = conn.Write([]byte("PONG"))
			_ = conn.Close()
		}
	}()

	// The fd-3 OOB survival marker (D4): a file we hand the child at fd 3.
	fd3 := filepath.Join(t.TempDir(), "fd3")
	if err := os.WriteFile(fd3, []byte("FD3_MARKER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fd3f, err := os.Open(fd3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd3f.Close() }()

	script := fmt.Sprintf(
		`
read -r line <&3 || exit 11
[ "$line" = "FD3_MARKER" ] || exit 12
if touch /usr/.sbtest 2>/dev/null; then rm -f /usr/.sbtest; exit 13; fi
touch %q || exit 14
touch %q || exit 15
[ ! -e "$HOME/.ssh/secret" ] || exit 16
exec 4<>/dev/tcp/127.0.0.1/%d || exit 17
printf PING >&4 || exit 18
echo BOUNDARY_OK
`,
		filepath.Join(workspace, ".wtest"),
		filepath.Join(observerDir, ".otest"),
		port,
	)

	argv := append(append([]string(nil), wrap...), "bash", "-c", script)
	//nolint:gosec // argv[0] is the Prepare-resolved bwrap path; the rest is the
	// test's own fixed script — no external input.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.ExtraFiles = []*os.File{fd3f}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed assertions failed: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "BOUNDARY_OK") {
		t.Fatalf("missing BOUNDARY_OK marker; output:\n%s", out)
	}
}

// TestBwrapIntegrationTeardown proves the G12 teardown ladder: a long-running
// inner process, launched as a process-group leader, is reaped within 3s when
// the group is killed (the same kill(-pgid) the unix PTY backend uses).
func TestBwrapIntegrationTeardown(t *testing.T) {
	bwrapUsableForTest(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH — skipping")
	}

	wrap, home, _, _ := prepLiveWrapArgv(t)
	argv := append(append([]string(nil), wrap...), "bash", "-c", "sleep 300")
	//nolint:gosec // argv[0] is the Prepare-resolved bwrap path; fixed test argv.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Give it a beat to establish the group, then kill the whole group.
	time.Sleep(200 * time.Millisecond)
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatalf("kill -pgid: %v", err)
	}
	select {
	case <-done:
		// reaped
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("inner process not reaped within 3s of SIGTERM to the group")
	}
}
