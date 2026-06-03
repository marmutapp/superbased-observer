package antigravity

import (
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// LanguageServer represents a running language_server process —
// either a real one this observer can talk to, or a discovered
// stub waiting for connection details.
type LanguageServer struct {
	PID         int
	HTTPPort    int    // best-guess HTTP/2 plaintext port (heuristic)
	HTTPSPort   int    // best-guess HTTP/2 TLS port (heuristic)
	Ports       []int  // all owned listening ports, sorted ascending
	CSRFToken   string // value of --csrf_token cmdline arg
	WorkspaceID string // value of --workspace_id (may be empty for non-LSP server)
	Endpoint    string // built once via PreferredEndpoint(); cached
}

// PreferredEndpoint returns the URL prefix for the gRPC call:
// `http://127.0.0.1:<httpPort>` if HTTPPort is set, else
// `https://127.0.0.1:<httpsPort>`. Returns empty string if neither
// is known. Used as the first candidate in Endpoints() but not the
// only one — the heuristic that populates HTTPPort vs HTTPSPort is
// unreliable across language_server versions, so callers should
// iterate Endpoints() until one works.
func (ls *LanguageServer) PreferredEndpoint() string {
	if ls.HTTPPort > 0 {
		return "http://127.0.0.1:" + strconv.Itoa(ls.HTTPPort)
	}
	if ls.HTTPSPort > 0 {
		return "https://127.0.0.1:" + strconv.Itoa(ls.HTTPSPort)
	}
	return ""
}

// Endpoints returns every plausible URL the language_server might
// answer gRPC on, ordered by likelihood of success. Antigravity's
// port-protocol assignment is unstable across versions — one server
// may expose lower-port=HTTPS+higher-port=HTTP, another expose them
// flipped — so callers iterate this list until one works rather than
// committing to the heuristic up-front.
//
// Order:
//  1. PreferredEndpoint() (heuristic best-guess) for backwards-compat
//  2. http://port for every owned port (ascending)
//  3. https://port for every owned port (ascending)
//
// Duplicates from (1) into (2)/(3) are deduped. Empty when neither
// Ports nor HTTPPort/HTTPSPort are populated.
func (ls *LanguageServer) Endpoints() []string {
	seen := map[string]bool{}
	var out []string
	addIf := func(u string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	addIf(ls.PreferredEndpoint())
	for _, p := range ls.Ports {
		addIf("http://127.0.0.1:" + strconv.Itoa(p))
	}
	for _, p := range ls.Ports {
		addIf("https://127.0.0.1:" + strconv.Itoa(p))
	}
	if ls.HTTPPort > 0 {
		addIf("http://127.0.0.1:" + strconv.Itoa(ls.HTTPPort))
	}
	if ls.HTTPSPort > 0 {
		addIf("https://127.0.0.1:" + strconv.Itoa(ls.HTTPSPort))
	}
	return out
}

// resolveWorkspaceIDToPath inverts the lossy "file_" + replace-non-
// alphanumerics-with-underscore encoding used by the antigravity
// language_server's --workspace_id flag. Walks each candidate root
// dir up to maxDepth levels, encoding every visited directory's
// absolute path back to the workspace_id form via workspaceIDFromURI,
// and returns the first match.
//
// The encoding is one-way (path → wsID is deterministic, wsID → path
// is ambiguous because '/', '-', '.', and '_' all map to '_'), so
// reverse-resolution by enumeration is the only reliable approach.
// For typical workspace layouts (workspace 2-3 levels under home root)
// the BFS is bounded by maybe a few hundred directories — cheap.
//
// Returns "" when no match is found within depth limit. Caller should
// cache hits AND misses so repeated lookups for the same wsID don't
// re-walk the FS (the adapter does this via wsResolveCache).
//
// Used by recoverViaLocalGRPC for the in-progress-conversation case:
// the language_server has the workspace_id in its --workspace_id
// flag, but if the conversation hasn't been written into
// trajectorySummaries yet (Antigravity flushes the index on session
// save, not real-time), idxEntry is nil and we have no other source
// of truth for the workspace path.
func resolveWorkspaceIDToPath(wsID string, roots []string, maxDepth int) string {
	if !strings.HasPrefix(wsID, "file_") || maxDepth <= 0 {
		return ""
	}
	type entry struct {
		path  string
		depth int
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		queue := []entry{{path: root, depth: 0}}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if workspaceIDFromURI("file://"+cur.path) == wsID {
				return cur.path
			}
			if cur.depth >= maxDepth {
				continue
			}
			entries, err := os.ReadDir(cur.path)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				// Skip dotfiles and noisy roots that won't host a
				// user workspace (node_modules, .git, vendor, etc.).
				if strings.HasPrefix(name, ".") {
					continue
				}
				if name == "node_modules" || name == "vendor" || name == "target" {
					continue
				}
				queue = append(queue, entry{
					path:  filepath.Join(cur.path, name),
					depth: cur.depth + 1,
				})
			}
		}
	}
	return ""
}

