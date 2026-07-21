//go:build windows

package main

import "os"

// fileOwnedByCurrentUser reports whether info's file is owned by the current
// user (finding N6). The shared-non-sticky-TMPDIR multi-user hazard the reaper
// guards against is a POSIX concern; on Windows the per-user temp directory
// (%TEMP% under the user profile) is not shared across users, so a matching
// bypass file is always the current user's — treat every match as owned so the
// stale-file reaper keeps working on Windows.
func fileOwnedByCurrentUser(_ os.FileInfo) bool {
	return true
}
