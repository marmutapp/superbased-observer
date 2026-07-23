//go:build windows

package fsview

import "os"

// openForRead opens path read-only. Windows has no O_NONBLOCK/FIFO-blocking
// hazard of the same shape as unix named pipes on this read path, so a plain
// Open suffices; the caller still re-checks f.Stat().Mode().IsRegular() (finding
// 3).
func openForRead(path string) (*os.File, error) {
	return os.Open(path)
}
