//go:build windows

package main

import "github.com/marmutapp/superbased-observer/internal/processobs/etw"

// startETWNetworkCapture starts the ETW per-process network capture on
// Windows.
//
// The error path is the EXPECTED path: starting an ETW trace session always
// requires an elevated process, so an ordinary capturer run gets
// ERROR_ACCESS_DENIED here. The caller must treat that as "no network series",
// never as a reason to stop capturing.
func startETWNetworkCapture() (etwNetworkCapture, error) {
	c, err := etw.StartCapture(etw.Options{})
	if err != nil {
		return nil, err
	}
	return c, nil
}
