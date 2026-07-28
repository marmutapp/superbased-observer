package etw

import (
	"errors"
	"syscall"
)

// Sentinel errors. They are declared in an UNTAGGED file on purpose: the
// elevation and unsupported-OS outcomes are the degradation paths an operator
// actually hits, and the plan's "validation reality" section notes that the
// degradation path is the one an automated test CAN cover. Keeping the
// sentinels off the windows build tag lets those assertions run on Linux.
var (
	// ErrNeedsElevation reports that the process lacks the privilege ETW
	// session control requires. Returned when StartTraceW fails with
	// ERROR_ACCESS_DENIED (5) — the E0 spike's finding 3. Callers should treat
	// this as "degrade cleanly and tell the operator", never as a crash: the
	// non-elevated poll bridge keeps working without ETW.
	ErrNeedsElevation = errors.New("etw: controlling a trace session requires an elevated process (Administrator, Performance Log Users, or a LocalSystem service)")

	// ErrSessionExists reports that a trace session with the requested name is
	// already running and could not be reclaimed.
	ErrSessionExists = errors.New("etw: a trace session with that name already exists")

	// ErrUnsupportedOS reports that ETW capture was requested off Windows. It
	// is what the non-Windows stub returns so OS-agnostic wiring in later
	// phases compiles and fails legibly everywhere.
	ErrUnsupportedOS = errors.New("etw: ETW capture is only available on Windows")

	// ErrShortPayload reports an event payload smaller than its template's
	// documented length. Payloads LONGER than documented are accepted — every
	// field this package reads sits in the fixed leading prefix, so trailing
	// growth (a wider connid on some build, say) is harmless — but a short one
	// means the event is not what its id claims and must not be decoded.
	ErrShortPayload = errors.New("etw: event payload shorter than its documented template")

	// ErrNotTCPEvent reports that DecodeTCPData was handed an event id that is
	// not a TCP data event. UDP ids hit this — that refusal is the type-system
	// half of the TCP-only scope guarantee.
	ErrNotTCPEvent = errors.New("etw: event id is not a TCP data-transfer event")

	// ErrNotUDPEvent reports that DecodeUDPDatagram was handed an event id that
	// is not a UDP datagram event.
	ErrNotUDPEvent = errors.New("etw: event id is not a UDP datagram event")

	// ErrNoHandler reports that a Session was configured with neither an OnTCP
	// nor an OnUDP callback, which would start a privileged trace that throws
	// every event away.
	ErrNoHandler = errors.New("etw: session needs at least one event handler")

	// ErrUnsupportedEventVersion reports an event whose EVENT_DESCRIPTOR
	// .Version is not the one this package's layout table describes (see
	// supportedEventVersion). The alternative — decoding it anyway because the
	// payload happens to be long enough — is the silent-garbage failure the
	// plan's §0.1 exists to prevent, so such events are refused and counted in
	// Stats.UnsupportedVersion instead.
	ErrUnsupportedEventVersion = errors.New("etw: event version is newer than the layout this package decodes")

	// ErrUnknownAddressFamily reports a layout-table row with no address
	// family. It is a bug in this package's own table, not a property of the
	// event, and is deliberately NOT ErrShortPayload: the two need different
	// investigations.
	ErrUnknownAddressFamily = errors.New("etw: event template declares no address family")

	// ErrProcUnavailable reports that an advapi32.dll ETW entry point could not
	// be resolved. (*windows.LazyProc).Call PANICS on an unresolved proc, and
	// CLAUDE.md forbids panic() in library code, so every entry point is
	// Find()-resolved up front and this is returned instead.
	ErrProcUnavailable = errors.New("etw: an advapi32.dll ETW entry point is unavailable")
)

// errnoFromCall normalises the third result of a (*windows.LazyProc).Call into
// something safe to render.
//
// THE FOOTGUN IT EXISTS FOR: Call's error result is a syscall.Errno boxed in a
// non-nil error interface, ALWAYS. `if err == nil` after a Call is therefore
// dead code — the interface holds a concrete type even when GetLastError
// returned 0 — and formatting that zero errno yields, on Windows, the
// self-contradicting "The operation completed successfully." on exactly the
// path an operator reads while debugging a failure.
//
// So: test the call's own documented failure sentinel (an invalid handle, a
// non-zero return code) FIRST, and only then ask this function whether there is
// a real reason to append. ok is false for a nil error, a zero errno, and any
// non-Errno error — i.e. false means "say nothing about errno".
func errnoFromCall(err error) (syscall.Errno, bool) {
	if err == nil {
		return 0, false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) || errno == 0 {
		return 0, false
	}
	return errno, true
}
