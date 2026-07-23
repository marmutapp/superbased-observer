//go:build unix

package fsview

import (
	"os"
	"syscall"
)

// openForRead opens path read-only and NON-BLOCKING. O_NONBLOCK ensures that a
// FIFO or device that slipped past the Lstat regular-file check (e.g. a regular
// file swapped to a FIFO between Lstat and open) returns a descriptor
// immediately instead of blocking the HTTP handler forever waiting for a writer
// (finding 3). It has no effect on a regular file. The caller MUST re-check
// f.Stat().Mode().IsRegular() after this returns.
func openForRead(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
