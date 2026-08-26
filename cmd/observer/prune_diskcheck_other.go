//go:build !unix

package main

import "errors"

// diskHeadroomCheckSupported is false off unix (e.g. windows): there is no
// portable free-space syscall in this build, so the T0.3 pre-flight check
// is skipped there rather than guessed at. Mirrors the unix/other split in
// internal/orgserver/policykey_open_{unix,other}.go.
const diskHeadroomCheckSupported = false

// statfsFreeBytes always fails off unix — callers must gate on
// diskHeadroomCheckSupported and skip the check rather than call this.
func statfsFreeBytes(_ string) (uint64, error) {
	return 0, errors.New("disk free-space check not supported on this platform")
}
