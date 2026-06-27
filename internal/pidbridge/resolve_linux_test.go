//go:build linux

package pidbridge

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestParseHexAddrPort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want netip.AddrPort
	}{
		// 127.0.0.1 = 0x7F000001 → host-order 0100007F, port 8820 = 0x2274
		{"ipv4 loopback", "0100007F:2274", netip.MustParseAddrPort("127.0.0.1:8820")},
		{"ipv4 another port", "0100007F:D431", netip.MustParseAddrPort("127.0.0.1:54321")},
		// ::1 = 16 zero bytes then 1. Per-word-swap: the last 4 bytes become
		// 01000000 after swap, so that group encodes as "01000000". The
		// preceding 12 bytes are all-zero.
		{"ipv6 loopback", "00000000000000000000000001000000:2274", netip.MustParseAddrPort("[::1]:8820")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHexAddrPort(tc.in)
			if err != nil {
				t.Fatalf("parseHexAddrPort(%q): %v", tc.in, err)
			}
			if got.Port() != tc.want.Port() {
				t.Errorf("port: got %d want %d", got.Port(), tc.want.Port())
			}
			if got.Addr().Unmap() != tc.want.Addr().Unmap() {
				t.Errorf("addr: got %s want %s", got.Addr(), tc.want.Addr())
			}
		})
	}
}

// writeProcSocket constructs a fake /proc layout where a socket with
// inode `inode` is owned by pid `pid`. Returns the faked procDir.
func writeProcSocket(t *testing.T, pid int, inode uint64, localHexAddr, remoteHexAddr string, ppid int) string {
	t.Helper()
	procDir := t.TempDir()

	// /proc/net/tcp with our entry.
	netDir := filepath.Join(procDir, "net")
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		t.Fatalf("mkdir net: %v", err)
	}
	header := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	entry := fmt.Sprintf("   0: %s %s 01 00000000:00000000 00:00000000 00000000  1000        0 %d 1 0000000000000000 100 0 0 10 0\n",
		localHexAddr, remoteHexAddr, inode)
	if err := os.WriteFile(filepath.Join(netDir, "tcp"), []byte(header+entry), 0o644); err != nil {
		t.Fatalf("write net/tcp: %v", err)
	}
	// Empty tcp6 so findSocketInode sees it exists but has no match.
	if err := os.WriteFile(filepath.Join(netDir, "tcp6"), []byte(header), 0o644); err != nil {
		t.Fatalf("write net/tcp6: %v", err)
	}

	// /proc/<pid>/fd/3 → socket:[<inode>]
	pidDir := filepath.Join(procDir, strconv.Itoa(pid))
	fdDir := filepath.Join(pidDir, "fd")
	if err := os.MkdirAll(fdDir, 0o755); err != nil {
		t.Fatalf("mkdir fd: %v", err)
	}
	if err := os.Symlink(fmt.Sprintf("socket:[%d]", inode), filepath.Join(fdDir, "3")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// /proc/<pid>/status with PPid
	status := fmt.Sprintf("Name:\ttest\nPPid:\t%d\n", ppid)
	if err := os.WriteFile(filepath.Join(pidDir, "status"), []byte(status), 0o644); err != nil {
		t.Fatalf("write status: %v", err)
	}

	// Ancestor pid with its own status pointing at init (1).
	if ppid > 1 {
		ppidDir := filepath.Join(procDir, strconv.Itoa(ppid))
		if err := os.MkdirAll(ppidDir, 0o755); err != nil {
			t.Fatalf("mkdir ppid: %v", err)
		}
		pstatus := "Name:\tancestor\nPPid:\t1\n"
		if err := os.WriteFile(filepath.Join(ppidDir, "status"), []byte(pstatus), 0o644); err != nil {
			t.Fatalf("write ppid status: %v", err)
		}
	}
	return procDir
}

