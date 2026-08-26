//go:build !unix

package dblease

// available is false off unix (e.g. windows): there is no portable
// non-blocking advisory-lock syscall wired here. Mirrors the unix/other
// split in cmd/observer/prune_diskcheck_{unix,other}.go and
// internal/orgserver/policykey_open_{unix,other}.go.
const available = false

// tryAcquireFile always succeeds off unix — there is no cross-process
// coordination on this platform, so every caller proceeds as if it held
// the lease (behavior is unchanged from before dblease existed there).
func tryAcquireFile(_ string) (release func(), acquired bool, err error) {
	return noop, true, nil
}
