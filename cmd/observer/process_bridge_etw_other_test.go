//go:build !windows

package main

import (
	"strings"
	"testing"
)

// TestStartETWNetworkCaptureOffWindows pins the platform shim's refusal: off
// Windows there is no ETW at all, and the capturer must learn that as a plain
// error with a legible reason — never a nil capture it would then dereference,
// and never a silent success that would make the hello frame claim a feed that
// does not exist.
func TestStartETWNetworkCaptureOffWindows(t *testing.T) {
	c, err := startETWNetworkCapture()
	if err == nil {
		t.Fatal("startETWNetworkCapture succeeded off Windows")
	}
	if c != nil {
		t.Fatalf("expected a nil capture alongside the error, got %#v", c)
	}
	if !strings.Contains(err.Error(), "Windows") {
		t.Errorf("error %q does not say why it refused", err)
	}
}
