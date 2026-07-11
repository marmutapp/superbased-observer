//go:build linux

package poll

import (
	"os"
	"strings"
)

// platformBootID returns the kernel boot id, which changes every boot so a
// reused pid across reboots never collides in a ProcessKey. Empty if
// unreadable (the key then degrades to (pid, start) uniqueness within a
// single boot, which is still correct for live observation).
func platformBootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
