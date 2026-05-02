package crossmount

import (
	"runtime"
	"strings"
)

// TranslateForeignPath converts a path expressed in a foreign-OS
// convention to the equivalent path reachable from the running
// process. Today it handles one direction:
//
//   - on a non-Windows host: a Windows-style path like "c:\foo\bar"
//     (or "C:/foo/bar") translates to "/mnt/c/foo/bar".
//
// The intent is to recover correct project attribution for adapters
// that ingest session data captured on a foreign-OS install — e.g. an
// observer running in WSL2 reading codex rollouts written by codex on
// the Windows side. Without translation, filepath.Abs treats "c:\..."
// as a relative path on Linux, prepends the process CWD, and
// findGitRoot then walks UP that bogus path looking for a .git
// directory. In the worst case it lands on observer's own .git and
// every action ends up misattributed to observer's repo.
//
// Returns the input unchanged when no translation applies (already a
// native path, empty, or running on Windows where filepath.Abs
// handles drive letters natively).
//
// The reverse direction (a Linux/macOS path on a Windows host) would
// require knowing which WSL distro the path belongs to, which the
// source data rarely carries. Not implemented today.
func TranslateForeignPath(p string) string {
	if p == "" {
		return p
	}
	if runtime.GOOS == "windows" {
		return p
	}
	if !looksLikeWindowsPath(p) {
		return p
	}
	return windowsToWSLMnt(p)
}

// looksLikeWindowsPath reports whether p starts with a single drive
// letter, a colon, and a slash — the unambiguous prefix of a Windows
// absolute path. Both forward and backslash separators are accepted
// because some serialisation paths normalise to forward slashes.
func looksLikeWindowsPath(p string) bool {
	if len(p) < 3 {
		return false
	}
	c := p[0]
	if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
		return false
	}
	if p[1] != ':' {
		return false
	}
	return p[2] == '\\' || p[2] == '/'
}

// windowsToWSLMnt rewrites "C:\Users\foo" to "/mnt/c/Users/foo". The
// drive letter is lowercased to match the /mnt/<lower> convention WSL2
// uses; the rest of the path keeps its case. Backslashes are converted
// to forward slashes.
func windowsToWSLMnt(p string) string {
	drive := strings.ToLower(string(p[0]))
	rest := strings.ReplaceAll(p[3:], "\\", "/")
	return "/mnt/" + drive + "/" + rest
}
