//go:build !windows

package main

import "errors"

// errETWNotWindows is the refusal every non-Windows build gives for --etw. It
// is a plain sentinel rather than a call into internal/processobs/etw so this
// binary never links the Windows tracing package on Linux/macOS; the flag stays
// accepted everywhere (the capturer is deliberately cross-OS for testing) and
// simply degrades to poll-only capture with a legible reason.
var errETWNotWindows = errors.New("ETW is a Windows-only facility; this capturer is not running on Windows")

// startETWNetworkCapture always refuses off Windows.
func startETWNetworkCapture() (etwNetworkCapture, error) {
	return nil, errETWNotWindows
}
