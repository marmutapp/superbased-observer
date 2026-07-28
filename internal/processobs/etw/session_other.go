//go:build !windows

package etw

// Non-Windows stub. ETW exists only on Windows, but Session / NewSession /
// IsElevated are declared on every OS so later phases can write OS-agnostic
// wiring that compiles for linux and darwin unchanged — the same shape
// linuxebpf/backend_other.go and poll/enum_other.go already use.
//
// The decode half of this package (parse.go) is genuinely portable and carries
// no build tag: that is what makes the arc testable in CI, which has no Windows
// runner.

// Session is the non-Windows placeholder for an ETW trace session. It can never
// be constructed here; NewSession always fails with ErrUnsupportedOS.
//
// It deliberately carries NO fields — not even the sessionHandles the Windows
// Session holds. There is nothing off Windows to hold handles for, and an
// unpopulated field would only be dead weight. sessionHandles itself lives in
// an untagged file so its concurrency can be race-tested here; that is a
// property of the TYPE, not of this stub.
type Session struct{}

// Stats is the non-Windows placeholder for a Session's decode counters, present
// so callers can name the type on any OS. It must stay field-for-field
// identical to the Windows Stats.
type Stats struct {
	// Decoded is the number of events handed to a handler.
	Decoded int64
	// Dropped is the number of data events that failed to decode.
	Dropped int64
	// Ignored is the number of events not decoded by design.
	Ignored int64
	// UnsupportedVersion is the number of data events refused because their
	// event version is not the one the layout table describes.
	UnsupportedVersion int64
}

// NewSession always fails off Windows.
//
// Options are validated FIRST, so a misconfiguration (no handler) is reported
// as a misconfiguration on every OS rather than being masked by the platform
// error — a config bug found on a developer's Linux box is cheaper than one
// found on an elevated Windows task.
func NewSession(opts Options) (*Session, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	return nil, ErrUnsupportedOS
}

// Process always fails off Windows.
func (s *Session) Process() error { return ErrUnsupportedOS }

// Close is a no-op off Windows.
func (s *Session) Close() error { return nil }

// Stats reports zeroes off Windows.
func (s *Session) Stats() Stats { return Stats{} }

// IsElevated reports false off Windows: there is no ETW session to elevate for.
// The error is returned rather than a bare false so a caller cannot read the
// result as "checked, and not elevated".
func IsElevated() (bool, error) { return false, ErrUnsupportedOS }
