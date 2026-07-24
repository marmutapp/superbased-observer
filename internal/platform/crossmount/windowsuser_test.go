package crossmount

import "testing"

// forceWindowsUserProbe pins the identity-probe + WSL-gate seams for a test
// (restored on cleanup) so resolveWindowsUserName is deterministic on any host.
func forceWindowsUserProbe(t *testing.T, wsl bool, name string) {
	t.Helper()
	prevProbe, prevWSL := windowsUserProbe, windowsUserIsWSL
	windowsUserProbe = func() string { return name }
	windowsUserIsWSL = func() bool { return wsl }
	t.Cleanup(func() { windowsUserProbe = prevProbe; windowsUserIsWSL = prevWSL })
}

func TestResolveWindowsUserName(t *testing.T) {
	tests := []struct {
		name  string
		wsl   bool
		probe string
		want  string
	}{
		{name: "wsl with a name", wsl: true, probe: "Alice", want: "Alice"},
		{name: "wsl but interop off (empty probe)", wsl: true, probe: "", want: ""},
		{name: "not wsl never probes", wsl: false, probe: "Alice", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forceWindowsUserProbe(t, tc.wsl, tc.probe)
			if got := resolveWindowsUserName(); got != tc.want {
				t.Errorf("resolveWindowsUserName() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHomeOwnedBy(t *testing.T) {
	tests := []struct {
		name, home, user string
		want             bool
	}{
		{name: "exact match", home: "/mnt/c/Users/Alice", user: "Alice", want: true},
		{name: "case-insensitive match", home: "/mnt/c/Users/Alice", user: "alice", want: true},
		{name: "different user", home: "/mnt/c/Users/Alice", user: "Bob", want: false},
		{name: "unknown user never owns", home: "/mnt/c/Users/Alice", user: "", want: false},
		{name: "trailing slash still matches leaf", home: "/mnt/c/Users/Alice/", user: "Alice", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := homeOwnedBy(tc.home, tc.user); got != tc.want {
				t.Errorf("homeOwnedBy(%q, %q) = %v, want %v", tc.home, tc.user, got, tc.want)
			}
		})
	}
}
