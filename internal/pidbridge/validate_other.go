//go:build !linux

package pidbridge

// platformCanValidate reports that this platform cannot introspect its
// process table, so [ValidateLocalProcess] can never confirm a hit.
// Consumers gate on this const and trust bridge rows unchanged here:
// without the gate, requiring validation would silently disable pid
// resolution on macOS/Windows entirely instead of merely forgoing the
// staleness guard.
const platformCanValidate = false

// ValidateLocalProcess always reports false on non-Linux builds: without
// /proc the owning process's liveness + identity can't be confirmed, so
// the safe answer is "don't seed." Watcher/SQLite-path attribution
// (cline-cli, qwen-code) therefore degrades to the indirect cwd-anchor
// capture on non-Linux daemons — the same posture the hook ancestor-walk
// already has there.
func ValidateLocalProcess(_ int, _ string) bool {
	return false
}
