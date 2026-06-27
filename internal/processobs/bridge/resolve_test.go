package bridge

import "testing"

// windowsToWSLPath is a pure string transform, so this runs on every host. The
// filesystem-backed ResolveWindowsObserver tests live under the linux tag —
// it is WSL-only production code, and on a Windows host t.TempDir() yields a
// C:\ path that (correctly, for the WSL runtime) translates to a /mnt path
// that does not exist on the Windows host.
func TestWindowsToWSLPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`C:\Users\m\observer.exe`, "/mnt/c/Users/m/observer.exe"},
		{`D:/proj/observer.exe`, "/mnt/d/proj/observer.exe"},
		{"/mnt/d/proj/observer.exe", "/mnt/d/proj/observer.exe"}, // already a mount path
		{"/home/m/observer", "/home/m/observer"},                 // wsl-native, unchanged
		{"relative/path", "relative/path"},
		{"", ""},
	}
	for _, c := range cases {
		if got := windowsToWSLPath(c.in); got != c.want {
			t.Errorf("windowsToWSLPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