// workspaceIDFromURI converts a file:// (or vscode-remote://) URI to
// the workspace_id format the antigravity language_server uses for
// its --workspace_id cmdline flag. Encoding: "file_" + path-with-
// non-alphanumerics-replaced-by-underscore (leading slashes stripped
// first).
//
// Example: "file:///home/marmutapp/superbased-observer"
//
//	→ stripped scheme "/home/marmutapp/superbased-observer"
//	→ trimmed leading "home/marmutapp/superbased-observer"
//	→ encoded "file_home_marmutapp_superbased_observer"
//
// Verified against running language_server cmdlines on a real
// Antigravity-on-WSL host (2026-05-10): pid 694 had cmdline arg
// `--workspace_id file_home_marmutapp_superbased_observer` for the
// workspace at `/home/marmutapp/superbased-observer/`.
//
// Used by the recovery path to match a conversation's index entry
// (workspaceURI in state.vscdb) to the language_server hosting that
// workspace, so observer doesn't talk to a non-matching server that
// would only return empty stub responses.
func workspaceIDFromURI(uri string) string {
	if uri == "" {
		return ""
	}
	p := strings.TrimPrefix(uri, "file://")
	p = strings.TrimPrefix(p, "vscode-remote://")
	if decoded, err := url.PathUnescape(p); err == nil {
		p = decoded
	}
	p = strings.TrimLeft(p, "/")
	var sb strings.Builder
	sb.WriteString("file_")
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// isAntigravityServerExe reports whether basename (e.g. "agy",
// "language_server_linux_x64", "agy.exe") looks like an
// Antigravity-family gRPC server. Desktop Antigravity spawns the
// language_server as a separate process; the agy CLI hosts the
// equivalent embedded inside its own binary. Discovery routines need
// to consider both shapes — without this, CLI sessions are invisible
// to network recovery even when the CLI is running. (Class-A audit
// finding, 2026-05-23.)
func isAntigravityServerExe(basename string) bool {
	lower := strings.ToLower(basename)
	lower = strings.TrimSuffix(lower, ".exe")
	if lower == "" {
		return false
	}
	if strings.HasPrefix(lower, "language_server") {
		return true
	}
	return lower == "agy" || lower == "antigravity"
}

// requiresCSRF reports whether discovery should skip a process when
// its cmdline lacks --csrf_token. Desktop language_server processes
// always pass --csrf_token (verified across 2024-2026 builds); the
// agy CLI's embedded server uses an OAuth-token-file mechanism on
// localhost instead and may not advertise --csrf_token at all.
// Accepting servers without CSRF lets the gRPC call attempt — the
// server itself rejects unauthorised requests cheaply.
func requiresCSRF(basename string) bool {
	lower := strings.ToLower(basename)
	lower = strings.TrimSuffix(lower, ".exe")
	return strings.HasPrefix(lower, "language_server")
}

// discoverLanguageServers enumerates running Antigravity-family gRPC
// servers — both standalone language_server_*.exe processes (desktop
// Antigravity) and embedded agy.exe / agy CLI servers — and returns
// the (csrf_token, listening port) pairs needed to reach them.
//
// Implementation switches on host OS:
//   - Linux/WSL2: shells out to powershell.exe to query Windows processes
//     (the language_server runs Windows-side under Antigravity).
//   - Windows: native tasklist / WMIC.
//   - macOS: ps + lsof.
//
// Returns an empty slice (no error) when no language servers are
// running — the network-recovery path simply skips in that case.
func discoverLanguageServers() ([]LanguageServer, error) {
	switch runtime.GOOS {
	case "linux":
		if isWSL() {
			return discoverViaPowerShell()
		}
		return discoverNativeLinux()
	case "darwin":
		return discoverNativeMac()
	case "windows":
		return discoverNativeWindows()
	default:
		return nil, nil
	}
}

// isWSL reports whether the running process is inside WSL2 with
// Windows interop. Mirrors oscrypt's detection.
func isWSL() bool {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		return true
	}
	body, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(body)), "microsoft")
}