func TestProcResolver_Resolve_DirectPIDHit(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// Pid 4242 registered as owner of session "sess-x".
	if err := st.Write(ctx, Entry{PID: 4242, SessionID: "sess-x", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Fake /proc: pid 4242 owns inode 12345, whose local_address is
	// 127.0.0.1:54321 (hex "0100007F:D431"), rem_address is
	// 127.0.0.1:8820 (hex "0100007F:2274").
	procDir := writeProcSocket(t, 4242, 12345, "0100007F:D431", "0100007F:2274", 1)

	r := NewProcResolver(st, procDir, time.Minute)
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:54321")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !ok {
		t.Fatal("Resolve: miss, want hit")
	}
	if sid != "sess-x" {
		t.Fatalf("session: got %q want %q", sid, "sess-x")
	}
}

func TestProcResolver_Resolve_AncestorWalk(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)

	// Ancestor pid 100 registered; descendant pid 200 owns the socket.
	if err := st.Write(ctx, Entry{PID: 100, SessionID: "ancestor-sess", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	procDir := writeProcSocket(t, 200, 55555, "0100007F:1F40", "0100007F:2274", 100)

	r := NewProcResolver(st, procDir, time.Minute)
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !ok || sid != "ancestor-sess" {
		t.Fatalf("Resolve: sid=%q ok=%v", sid, ok)
	}
}

func TestProcResolver_Resolve_Miss(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	procDir := writeProcSocket(t, 777, 98765, "0100007F:D431", "0100007F:2274", 1)

	r := NewProcResolver(st, procDir, time.Minute)
	// No bridge entry for pid 777 → clean miss.
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:54321")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ok {
		t.Fatalf("Resolve: unexpected hit sid=%q", sid)
	}
}

func TestProcResolver_Resolve_NoMatchingSocket(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	// /proc layout with a different port than the one we query.
	procDir := writeProcSocket(t, 777, 98765, "0100007F:1F40", "0100007F:2274", 1)

	r := NewProcResolver(st, procDir, time.Minute)
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:54321")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ok || sid != "" {
		t.Fatalf("Resolve: want miss, got %q ok=%v", sid, ok)
	}
}

