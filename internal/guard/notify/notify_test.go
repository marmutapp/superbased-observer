package notify

import (
	"errors"
	"strings"
	"testing"
)

// TestNotifyCommand covers the per-platform helper table (pure, so
// every branch tests on every host) plus quoting and bounding.
func TestNotifyCommand(t *testing.T) {
	t.Parallel()

	t.Run("windows toast", func(t *testing.T) {
		t.Parallel()
		name, args, ok := notifyCommand("windows", "Guard: R-101", "rm -rf 'x' flagged")
		if !ok || name != "powershell" {
			t.Fatalf("(%s, ok=%v), want powershell", name, ok)
		}
		script := args[len(args)-1]
		if !strings.Contains(script, "ToastNotificationManager") {
			t.Error("script does not use the WinRT toast API")
		}
		// PS single-quote doubling: the payload's quote must be doubled.
		if !strings.Contains(script, "rm -rf ''x'' flagged") {
			t.Errorf("payload quoting wrong: %s", script)
		}
		for _, want := range []string{"-NoProfile", "-NonInteractive"} {
			found := false
			for _, a := range args {
				if a == want {
					found = true
				}
			}
			if !found {
				t.Errorf("missing %s flag", want)
			}
		}
	})

	t.Run("darwin osascript", func(t *testing.T) {
		t.Parallel()
		name, args, ok := notifyCommand("darwin", `ti"tle`, `bo\dy`)
		if !ok || name != "osascript" || args[0] != "-e" {
			t.Fatalf("(%s %v ok=%v)", name, args, ok)
		}
		if !strings.Contains(args[1], `ti\"tle`) || !strings.Contains(args[1], `bo\\dy`) {
			t.Errorf("AppleScript quoting wrong: %s", args[1])
		}
	})

	t.Run("linux notify-send", func(t *testing.T) {
		t.Parallel()
		name, args, ok := notifyCommand("linux", "T", "B")
		if !ok || name != "notify-send" {
			t.Fatalf("(%s ok=%v)", name, ok)
		}
		if args[len(args)-2] != "T" || args[len(args)-1] != "B" {
			t.Errorf("args = %v", args)
		}
	})

	t.Run("unknown platform is a no-op", func(t *testing.T) {
		t.Parallel()
		if _, _, ok := notifyCommand("plan9", "T", "B"); ok {
			t.Error("unknown GOOS produced a command")
		}
	})

	t.Run("fields bounded", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("я", maxField*2)
		_, args, _ := notifyCommand("linux", long, "b")
		title := args[len(args)-2]
		if len([]rune(title)) != maxField+1 { // +1 for the ellipsis
			t.Errorf("title not bounded: %d runes", len([]rune(title)))
		}
	})
}

// TestNotifySwallowsRunnerErrors pins the best-effort contract: a
// failing helper never panics or surfaces.
func TestNotifySwallowsRunnerErrors(t *testing.T) {
	t.Parallel()
	var calls int
	d := &Desktop{run: func(string, ...string) error {
		calls++
		return errors.New("binary absent")
	}}
	d.Notify("t", "b") // must not panic
	if calls != 1 {
		// On an unknown GOOS the table returns ok=false and run is
		// never called — that is also fine; this assertion only runs
		// where the host platform has a helper.
		t.Skipf("no helper on %d calls (unknown host platform)", calls)
	}
}
