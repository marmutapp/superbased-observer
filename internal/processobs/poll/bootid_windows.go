//go:build windows

package poll

import (
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// platformBootID returns a per-boot identifier so a reused pid across reboots
// never collides in a ProcessKey. Windows has no /proc/sys/kernel/random/
// boot_id, so we derive a stable string from the system boot instant
// (now − uptime, to the second). The poll Backend calls this once at New, so
// the value is constant for the daemon's lifetime; second-level precision is
// ample to distinguish boots.
func platformBootID() string {
	bootUnix := time.Now().Add(-windows.DurationSinceBoot()).Unix()
	return "win-boot-" + strconv.FormatInt(bootUnix, 10)
}
