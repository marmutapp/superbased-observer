//go:build !windows

package main

import (
	"os"
	"syscall"
)

// fileOwnedByCurrentUser reports whether the file described by info is owned by
// the current process's uid (finding N6). On a shared, non-sticky TMPDIR the
// stale-bypass reaper (sweepStaleBypassFiles) must NOT delete a file owned by
// another user — it could be that user's live one-shot `--settings` bypass. When
// ownership cannot be determined from the stat result, it returns false so the
// reaper skips the file (conservative: never delete what we can't prove is ours).
func fileOwnedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false // can't determine ownership → skip deletion
	}
	return int(st.Uid) == os.Getuid()
}
