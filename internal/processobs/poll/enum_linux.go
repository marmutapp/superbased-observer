//go:build linux

package poll

import (
	"os"
	"strconv"
	"strings"
)

// userHZ is the Linux ABI clock-tick rate (USER_HZ): /proc/<pid>/stat reports
// utime/stime in 1/USER_HZ-second ticks. It is fixed at 100 across all real
// Linux targets regardless of the kernel's CONFIG_HZ — it is the userspace ABI
// constant sysconf(_SC_CLK_TCK) returns — so we use it directly rather than
// taking a cgo/sysconf dependency (matching prometheus/procfs).
const userHZ = 100

// platformEnumerate reads the current process table from /proc. Processes
// that vanish mid-scan, kernel threads with no readable exe, and entries we
// lack permission to inspect are skipped or filled best-effort — never fatal.
func platformEnumerate() ([]ProcInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]ProcInfo, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a /proc/<pid> entry
		}
		p, ok := readProc(pid)
		if !ok {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// platformSessionToken is a no-op on Linux: the §5.5 P-B6 env-token (EV) path
// is the cross-OS-bridge high-confidence mechanism (the Windows capturer reads
// the env). Native Linux processes are attributed by the direct pidbridge seed
// (§9.2.1), so reading the env token here would be redundant.
func platformSessionToken(int) string { return "" }

// readProc assembles a ProcInfo for one pid. Returns ok=false only when the
// process is gone or its stat is unreadable (no ppid/start → unusable).
func readProc(pid int) (ProcInfo, bool) {
	base := "/proc/" + strconv.Itoa(pid)

	statRaw, err := os.ReadFile(base + "/stat")
	if err != nil {
		return ProcInfo{}, false
	}
	sf, ok := parseStat(string(statRaw))
	if !ok {
		return ProcInfo{}, false
	}

	p := ProcInfo{
		PID:        pid,
		PPID:       sf.ppid,
		StartTicks: sf.start,
		HasStart:   true,
		// Resource metrics from /proc — native-Linux parity with the Windows poll
		// capturer (procmetrics_windows.go). CPU + thread count come from stat
		// (always present once stat parsed); memory from status; disk from io.
		// HasMetrics distinguishes a genuine zero from "never read", exactly as on
		// Windows.
		HasMetrics:  true,
		CPUUserMs:   sf.cpuUserMs,
		CPUSystemMs: sf.cpuSystemMs,
		ThreadCount: sf.threads,
	}
	if exe, err := os.Readlink(base + "/exe"); err == nil {
		p.ExePath = exe
	}
	if cmdRaw, err := os.ReadFile(base + "/cmdline"); err == nil {
		p.Argv = parseCmdline(cmdRaw)
	}
	if cwd, err := os.Readlink(base + "/cwd"); err == nil {
		p.CWD = cwd // best-effort; requires ptrace-level access for other users
	}
	if statusRaw, err := os.ReadFile(base + "/status"); err == nil {
		status := string(statusRaw)
		p.UID, p.EUID = parseIDLine(status, "Uid:")
		p.GID, p.EGID = parseIDLine(status, "Gid:")
		p.SeccompMode = seccompLabel(statusField(status, "Seccomp:"))
		p.CapabilitiesEff = statusField(status, "CapEff:")
		// Memory: VmRSS = current resident set (≈ Windows working set), VmHWM =
		// peak resident (≈ PeakWorkingSetSize). Reported in kB; absent for kernel
		// threads (→ 0).
		p.WorkingSetBytes = procIntField(status, "VmRSS:") * 1024
		p.MaxRSSBytes = procIntField(status, "VmHWM:") * 1024
	}
	readProcIO(base, &p)
	readPosture(base, &p)
	return p, true
}

// readProcIO fills p's disk counters from /proc/<pid>/io: read_bytes/write_bytes
// are the bytes that actually hit the storage layer (the honest "disk" number —
// a page-cache-served read shows 0, unlike rchar/wchar), and syscr/syscw are the
// read/write syscall counts. Cumulative since process start, like the Windows
// IO_COUNTERS. Best-effort: the file needs same-uid or CAP_SYS_PTRACE, so
// another user's process simply leaves these zero (HasMetrics already true from
// stat).
func readProcIO(base string, p *ProcInfo) {
	raw, err := os.ReadFile(base + "/io")
	if err != nil {
		return
	}
	io := string(raw)
	p.ReadBytes = procIntField(io, "read_bytes:")
	p.WriteBytes = procIntField(io, "write_bytes:")
	p.ReadOps = procIntField(io, "syscr:")
	p.WriteOps = procIntField(io, "syscw:")
}

// procIntField parses the leading integer value of a /proc field line
// ("VmRSS:\t 1234 kB" → 1234, "read_bytes: 4096" → 4096), ignoring any trailing
// unit. Returns 0 when the line is absent or unparseable.
func procIntField(s, prefix string) int64 {
	f := strings.Fields(statusField(s, prefix))
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	return n
}

// readPosture fills the §8 security + isolation fields from /proc (P4). All
// best-effort: a process we lack permission to inspect, or a kernel without
// a given LSM/namespace, simply leaves the field empty.
func readPosture(base string, p *ProcInfo) {
	if attr, err := os.ReadFile(base + "/attr/current"); err == nil {
		p.AppArmorLabel, p.SELinuxLabel = classifyLSMLabel(string(attr))
	}
	if cg, err := os.ReadFile(base + "/cgroup"); err == nil {
		p.CgroupPath = parseCgroupPath(string(cg))
		p.ContainerID = containerIDFromCgroup(p.CgroupPath)
	}
	p.PIDNamespace = nsInode(base + "/ns/pid")
	p.MountNamespace = nsInode(base + "/ns/mnt")
	p.NetNamespace = nsInode(base + "/ns/net")
}

// statusField returns the trimmed value after a /proc field-line prefix — used
// for /proc/<pid>/status ("CapEff:\t0000003fffffffff") and, via procIntField,
// /proc/<pid>/io ("read_bytes: 4096"). Returns "" when the prefix is absent.
func statusField(status, prefix string) string {
	for _, line := range strings.Split(status, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}

// seccompLabel maps the numeric Seccomp: mode to a compact label.
func seccompLabel(v string) string {
	switch v {
	case "0":
		return "disabled"
	case "1":
		return "strict"
	case "2":
		return "filter"
	default:
		return ""
	}
}

// classifyLSMLabel routes a /proc/<pid>/attr/current value to AppArmor or
// SELinux. SELinux contexts look like "user:role:type:level" (≥3 colons, no
// spaces/parens); everything else (incl. "unconfined" and "<profile>
// (enforce)") is AppArmor.
func classifyLSMLabel(raw string) (apparmor, selinux string) {
	s := strings.TrimSpace(strings.TrimRight(raw, "\x00\n"))
	if s == "" {
		return "", ""
	}
	if strings.Count(s, ":") >= 3 && !strings.ContainsAny(s, " ()") {
		return "", s
	}
	return s, ""
}

// parseCgroupPath returns the controller path from /proc/<pid>/cgroup. For
// cgroup v2 the single line is "0::/path"; for v1 it takes the last entry's
// path. Empty when unreadable.
func parseCgroupPath(cgroup string) string {
	var path string
	for _, line := range strings.Split(strings.TrimSpace(cgroup), "\n") {
		if i := strings.LastIndexByte(line, ':'); i >= 0 && i+1 < len(line) {
			path = line[i+1:]
		}
	}
	return path
}

// containerIDFromCgroup extracts a short container id (first 12 chars of a
// 64-hex run) from a cgroup path — covers docker/containerd/cri/kubepods
// shapes. Empty when the path has no container-id-shaped token.
func containerIDFromCgroup(path string) string {
	run := 0
	start := -1
	for i := 0; i <= len(path); i++ {
		if i < len(path) && isHexByte(path[i]) {
			if run == 0 {
				start = i
			}
			run++
			continue
		}
		if run >= 64 {
			return path[start : start+12]
		}
		run = 0
	}
	return ""
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// nsInode reads a /proc/<pid>/ns/<kind> symlink ("net:[4026531992]") and
// returns the inode number ("4026531992"). Empty when unreadable.
func nsInode(path string) string {
	link, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	open := strings.IndexByte(link, '[')
	closeB := strings.IndexByte(link, ']')
	if open >= 0 && closeB > open {
		return link[open+1 : closeB]
	}
	return ""
}

// statFields are the /proc/<pid>/stat values we use: ppid (field 4), the
// cumulative CPU times utime/stime (fields 14/15, converted from USER_HZ ticks
// to ms), num_threads (field 20), and starttime (field 22 — the ProcessKey
// stamp). ppid + start are load-bearing; CPU + threads are best-effort extras.
type statFields struct {
	ppid        int
	cpuUserMs   int64
	cpuSystemMs int64
	threads     int32
	start       int64
}

// parseStat extracts statFields from /proc/<pid>/stat. The comm field is
// parenthesized and may contain spaces or ')', so we split on the LAST ')' and
// index the remainder (field 3 = state is index 0 there).
func parseStat(s string) (statFields, bool) {
	closeIdx := strings.LastIndexByte(s, ')')
	if closeIdx < 0 || closeIdx+2 >= len(s) {
		return statFields{}, false
	}
	fields := strings.Fields(s[closeIdx+2:])
	// After comm, fields are: state(0) ppid(1) ... utime(11) stime(12) ...
	// num_threads(17) ... starttime(19).
	if len(fields) < 20 {
		return statFields{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return statFields{}, false
	}
	start, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return statFields{}, false
	}
	sf := statFields{ppid: ppid, start: start}
	// CPU + threads must not fail the whole stat read on a parse hiccup — they
	// stay zero (HasMetrics still reports "read"). utime/stime are USER_HZ ticks.
	if ut, err := strconv.ParseInt(fields[11], 10, 64); err == nil {
		sf.cpuUserMs = ut * 1000 / userHZ
	}
	if st, err := strconv.ParseInt(fields[12], 10, 64); err == nil {
		sf.cpuSystemMs = st * 1000 / userHZ
	}
	if nt, err := strconv.Atoi(fields[17]); err == nil {
		sf.threads = int32(nt)
	}
	return sf, true
}

// parseCmdline splits the NUL-separated /proc/<pid>/cmdline into argv,
// dropping the trailing empty element. Empty for kernel threads.
func parseCmdline(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	parts := strings.Split(string(raw), "\x00")
	for len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// parseIDLine reads a "Uid:\treal\teff\tsaved\tfs" (or Gid:) line from
// /proc/<pid>/status, returning (real, effective). Missing → (0, 0).
func parseIDLine(status, prefix string) (real, eff int) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		f := strings.Fields(line[len(prefix):])
		if len(f) >= 2 {
			real, _ = strconv.Atoi(f[0])
			eff, _ = strconv.Atoi(f[1])
		}
		return real, eff
	}
	return 0, 0
}