// discoverViaPowerShell shells out to powershell.exe to query
// Win32_Process + Get-NetTCPConnection. Returns the (PID, csrf,
// httpPort, httpsPort, workspace_id) tuple per running process.
//
// PowerShell script returns one line per language_server, fields
// tab-separated:
//
//	<pid>\t<httpPort>\t<httpsPort>\t<csrfToken>\t<workspaceId>
func discoverViaPowerShell() ([]LanguageServer, error) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return nil, errors.New("antigravity: powershell.exe not on PATH (corporate-locked WSL?)")
	}
	// Two process shapes are considered:
	//   - language_server_*.exe  → desktop Antigravity (always carries --csrf_token)
	//   - agy.exe / antigravity.exe → CLI embedded server (may NOT
	//     carry --csrf_token; we still emit a row so the gRPC call can
	//     attempt against the listening ports).
	script := `
$ErrorActionPreference = "SilentlyContinue"
$lsProcesses = Get-WmiObject Win32_Process | Where-Object {
    $_.Name -like 'language_server_*.exe' -or
    $_.Name -eq 'agy.exe' -or
    $_.Name -eq 'antigravity.exe'
}
foreach ($p in $lsProcesses) {
    $cmd = $p.CommandLine
    $name = $p.Name
    $csrf = ''
    $workspace = ''
    if ($cmd -match '--csrf_token\s+(\S+)')   { $csrf = $matches[1] }
    if ($cmd -match '--workspace_id\s+(\S+)') { $workspace = $matches[1] }
    if (-not $csrf -and $name -like 'language_server_*.exe') { continue }
    $ports = Get-NetTCPConnection -State Listen -LocalAddress 127.0.0.1 -OwningProcess $p.ProcessId | Sort-Object LocalPort | Select-Object -ExpandProperty LocalPort
    if ($ports.Count -lt 2) { continue }
    # Lowest port is HTTPS; next is HTTP (per server.go log line ordering).
    $httpsPort = $ports[0]
    $httpPort = $ports[1]
    "$($p.ProcessId)` + "`t" + `$httpPort` + "`t" + `$httpsPort` + "`t" + `$csrf` + "`t" + `$workspace"
}
`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return nil, err
	}
	return parsePSDiscoveryOutput(string(out))
}

// parsePSDiscoveryOutput parses the tab-separated line format produced
// by the PowerShell discovery script. Tolerant of CRLF + blank lines.
func parsePSDiscoveryOutput(out string) ([]LanguageServer, error) {
	var servers []LanguageServer
	out = strings.ReplaceAll(out, "\r", "")
	for _, line := range strings.Split(out, "\n") {
		// Trim ONLY spaces — never tabs — because the trailing
		// CSRF field is empty for agy.exe / antigravity.exe rows
		// ("pid\thttpPort\thttpsPort\t\t" with CSRF == "" and
		// optional workspace == ""). A strings.TrimSpace here
		// strips the trailing tabs and collapses the row, getting
		// dropped by the len(parts) < 4 guard below.
		line = strings.Trim(line, " ")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hp, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		hsp, err3 := strconv.Atoi(strings.TrimSpace(parts[2]))
		csrf := strings.TrimSpace(parts[3])
		workspace := ""
		if len(parts) >= 5 {
			workspace = strings.TrimSpace(parts[4])
		}
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		// CSRF may legitimately be empty for agy.exe / antigravity.exe
		// CLI rows — the PowerShell script already filters
		// language_server_*.exe rows lacking CSRF before they reach
		// here, so an empty CSRF at this point means a CLI embedded
		// server.
		// Populate Ports too so Endpoints() iteration works for the
		// PowerShell-discovered Windows-side servers as well, not just
		// /proc-discovered Linux-native ones.
		ports := []int{}
		if hsp > 0 {
			ports = append(ports, hsp)
		}
		if hp > 0 && hp != hsp {
			ports = append(ports, hp)
		}
		sortInts(ports)
		servers = append(servers, LanguageServer{
			PID:         pid,
			HTTPPort:    hp,
			HTTPSPort:   hsp,
			Ports:       ports,
			CSRFToken:   csrf,
			WorkspaceID: workspace,
		})
	}
	return servers, nil
}

