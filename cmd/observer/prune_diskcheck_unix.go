//go:build unix

package main

import "golang.org/x/sys/unix"

// diskHeadroomCheckSupported is true wherever statfsFreeBytes can actually
// report free space (every unix target this binary ships for: linux,
// darwin, freebsd, ...). Mirrors the unix/other split in
// internal/orgserver/policykey_open_{unix,other}.go.
const diskHeadroomCheckSupported = true

// statfsFreeBytes returns the free space available to an unprivileged
// process on the filesystem containing path, in bytes (Bavail * Bsize,
// not Bfree * Bsize — Bavail excludes root-reserved blocks, which is the
// honest number for "can this process actually write N more bytes here").
func statfsFreeBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), //nolint:unconvert,gosec // G115: Bavail/Bsize widths vary by unix target (int32 on some); widening is intentional and lossless for real free-space values.
		nil
}
