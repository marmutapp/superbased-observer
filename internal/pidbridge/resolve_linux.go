//go:build linux

package pidbridge

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcResolver is the Linux implementation of [Resolver]. It scans
// /proc/net/tcp and /proc/net/tcp6 to map a client TCP remote addr to a
// socket inode, then scans /proc/<pid>/fd to find the owning pid, then
// walks up the PPid chain looking for a bridge entry. Results are
// cached for CacheTTL per remote addr.
//
// The ancestor walk matters because Claude Code's own process is the
// pid captured by the hook, but in principle a child process could be
// the one that opened the socket. In practice Claude Code opens the
// HTTP connection itself, so the first lookup hits.
//
// A zero-value ProcResolver is unusable — use [NewProcResolver].
type ProcResolver struct {
	store    *Store
	procDir  string
	cacheTTL time.Duration
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	sessionID string
	ok        bool
	expires   time.Time
}

// NewProcResolver constructs a ProcResolver. procDir defaults to "/proc"
// when empty; tests pass a temp directory. cacheTTL defaults to 30s when
// zero.
func NewProcResolver(store *Store, procDir string, cacheTTL time.Duration) *ProcResolver {
	if procDir == "" {
		procDir = "/proc"
	}
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Second
	}
	return &ProcResolver{
		store:    store,
		procDir:  procDir,
		cacheTTL: cacheTTL,
		now:      time.Now,
		cache:    map[string]cacheEntry{},
	}
}

// SetClock overrides the resolver's clock. Not safe for concurrent use
// with Resolve; call once at construction.
func (r *ProcResolver) SetClock(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

// Resolve implements [Resolver]. remoteAddr is the "IP:port" the server
// sees for the incoming connection (from http.Request.RemoteAddr).
func (r *ProcResolver) Resolve(ctx context.Context, remoteAddr string) (string, bool, error) {
	if remoteAddr == "" {
		return "", false, nil
	}
	now := r.now()
	r.mu.Lock()
	if e, ok := r.cache[remoteAddr]; ok && now.Before(e.expires) {
		r.mu.Unlock()
		return e.sessionID, e.ok, nil
	}
	r.mu.Unlock()

	sessionID, ok, err := r.resolveUncached(ctx, remoteAddr)
	if err != nil {
		return "", false, err
	}
	r.mu.Lock()
	r.cache[remoteAddr] = cacheEntry{
		sessionID: sessionID,
		ok:        ok,
		expires:   now.Add(r.cacheTTL),
	}
	r.mu.Unlock()
	return sessionID, ok, nil
}

func (r *ProcResolver) resolveUncached(ctx context.Context, remoteAddr string) (string, bool, error) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", false, fmt.Errorf("pidbridge.Resolve: parse %q: %w", remoteAddr, err)
	}
	inode, err := r.findSocketInode(ap)
	if err != nil {
		return "", false, err
	}
	if inode == 0 {
		return "", false, nil
	}
	pid, err := r.findPIDForInode(inode)
	if err != nil {
		return "", false, err
	}
	if pid == 0 {
		return "", false, nil
	}
	return r.walkAncestors(ctx, pid)
}

// findSocketInode reads /proc/net/tcp and /proc/net/tcp6 looking for an
// entry whose local_address matches target. The proxy's server sees the
// client-side tuple as the request's RemoteAddr, which in /proc/net/tcp
// appears as local_address of the client socket.
func (r *ProcResolver) findSocketInode(target netip.AddrPort) (uint64, error) {
	for _, name := range []string{"net/tcp", "net/tcp6"} {
		inode, err := scanNetTCP(filepath.Join(r.procDir, name), target)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if inode != 0 {
			return inode, nil
		}
	}
	return 0, nil
}

func scanNetTCP(path string, target netip.AddrPort) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}
		local, err := parseHexAddrPort(fields[1])
		if err != nil {
			continue
		}
		if !addrsMatch(local, target) {
			continue
		}
		inode, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		return inode, nil
	}
	return 0, scanner.Err()
}

// addrsMatch compares two AddrPort values tolerant of IPv4-mapped-IPv6
// (::ffff:127.0.0.1 vs 127.0.0.1).
func addrsMatch(a, b netip.AddrPort) bool {
	if a.Port() != b.Port() {
		return false
	}
	return a.Addr().Unmap() == b.Addr().Unmap()
}

// parseHexAddrPort parses the "HEXIP:HEXPORT" format used by /proc/net/tcp.
// The IP portion is laid out in host byte order per 4-byte group; on
// little-endian platforms that means each word needs byte-swapping.
func parseHexAddrPort(s string) (netip.AddrPort, error) {
	colon := strings.IndexByte(s, ':')
	if colon < 0 {
		return netip.AddrPort{}, fmt.Errorf("pidbridge: bad addr %q", s)
	}
	ipHex := s[:colon]
	portHex := s[colon+1:]
	raw, err := hex.DecodeString(ipHex)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("pidbridge: hex decode ip %q: %w", ipHex, err)
	}
	if len(raw) != 4 && len(raw) != 16 {
		return netip.AddrPort{}, fmt.Errorf("pidbridge: unexpected ip length %d", len(raw))
	}
	// Kernel writes each 32-bit group in host byte order. Reverse each
	// 4-byte chunk to get network byte order.
	for i := 0; i+3 < len(raw); i += 4 {
		raw[i], raw[i+3] = raw[i+3], raw[i]
		raw[i+1], raw[i+2] = raw[i+2], raw[i+1]
	}
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("pidbridge: AddrFromSlice failed on %x", raw)
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("pidbridge: parse port %q: %w", portHex, err)
	}
	return netip.AddrPortFrom(addr, uint16(port)), nil
}

// findPIDForInode scans /proc/<pid>/fd for a symlink pointing at
// "socket:[<inode>]". Returns 0 on clean miss.
func (r *ProcResolver) findPIDForInode(inode uint64) (int, error) {
	socketLink := fmt.Sprintf("socket:[%d]", inode)
	entries, err := os.ReadDir(r.procDir)
	if err != nil {
		return 0, fmt.Errorf("pidbridge: read %s: %w", r.procDir, err)
	}
	for _, de := range entries {
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid <= 0 {
			continue
		}
		fdDir := filepath.Join(r.procDir, de.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			// EACCES (different uid) or ESRCH (pid died) — skip.
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if target == socketLink {
				return pid, nil
			}
		}
	}
	return 0, nil
}

// walkAncestors walks pid's PPid chain, looking up each pid in the bridge
// table. The first hit wins. The walk stops at pid 1 (init) or on the
// first /proc read failure.
func (r *ProcResolver) walkAncestors(ctx context.Context, pid int) (string, bool, error) {
	seen := map[int]struct{}{}
	cur := pid
	for cur > 1 {
		if _, ok := seen[cur]; ok {
			break
		}
		seen[cur] = struct{}{}
		e, hit, err := r.store.Lookup(ctx, cur)
		if err != nil {
			return "", false, err
		}
		if hit {
			return e.SessionID, true, nil
		}
		ppid, err := readPPid(filepath.Join(r.procDir, strconv.Itoa(cur), "status"))
		if err != nil {
			return "", false, nil
		}
		cur = ppid
	}
	return "", false, nil
}

// readPPid extracts the PPid field from /proc/<pid>/status.
func readPPid(statusPath string) (int, error) {
	f, err := os.Open(statusPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		return strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("pidbridge: no PPid line")
}