func TestProcResolver_CacheHit(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Write(ctx, Entry{PID: 500, SessionID: "cached", Tool: "claude-code"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	procDir := writeProcSocket(t, 500, 7777, "0100007F:1F40", "0100007F:2274", 1)

	r := NewProcResolver(st, procDir, time.Minute)
	if _, _, err := r.Resolve(ctx, "127.0.0.1:8000"); err != nil {
		t.Fatalf("Resolve 1: %v", err)
	}
	// Break the /proc layout; a cached resolve should still succeed.
	if err := os.WriteFile(filepath.Join(procDir, "net", "tcp"), []byte(""), 0o644); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("Resolve 2: %v", err)
	}
	if !ok || sid != "cached" {
		t.Fatalf("cache miss: sid=%q ok=%v", sid, ok)
	}
}

func TestProcResolver_EmptyAddr(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	r := NewProcResolver(st, t.TempDir(), time.Minute)
	sid, ok, err := r.Resolve(ctx, "")
	if sid != "" || ok || err != nil {
		t.Fatalf("empty addr: sid=%q ok=%v err=%v", sid, ok, err)
	}
}

// TestProcResolver_PoisonedInitEntryIgnored pins the D17 guard: a
// bridge entry registered at pid 2 (WSL distro init — wsl.exe-bridged
// hooks used to write these) must never resolve. The ancestor walk
// stops before system pids even when the poisoned row exists.
func TestProcResolver_PoisonedInitEntryIgnored(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Write(ctx, Entry{PID: 2, SessionID: "poison", Tool: "claude-code", CWD: `D:\somewhere`}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Socket owner 600 chains to init: 600 -> 2 (-> 1).
	procDir := writeProcSocket(t, 600, 31337, "0100007F:1F40", "0100007F:2274", 2)

	r := NewProcResolver(st, procDir, time.Minute)
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if ok || sid != "" {
		t.Fatalf("poisoned init entry resolved: sid=%q ok=%v", sid, ok)
	}
	tool, ok, err := r.ResolveTool(ctx, "127.0.0.1:8000")
	if err != nil || ok || tool != "" {
		t.Fatalf("poisoned tool resolved: tool=%q ok=%v err=%v", tool, ok, err)
	}
}

// TestProcResolver_ResolveCWD_ProcFallback pins the hookless-tool R3
// path: on a clean bridge miss the resolver reports the socket-owning
// process's own /proc/<pid>/cwd so per-project overrides work for
// clients with no SessionStart hook (codex exec). Session and tool
// stay unresolved.
func TestProcResolver_ResolveCWD_ProcFallback(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	procDir := writeProcSocket(t, 700, 42424, "0100007F:1F40", "0100007F:2274", 1)
	wsDir := t.TempDir()
	if err := os.Symlink(wsDir, filepath.Join(procDir, "700", "cwd")); err != nil {
		t.Fatalf("symlink cwd: %v", err)
	}

	r := NewProcResolver(st, procDir, time.Minute)
	cwd, ok, err := r.ResolveCWD(ctx, "127.0.0.1:8000")
	if err != nil {
		t.Fatalf("ResolveCWD: %v", err)
	}
	if !ok || cwd != wsDir {
		t.Fatalf("cwd fallback: got %q ok=%v want %q true", cwd, ok, wsDir)
	}
	// Session and tool remain clean misses for the same (cached) walk.
	if sid, ok, _ := r.Resolve(ctx, "127.0.0.1:8000"); ok || sid != "" {
		t.Fatalf("session resolved from cwd fallback: %q ok=%v", sid, ok)
	}
	if tool, ok, _ := r.ResolveTool(ctx, "127.0.0.1:8000"); ok || tool != "" {
		t.Fatalf("tool resolved from cwd fallback: %q ok=%v", tool, ok)
	}
}

// TestProcResolver_ResolveCWD_BridgeEntryWins pins precedence: when
// the ancestor walk hits a hook-fed bridge entry, its CWD wins over
// the /proc fallback (the hook payload's cwd is the session's true
// project directory; the socket owner may be a subprocess elsewhere).
func TestProcResolver_ResolveCWD_BridgeEntryWins(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Write(ctx, Entry{PID: 800, SessionID: "s", Tool: "claude-code", CWD: "/repo/project"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	procDir := writeProcSocket(t, 800, 53535, "0100007F:1F40", "0100007F:2274", 1)
	if err := os.Symlink(t.TempDir(), filepath.Join(procDir, "800", "cwd")); err != nil {
		t.Fatalf("symlink cwd: %v", err)
	}

	r := NewProcResolver(st, procDir, time.Minute)
	cwd, ok, err := r.ResolveCWD(ctx, "127.0.0.1:8000")
	if err != nil || !ok || cwd != "/repo/project" {
		t.Fatalf("bridge cwd should win: got %q ok=%v err=%v", cwd, ok, err)
	}
}

// TestProcResolver_ResolveTool reports the bridge entry's Tool for
// the same connection Resolve maps to a session — the R2 input for
// per-tool compression profile assignment. Shares Resolve's cache:
// the second call must not re-walk /proc (verified by deleting the
// fixture before it).
func TestProcResolver_ResolveTool(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	if err := st.Write(ctx, Entry{PID: 4242, SessionID: "sess-x", Tool: "kilo-code-cli"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	procDir := writeProcSocket(t, 4242, 12345, "0100007F:D431", "0100007F:2274", 1)

	r := NewProcResolver(st, procDir, time.Minute)
	tool, ok, err := r.ResolveTool(ctx, "127.0.0.1:54321")
	if err != nil {
		t.Fatalf("ResolveTool: %v", err)
	}
	if !ok || tool != "kilo-code-cli" {
		t.Fatalf("tool: got %q ok=%v, want kilo-code-cli true", tool, ok)
	}

	// Cached: removing the fake /proc must not break the second call
	// (one walk fills both Resolve and ResolveTool).
	if err := os.RemoveAll(procDir); err != nil {
		t.Fatal(err)
	}
	sid, ok, err := r.Resolve(ctx, "127.0.0.1:54321")
	if err != nil || !ok || sid != "sess-x" {
		t.Fatalf("cached Resolve after ResolveTool: sid=%q ok=%v err=%v", sid, ok, err)
	}

	// A miss reports cleanly.
	tool, ok, err = r.ResolveTool(ctx, "127.0.0.1:1")
	if err != nil || ok || tool != "" {
		t.Fatalf("miss: got %q ok=%v err=%v, want clean miss", tool, ok, err)
	}
}
