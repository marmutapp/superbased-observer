//go:build !linux

package pidbridge

// ValidateLocalProcess always reports false on non-Linux builds: without
// /proc the owning process's liveness + identity can't be confirmed, so
// the safe answer is "don't seed." Watcher/SQLite-path attribution
// (cline-cli, qwen-code) therefore degrades to the indirect cwd-anchor
// capture on non-Linux daemons — the same posture the hook ancestor-walk
// already has there.
func ValidateLocalProcess(_ int, _ string) bool {
	return false
}
