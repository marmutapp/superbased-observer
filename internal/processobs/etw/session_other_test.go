//go:build !windows

package etw

import (
	"errors"
	"testing"
)

// The Windows capture path itself cannot be exercised from CI (no Windows
// runner). What CAN be pinned everywhere is the degradation contract: the
// package refuses cleanly and with a typed error rather than panicking or
// returning a usable-looking zero Session.
func TestNewSessionOffWindows(t *testing.T) {
	t.Parallel()

	t.Run("reports the unsupported OS", func(t *testing.T) {
		t.Parallel()
		s, err := NewSession(Options{OnTCP: func(TCPDataEvent) {}})
		if !errors.Is(err, ErrUnsupportedOS) {
			t.Fatalf("NewSession() error = %v, want ErrUnsupportedOS", err)
		}
		if s != nil {
			t.Fatalf("NewSession() session = %v, want nil", s)
		}
	})

	t.Run("a config error is reported ahead of the platform error", func(t *testing.T) {
		t.Parallel()
		if _, err := NewSession(Options{}); !errors.Is(err, ErrNoHandler) {
			t.Fatalf("NewSession() error = %v, want ErrNoHandler", err)
		}
	})

	t.Run("IsElevated does not claim to have checked", func(t *testing.T) {
		t.Parallel()
		elevated, err := IsElevated()
		if elevated {
			t.Fatalf("IsElevated() = true off Windows")
		}
		if !errors.Is(err, ErrUnsupportedOS) {
			t.Fatalf("IsElevated() error = %v, want ErrUnsupportedOS", err)
		}
	})
}

// ErrNeedsElevation is the sentinel the Windows path returns for a
// non-elevated StartTraceW. It lives in an untagged file so this identity
// assertion — the thing a caller will actually errors.Is against — runs on the
// platform CI actually has.
func TestElevationSentinelIsDistinct(t *testing.T) {
	t.Parallel()

	all := []error{
		ErrNeedsElevation, ErrSessionExists, ErrUnsupportedOS, ErrShortPayload,
		ErrNotTCPEvent, ErrNotUDPEvent, ErrNoHandler, ErrUnsupportedEventVersion,
		ErrUnknownAddressFamily, ErrProcUnavailable,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Fatalf("sentinel %v matches unrelated sentinel %v", a, b)
			}
		}
	}
}
