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
