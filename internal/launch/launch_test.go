package launch

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestPlanMechanismTable(t *testing.T) {
	exe := "/opt/observer/bin/observer"
	cases := []struct {
		name       string
		req        Request
		env        Environment
		wantMethod string
		wantArgv   []string
		wantCmd    string
	}{
		{
			name:       "windows wt routed",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "windows", WTPath: `C:\wt.exe`, CmdPath: `C:\cmd.exe`, ExePath: `C:\obs.exe`},
			wantMethod: "wt",
			wantArgv:   []string{`C:\wt.exe`, "cmd", "/k", "claude"},
			wantCmd:    "claude",
		},
		{
			name:       "windows cmd-start fallback wrapper",
			req:        Request{Tool: "claude-code"},
			env:        Environment{GOOS: "windows", CmdPath: `C:\cmd.exe`, ExePath: `C:\Program Files\obs.exe`},
			wantMethod: "cmd-start",
			wantArgv:   []string{`C:\cmd.exe`, "/c", "start", "", "cmd", "/k", `C:\Program Files\obs.exe`, "claude"},
			wantCmd:    `'C:\Program Files\obs.exe' claude`,
		},
		{
			name:       "wsl wt routed with distro",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "linux", IsWSL: true, WSLDistro: "Ubuntu", WTPath: "/mnt/c/wt.exe", CmdPath: "/mnt/c/cmd.exe", ExePath: exe},
			wantMethod: "wsl-wt",
			wantArgv:   []string{"/mnt/c/wt.exe", "wsl.exe", "-d", "Ubuntu", "--", "bash", "-lc", "claude"},
			wantCmd:    "claude",
		},
		{
			name:       "wsl cmd-start no wt, wrapper, default distro",
			req:        Request{Tool: "codex"},
			env:        Environment{GOOS: "linux", IsWSL: true, CmdPath: "/mnt/c/cmd.exe", ExePath: exe},
			wantMethod: "wsl-cmd-start",
			wantArgv:   []string{"/mnt/c/cmd.exe", "/c", "start", "", "wsl.exe", "--", "bash", "-lc", exe + " codex"},
			wantCmd:    exe + " codex",
		},
		{
			name:       "wsl no interop launchers",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "linux", IsWSL: true, ExePath: exe},
			wantMethod: "none",
			wantCmd:    "claude",
		},
		{
			name:       "linux gnome-terminal",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "linux", Display: true, TermPath: "/usr/bin/gnome-terminal", TermName: "gnome-terminal", ExePath: exe},
			wantMethod: "terminal",
			wantArgv:   []string{"/usr/bin/gnome-terminal", "--", "bash", "-lc", "claude"},
			wantCmd:    "claude",
		},
		{
			name:       "linux xterm -e family",
			req:        Request{Tool: "codex", Routed: true},
			env:        Environment{GOOS: "linux", Display: true, TermPath: "/usr/bin/xterm", TermName: "xterm", ExePath: exe},
			wantMethod: "terminal",
			wantArgv:   []string{"/usr/bin/xterm", "-e", "bash", "-lc", "codex"},
			wantCmd:    "codex",
		},
		{
			name:       "linux headless",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "linux", TermPath: "/usr/bin/xterm", TermName: "xterm", ExePath: exe},
			wantMethod: "none",
			wantCmd:    "claude",
		},
		{
			name:       "darwin osascript",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "darwin", OsaPath: "/usr/bin/osascript", ExePath: exe},
			wantMethod: "osascript",
			wantArgv: []string{
				"/usr/bin/osascript",
				"-e", `tell application "Terminal" to do script "claude"`,
				"-e", `tell application "Terminal" to activate`,
			},
			wantCmd: "claude",
		},
		{
			name:       "unsupported platform",
			req:        Request{Tool: "claude-code", Routed: true},
			env:        Environment{GOOS: "plan9", ExePath: exe},
			wantMethod: "none",
			wantCmd:    "claude",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := Plan(tc.req, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Method != tc.wantMethod {
				t.Errorf("method: got %q want %q", spec.Method, tc.wantMethod)
			}
			if !reflect.DeepEqual(spec.Argv, tc.wantArgv) {
				t.Errorf("argv:\n got %#v\nwant %#v", spec.Argv, tc.wantArgv)
			}
			if spec.Command != tc.wantCmd {
				t.Errorf("command: got %q want %q", spec.Command, tc.wantCmd)
			}
			if spec.Command == "" {
				t.Error("Command must always be set (honesty contract)")
			}
			if tc.wantMethod == "none" && spec.Reason == "" {
				t.Error("empty Argv must carry a Reason")
			}
		})
	}
}

func TestPlanRejectsUnknownTool(t *testing.T) {
	_, err := Plan(Request{Tool: "rm -rf /"}, Environment{GOOS: "linux", ExePath: "/x"})
	if err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("want unknown-tool error, got %v", err)
	}
}

func TestPlanWrapperNeedsExePath(t *testing.T) {
	_, err := Plan(Request{Tool: "claude-code"}, Environment{GOOS: "windows", WTPath: "wt"})
	if err == nil {
		t.Fatal("want error when wrapper launch has no exe path")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"claude":        "claude",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"/plain/path-1": "/plain/path-1",
		"":              "''",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpawnEmptyArgv(t *testing.T) {
	if err := Spawn(context.Background(), Spec{}); err == nil {
		t.Fatal("want error for empty argv")
	}
}
