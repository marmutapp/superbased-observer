//go:build linux

package bridge

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/processobs"
)

// These tests drive the bridge backend's streaming/respawn core directly
// (white-box, bypassing Start's isWSL gate) against a fake capturer — a shell
// script standing in for the Windows observer.exe `process-bridge` over interop.
// Linux-tagged (needs /bin/sh); validated under WSL + Linux CI. Start's gate +
// the real interop spawn are exercised by the P-B5 live smoke.

// writeFakeCapturer writes an executable /bin/sh script that emits `body` to
// stdout verbatim, then exits with `code`. Returns its path.
func writeFakeCapturer(t *testing.T, body string, code int) string {
	t.Helper()
	dir := t.TempDir()
	bodyFile := filepath.Join(dir, "body.ndjson")
	if err := os.WriteFile(bodyFile, []byte(body), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncat " + bodyFile + "\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, "fake-capturer.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { //nolint:gosec // executable fixture
		t.Fatal(err)
	}
	return path
}

// cannedStream builds an NDJSON hello + the given events via the real Encoder.
func cannedStream(t *testing.T, evs ...processobs.RawEvent) string {
	t.Helper()
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	if err := enc.Hello(Hello{Backend: "poll", OS: "windows", PID: 1}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		if err := enc.Event(ev); err != nil {
			t.Fatal(err)
		}
	}
	return buf.String()
}

func TestResolveWindowsObserverExplicitFound(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "observer.exe")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	// On Linux t.TempDir() is /tmp/... (no drive letter), so the explicit path
	// passes through windowsToWSLPath unchanged and resolves to the real file.
	got, ok := ResolveWindowsObserver(exe)
	if !ok || got != exe {
		t.Fatalf("ResolveWindowsObserver(explicit) = (%q, %v), want (%q, true)", got, ok, exe)
	}
}

func TestResolveWindowsObserverMiss(t *testing.T) {
	// Isolate from the auto-candidates: an empty cwd (no bin/observer.exe) and
	// a cleared env override. The os.Executable() candidate is a build temp
	// binary with no observer.exe beside it, so it is skipped.
	t.Chdir(t.TempDir())
	t.Setenv("OBSERVER_WINDOWS_BINARY", "")
	if got, ok := ResolveWindowsObserver(filepath.Join(t.TempDir(), "nope.exe")); ok {
		t.Fatalf("expected a miss, resolved to %q", got)
	}
}

func TestRunCapturerForwardsEvents(t *testing.T) {
	body := cannedStream(
		t,
		processobs.RawEvent{Type: processobs.EventExec, PID: 100, PPID: 4, StartTimeTicks: 1, HasStartTime: true, BootID: "b", CWD: `C:\proj`},
		processobs.RawEvent{Type: processobs.EventExit, PID: 100, StartTimeTicks: 1, HasStartTime: true, BootID: "b"},
	)
	b := &Backend{resolvedPath: writeFakeCapturer(t, body, 0), out: make(chan processobs.RawEvent, 16), stop: make(chan struct{})}

	events, err := b.runCapturer(context.Background())
	if err != nil {
		t.Fatalf("runCapturer: %v", err)
	}
	if events != 2 {
		t.Fatalf("forwarded %d events, want 2", events)
	}
	close(b.out)
	var got []processobs.RawEvent
	for ev := range b.out {
		got = append(got, ev)
	}
	if len(got) != 2 || got[0].CWD != `C:\proj` || got[1].Type != processobs.EventExit {
		t.Fatalf("unexpected forwarded events: %+v", got)
	}
	if b.Stats().Events != 2 {
		t.Fatalf("Stats().Events = %d, want 2", b.Stats().Events)
	}
}

func TestRunCapturerToleratesDecodeErrors(t *testing.T) {
	good := cannedStream(t, processobs.RawEvent{Type: processobs.EventExec, PID: 7, StartTimeTicks: 1, HasStartTime: true, BootID: "b"})
	// Splice a garbage line between the hello and the event line.
	parts := strings.SplitN(good, "\n", 2)
	body := parts[0] + "\n" + "GARBAGE NOT JSON\n" + parts[1]
	b := &Backend{resolvedPath: writeFakeCapturer(t, body, 0), out: make(chan processobs.RawEvent, 16), stop: make(chan struct{})}

	events, _ := b.runCapturer(context.Background())
	if events != 1 {
		t.Fatalf("forwarded %d events, want 1 (garbage line skipped)", events)
	}
	if b.Stats().DecodeErrs == 0 {
		t.Fatal("expected a decode error to be counted")
	}
}

func TestLoopRespawnsAndGivesUp(t *testing.T) {
	// A capturer that emits nothing and exits non-zero produces 0 events each
	// run → the loop respawns up to the failure cap, then closes its channel.
	b := &Backend{
		resolvedPath: writeFakeCapturer(t, "", 1),
		out:          make(chan processobs.RawEvent, 4),
		stop:         make(chan struct{}),
		minBackoff:   time.Millisecond,
		maxBackoff:   2 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() { b.loop(context.Background()); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not give up within timeout")
	}
	if _, open := <-b.out; open {
		t.Fatal("expected b.out closed after give-up")
	}
	if got := b.Stats().Respawns; got < maxConsecutiveFailures {
		t.Fatalf("respawns = %d, want >= %d", got, maxConsecutiveFailures)
	}
	if b.Stats().LastErr == "" {
		t.Fatal("expected a non-empty LastErr after give-up")
	}
}

func TestLoopRespawnsAndKeepsStreaming(t *testing.T) {
	// A capturer that emits one event then exits cleanly: a healthy run resets
	// the failure streak, so the loop keeps respawning and streaming forever.
	body := cannedStream(t, processobs.RawEvent{Type: processobs.EventExec, PID: 1, StartTimeTicks: 1, HasStartTime: true, BootID: "b"})
	b := &Backend{
		resolvedPath: writeFakeCapturer(t, body, 0),
		out:          make(chan processobs.RawEvent, 64),
		stop:         make(chan struct{}),
		minBackoff:   time.Millisecond,
		maxBackoff:   2 * time.Millisecond,
	}
	go b.loop(context.Background())
	defer b.Close()

	got := 0
	deadline := time.After(3 * time.Second)
	for got < 3 {
		select {
		case <-b.out:
			got++
		case <-deadline:
			t.Fatalf("collected only %d events across respawns, want >= 3", got)
		}
	}
	if r := b.Stats().Respawns; r < 2 {
		t.Fatalf("respawns = %d, want >= 2 (proves it relaunched)", r)
	}
}
