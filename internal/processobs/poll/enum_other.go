//go:build !linux && !windows

package poll

// platformEnumerate has no native implementation on this OS yet (Linux reads
// /proc, Windows uses ToolHelp; macOS via libproc/sysctl is a later
// increment). It returns ErrUnsupported so Backend.Start fails cleanly and the
// Observer reports degraded health — the daemon keeps running (fail-open). An
// operator on such a host should run with a native backend once available.
func platformEnumerate() ([]ProcInfo, error) {
	return nil, ErrUnsupported
}

// platformBootID has no source on non-Linux yet; "" is acceptable because
// platformEnumerate fails first and no events are produced.
func platformBootID() string { return "" }

// platformSessionToken has no env-read implementation on this OS (§5.5 P-B6
// env-token is Windows-only; Linux uses the direct pidbridge seed). "" disables
// EV here — platformEnumerate fails first anyway, so no events are produced.
func platformSessionToken(int) string { return "" }