// discoverNativeLinux queries /proc for language_server processes
// AND enumerates their listening TCP ports via /proc/<pid>/net/tcp
// (which is the per-namespace view the process itself sees). On
// Antigravity's "remote workspace" feature this surfaces a
// Linux-native language_server_linux_x64 that observer-on-WSL can
// reach directly via 127.0.0.1 — no Windows bridge needed.
func discoverNativeLinux() ([]LanguageServer, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}
	var servers []LanguageServer
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		exe, _ := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		base := filepath.Base(exe)
		if !isAntigravityServerExe(base) {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(cmdline), "\x00")
		csrf, workspace := parseCmdlineFlags(args)
		if csrf == "" && requiresCSRF(base) {
			continue
		}
		ports := listeningPortsForPID(pid)
		ls := LanguageServer{
			PID:         pid,
			CSRFToken:   csrf,
			WorkspaceID: workspace,
			Ports:       ports,
		}
		// Heuristic: lower port = HTTPS, next = HTTP. Unreliable
		// across language_server versions (verified 2026-05-10: one
		// running pid had ports[0]=HTTP, ports[1]=TLS, breaking this
		// assumption). Kept as a best-guess for PreferredEndpoint();
		// callers should iterate Endpoints() rather than trusting the
		// heuristic.
		if len(ports) >= 2 {
			ls.HTTPSPort = ports[0]
			ls.HTTPPort = ports[1]
		} else if len(ports) == 1 {
			ls.HTTPSPort = ports[0]
		}
		servers = append(servers, ls)
	}
	return servers, nil
}

// listeningPortsForPID returns the loopback TCP ports the given PID
// is listening on, sorted ascending. Reads /proc/<pid>/net/tcp
// (filters state==0A==LISTEN) and matches the socket inode against
// /proc/<pid>/fd/* symlinks. Best-effort; returns nil on any error.
func listeningPortsForPID(pid int) []int {
	// Build the set of socket inodes the process owns.
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		return nil
	}
	owned := map[string]bool{}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if err != nil {
			continue
		}
		// Format: "socket:[<inode>]"
		if !strings.HasPrefix(target, "socket:[") {
			continue
		}
		inode := strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")
		owned[inode] = true
	}
	if len(owned) == 0 {
		return nil
	}
	// Walk /proc/net/tcp and /proc/net/tcp6 looking for LISTEN-state
	// rows whose inode is in `owned` and local addr is loopback.
	var ports []int
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		body, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		ports = append(ports, parseTCPListeners(string(body), owned)...)
	}
	// Dedup + sort.
	seen := map[int]bool{}
	var out []int
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sortInts(out)
	return out
}

