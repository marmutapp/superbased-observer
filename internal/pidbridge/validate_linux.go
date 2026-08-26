//go:build linux

package pidbridge

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// platformCanValidate reports that this build's platform can answer
// liveness + identity questions about local processes (/proc is
// introspectable), so the resolver may gate bridge hits on
// [ValidateLocalProcess].
const platformCanValidate = true

// ValidateLocalProcess reports whether pid names a live process on THIS
// host whose identity matches execHint — the guard the watcher/SQLite
// attribution path applies before seeding session_pid_bridge from an
// adapter-discovered pid (cline-cli's sessions.pid, qwen-code's
// runtime.json). A recycled or foreign-host pid must never
// false-attribute (a miss beats a wrong link), so both checks must
// pass:
//
//   - Liveness: /proc/<pid>/comm is readable (the process exists).
//   - Identity: execHint (lowercased) is a substring of the process's
//     comm OR any /proc/<pid>/cmdline argument. Empty execHint skips
//     the identity check (liveness-only) — callers that know the
//     binary name should always pass one.
//
// On non-Linux builds this always returns false (see
// validate_other.go): the process table isn't introspected, so the
// safe answer is "can't confirm → don't seed."
func ValidateLocalProcess(pid int, execHint string) bool {
	return validateProcess("/proc", pid, execHint)
}

// validateProcess is the testable core of ValidateLocalProcess; procDir
// is injected so tests can point it at a synthetic /proc tree.
func validateProcess(procDir string, pid int, execHint string) bool {
	if pid <= 1 {
		return false
	}
	commBytes, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "comm"))
	if err != nil {
		// Process is gone (or unreadable) — treat as a miss.
		return false
	}
	hint := strings.ToLower(strings.TrimSpace(execHint))
	if hint == "" {
		// Liveness-only: comm was readable, so the pid is alive.
		return true
	}
	comm := strings.ToLower(strings.TrimSpace(string(commBytes)))
	if strings.Contains(comm, hint) {
		return true
	}
	// comm is capped at 15 chars by the kernel and Node/Python CLIs
	// report their interpreter name there, so fall through to the full
	// argv — the tool's own path/name almost always appears in cmdline.
	cmdline, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	args := strings.ToLower(strings.ReplaceAll(string(cmdline), "\x00", " "))
	return strings.Contains(args, hint)
}
