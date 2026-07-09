package bridge

import (
	"os"
	"path/filepath"
	"strings"
)

// isWSL reports whether this process runs inside WSL2 with Windows interop —
// the only environment where the bridge can exec a Windows .exe. Same probe
// the antigravity/launch/oscrypt paths use; kept local so this package depends
// on neither (a 3-line OS check, not shared infra worth a new package).
func isWSL() bool {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		return true
	}
	if b, err := os.ReadFile("/proc/version"); err == nil {
		return strings.Contains(strings.ToLower(string(b)), "microsoft")
	}
	return false
}

// ResolveWindowsObserver finds the Windows observer.exe the bridge execs over
// interop (spec §5.5). Resolution order:
//
//  1. explicit — the [observer.process].windows_binary_path config, when set;
//  2. $OBSERVER_WINDOWS_BINARY;
//  3. an observer.exe beside the daemon's own binary (a /mnt dev checkout
//     builds bin/observer.exe next to bin/observer);
//  4. <cwd>/bin/observer.exe.
//
// A drive-letter path (C:\…) in the explicit/env slots is translated to its
// /mnt form. Returns the first existing regular file, or ("", false). WSL
// interop can only launch Windows-reachable paths, so /home (WSL-native)
// candidates are skipped; an explicit path is trusted if it exists.
func ResolveWindowsObserver(explicit string) (string, bool) {
	candidates := []string{
		windowsToWSLPath(strings.TrimSpace(explicit)),
		windowsToWSLPath(strings.TrimSpace(os.Getenv("OBSERVER_WINDOWS_BINARY"))),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "observer.exe"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "bin", "observer.exe"))
	}

	for i, c := range candidates {
		if c == "" {
			continue
		}
		// Auto candidates (index >= 2) must be /mnt-reachable; an explicit/env
		// path is trusted as given (the operator knows their layout).
		if i >= 2 && !strings.HasPrefix(c, "/mnt/") {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, true
		}
	}
	return "", false
}

// AvailableInWSL reports whether the cross-OS bridge can run here: this process
// is inside WSL interop AND a Windows observer.exe resolves. selectProcessBackend
// uses it so "auto" prefers the bridge on the canonical WSL topology and falls
// back to the local poll backend otherwise.
func AvailableInWSL(explicitPath string) bool {
	if !isWSL() {
		return false
	}
	_, ok := ResolveWindowsObserver(explicitPath)
	return ok
}

// windowsToWSLPath converts a drive-letter Windows path (C:\foo\bar or C:/foo)
// to its WSL mount form (/mnt/c/foo/bar). Non-drive-letter paths (already a
// /mnt or /home path) are returned unchanged.
func windowsToWSLPath(p string) string {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		drive := strings.ToLower(p[:1])
		rest := strings.ReplaceAll(p[2:], `\`, "/")
		return "/mnt/" + drive + rest
	}
	return p
}