// parseTCPListeners parses /proc/net/tcp{,6} content for rows in
// LISTEN state on loopback whose inode matches `owned`. Returns
// the local ports.
//
// /proc/net/tcp row format (whitespace-separated):
//
//	sl  local_addr:local_port rem_addr:rem_port st ... inode
//
// where addr is hex (big-endian-but-byte-swapped on x86) and st=0A
// means LISTEN.
func parseTCPListeners(body string, owned map[string]bool) []int {
	var out []int
	for i, line := range strings.Split(body, "\n") {
		if i == 0 {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		// fields[1] is local_addr:port (hex)
		// fields[3] is state (0A = LISTEN)
		// fields[9] is inode
		if fields[3] != "0A" {
			continue
		}
		inode := fields[9]
		if !owned[inode] {
			continue
		}
		// Parse port from local_addr:port. Local addr is hex.
		colon := strings.LastIndex(fields[1], ":")
		if colon < 0 {
			continue
		}
		addrHex := fields[1][:colon]
		portHex := fields[1][colon+1:]
		// Loopback check: ipv4 addr is 0100007F (127.0.0.1 byte-swapped),
		// ipv6 includes 00000000000000000000000001000000 etc. Accept any
		// loopback.
		if !isLoopbackHexAddr(addrHex) {
			continue
		}
		port, err := strconv.ParseInt(portHex, 16, 32)
		if err != nil {
			continue
		}
		out = append(out, int(port))
	}
	return out
}

// isLoopbackHexAddr reports whether a hex-encoded /proc/net/tcp
// local_addr is on the loopback interface. Handles ipv4 (8 hex
// chars, byte-swapped) and ipv6 (32 hex chars, ::1 or
// ::ffff:127.0.0.1 forms).
func isLoopbackHexAddr(h string) bool {
	switch len(h) {
	case 8: // ipv4
		// Byte-swapped: "0100007F" = 127.0.0.1
		return strings.EqualFold(h, "0100007F")
	case 32: // ipv6
		lower := strings.ToLower(h)
		// ::1 = 32 zeros except last "01000000" group? Actually:
		// 16-byte addr written as 4 little-endian uint32s.
		// ::1 → 00000000 00000000 00000000 01000000
		// ::ffff:127.0.0.1 → 00000000 00000000 0000FFFF 0100007F (byte-swapped)
		return lower == "00000000000000000000000001000000" ||
			strings.HasSuffix(lower, "0100007f") && strings.Contains(lower, "0000ffff")
	}
	return false
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// discoverNativeMac queries `ps` + `lsof` to find language_server
// processes and their listening ports on macOS.
func discoverNativeMac() ([]LanguageServer, error) {
	out, err := exec.Command("ps", "-axwwo", "pid=,command=").Output()
	if err != nil {
		return nil, err
	}
	var servers []LanguageServer
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Match language_server_darwin_x64 / arm64, plus the agy CLI
		// shape (basename "agy" or "antigravity") that hosts an
		// embedded server.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		exeBase := filepath.Base(fields[1])
		if !isAntigravityServerExe(exeBase) {
			continue
		}
		csrf, workspace := parseCmdlineFlags(fields)
		if csrf == "" && requiresCSRF(exeBase) {
			continue
		}
		// Use lsof to find listening TCP ports for this PID.
		lsofOut, err := exec.Command("lsof", "-iTCP", "-sTCP:LISTEN", "-a", "-p", strconv.Itoa(pid), "-Pn").Output() //nolint:gosec // G204: fixed 'lsof' binary; the only variable argument is an integer PID via strconv.Itoa.
		if err != nil {
			servers = append(servers, LanguageServer{PID: pid, CSRFToken: csrf, WorkspaceID: workspace})
			continue
		}
		var ports []int
		for _, l := range strings.Split(string(lsofOut), "\n") {
			if !strings.Contains(l, "LISTEN") {
				continue
			}
			// e.g. "language_  1234 user   12u  IPv4 0x... 0t0 TCP 127.0.0.1:55860 (LISTEN)"
			idx := strings.LastIndex(l, ":")
			if idx < 0 {
				continue
			}
			tail := l[idx+1:]
			tail = strings.TrimSuffix(strings.Fields(tail)[0], "")
			port, err := strconv.Atoi(tail)
			if err == nil {
				ports = append(ports, port)
			}
		}
		sortInts(ports)
		ls := LanguageServer{PID: pid, CSRFToken: csrf, WorkspaceID: workspace, Ports: ports}
		if len(ports) >= 1 {
			ls.HTTPSPort = ports[0]
		}
		if len(ports) >= 2 {
			ls.HTTPPort = ports[1]
		}
		servers = append(servers, ls)
	}
	return servers, nil
}

// discoverNativeWindows uses tasklist + Get-NetTCPConnection (via
// powershell). Reuses the same script as discoverViaPowerShell.
func discoverNativeWindows() ([]LanguageServer, error) {
	return discoverViaPowerShell()
}

// parseCmdlineFlags extracts --csrf_token and --workspace_id from
// a command-line argv slice (handles both "flag value" and
// "flag=value" forms).
func parseCmdlineFlags(args []string) (csrf, workspace string) {
	for i, a := range args {
		switch {
		case a == "--csrf_token" && i+1 < len(args):
			csrf = args[i+1]
		case strings.HasPrefix(a, "--csrf_token="):
			csrf = strings.TrimPrefix(a, "--csrf_token=")
		case a == "--workspace_id" && i+1 < len(args):
			workspace = args[i+1]
		case strings.HasPrefix(a, "--workspace_id="):
			workspace = strings.TrimPrefix(a, "--workspace_id=")
		}
	}
	return csrf, workspace
}
