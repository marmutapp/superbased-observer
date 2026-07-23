package main

import (
	"os"
	"strings"
	"testing"
)

func TestCommandNameFrom(t *testing.T) {
	cases := []struct {
		name string
		argv string
		want string
	}{
		{"canonical observer", "/usr/local/bin/observer", "observer"},
		{"superbased basename", "/usr/local/bin/superbased", "superbased"},
		{"superbased windows exe", `C:\tools\superbased.exe`, "superbased"},
		{"observer windows exe", `C:\tools\observer.exe`, "observer"},
		{"npm shim spawns as observer", "/home/u/.npm/_npx/observer", "observer"},
		{"bare superbased", "superbased", "superbased"},
		{"unknown argv0 defaults to observer", "/tmp/go-build123/b001/exe/main", "observer"},
		{"test binary defaults to observer", "cmd.test", "observer"},
		{"empty defaults to observer", "", "observer"},
		{"windows mixed case exe", `C:\Program Files\SuperBased.EXE`, "superbased"},
		{"observer-like prefix not matched", "/usr/bin/observer-org", "observer"},
		{"superbased-suffix not matched", "/usr/bin/mysuperbased", "observer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commandNameFrom(tc.argv); got != tc.want {
				t.Fatalf("commandNameFrom(%q) = %q, want %q", tc.argv, got, tc.want)
			}
		})
	}
}

// TestRootCmdUseFromArgv0 pins the Cobra wiring: the root command's displayed
// name follows argv[0], and subcommand dispatch keeps working under either
// name (a UseLine/Name regression or broken dispatch would slip past the pure
// commandNameFrom test).
func TestRootCmdUseFromArgv0(t *testing.T) {
	orig := os.Args[0]
	t.Cleanup(func() { os.Args[0] = orig })

	for _, tc := range []struct{ argv0, want string }{
		{"/usr/local/bin/superbased", "superbased"},
		{"/usr/local/bin/observer", "observer"},
		{`C:\tools\SuperBased.exe`, "superbased"},
	} {
		os.Args[0] = tc.argv0
		root := newRootCmd()
		if root.Name() != tc.want {
			t.Fatalf("argv0 %q: root.Name() = %q, want %q", tc.argv0, root.Name(), tc.want)
		}
		if !strings.HasPrefix(root.UseLine(), tc.want) {
			t.Fatalf("argv0 %q: UseLine() = %q, want prefix %q", tc.argv0, root.UseLine(), tc.want)
		}
		// Subcommand dispatch must resolve regardless of the root's name.
		if cmd, _, err := root.Find([]string{"doctor"}); err != nil || cmd == nil || cmd.Name() != "doctor" {
			t.Fatalf("argv0 %q: Find([doctor]) failed: cmd=%v err=%v", tc.argv0, cmd, err)
		}
	}
}
