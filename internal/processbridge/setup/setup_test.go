package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// Nothing in this file executes a real schtasks.exe. Every probe goes through
// the Env seam, so these tests are identical on CI (no
// Windows, no Task Scheduler) and on the WSL reference host — and they can
// never create, change or delete an operator's real Scheduled Task.

// fakeTaskEnv builds a Env whose every side effect is
// recorded, plus counters proving which probes were reached.
type fakeTaskEnv struct {
	env           Env
	schtasksCalls [][]string
	observerCalls int
	userCalls     int
}

// newFakeTaskEnv returns an env where schtasks.exe resolves, /Query answers
// with (out, err), and the Windows binary resolves to a /mnt path.
func newFakeTaskEnv(queryOut string, queryErr error) *fakeTaskEnv {
	f := &fakeTaskEnv{}
	f.env = Env{
		LookPath: func(string) (string, error) { return `/mnt/c/Windows/system32/schtasks.exe`, nil },
		RunSchtasks: func(_ context.Context, exe string, args ...string) (string, error) {
			f.schtasksCalls = append(f.schtasksCalls, append([]string{exe}, args...))
			return queryOut, queryErr
		},
		ResolveObserver: func(string) (string, bool) {
			f.observerCalls++
			return "/mnt/c/Users/auzy_/bin/observer.exe", true
		},
		Distro:      "Ubuntu-20.04",
		WindowsUser: func() string { f.userCalls++; return "auzy_" },
	}
	return f
}

// etwConfig is a ProcessConfig with the ETW listener block set.
// It switches BOTH gates together — the master
// [observer.process].enabled and [observer.process.etw].enabled — because
// "the feed is on" means both (see Inputs.Enabled). The one-off-each
// combination is exercised by TestResolveInputsRequiresBothSwitches.
func etwConfig(enabled bool) config.ProcessConfig {
	return config.ProcessConfig{
		Enabled: enabled,
		ETW: config.ProcessETWConfig{
			Enabled:    enabled,
			ListenAddr: "127.0.0.1:8823",
		},
	}
}

const schtasksNotFound = "ERROR: The system cannot find the file specified."

// TestPlanProcessBridgeTask pins the decision table: which inputs produce a
// silent skip, an idempotent no-op, an emitted command, or a named blocker.
func TestPlanProcessBridgeTask(t *testing.T) {
	base := Inputs{
		Enabled:         true,
		SchtasksPath:    `/mnt/c/Windows/system32/schtasks.exe`,
		Probe:           ProbeAbsent,
		WindowsObserver: "/mnt/c/Users/auzy_/bin/observer.exe",
		ListenAddr:      "127.0.0.1:8823",
		TokenPath:       "/home/marmutapp/.observer/process-bridge-token",
		Distro:          "Ubuntu-20.04",
		WindowsUser:     "auzy_",
	}
	with := func(mut func(*Inputs)) Inputs {
		in := base
		mut(&in)
		return in
	}

	tests := []struct {
		name        string
		in          Inputs
		wantOutcome Outcome
		wantCmd     []string // substrings required in Command
		wantReason  string   // substring required in Reason
	}{
		{
			name:        "etw disabled is a silent skip",
			in:          with(func(i *Inputs) { i.Enabled = false }),
			wantOutcome: OutcomeSkip,
		},
		{
			name:        "no schtasks on this host is a silent skip",
			in:          with(func(i *Inputs) { i.SchtasksPath = "" }),
			wantOutcome: OutcomeSkip,
		},
		{
			name:        "existing task is reported and left alone",
			in:          with(func(i *Inputs) { i.Probe = ProbePresent }),
			wantOutcome: OutcomePresent,
		},
		{
			name:        "absent task emits the fully-resolved command",
			in:          base,
			wantOutcome: OutcomeManual,
			wantCmd: []string{
				`/Create /TN "SuperBasedObserverETW"`,
				`/SC ONLOGON`,
				`/RL HIGHEST`,
				`process-bridge --etw --connect 127.0.0.1:8823 --token-file`,
				`C:\Users\auzy_\bin\observer.exe`,
				`\\wsl.localhost\Ubuntu-20.04\home\marmutapp\.observer\process-bridge-token`,
			},
		},
		{
			name: "unknown probe still emits, with the probe error carried",
			in: with(func(i *Inputs) {
				i.Probe = ProbeUnknown
				i.ProbeErr = "ERROR: Access is denied."
			}),
			wantOutcome: OutcomeUnknown,
			wantCmd:     []string{`/Create /TN "SuperBasedObserverETW"`},
			wantReason:  "Access is denied",
		},
		{
			name:        "no windows observer.exe names the exact missing dependency",
			in:          with(func(i *Inputs) { i.WindowsObserver = "" }),
			wantOutcome: OutcomeBlocked,
			wantReason:  "windows_binary_path",
		},
		{
			name: "a configured-but-missing binary names the path it tried",
			in: with(func(i *Inputs) {
				i.WindowsObserver = ""
				i.WindowsObserverHint = "/mnt/c/nope/observer.exe"
				i.WindowsObserverHintSource = "[observer.process].windows_binary_path"
			}),
			wantOutcome: OutcomeBlocked,
			wantReason:  `/mnt/c/nope/observer.exe`,
		},
		{
			name: "wsl-native token without a distro name is blocked, never guessed",
			in: with(func(i *Inputs) {
				i.Distro = ""
			}),
			wantOutcome: OutcomeBlocked,
			wantReason:  "WSL_DISTRO_NAME",
		},
		{
			name: "windows host needs no translation",
			in: with(func(i *Inputs) {
				i.HostIsWindows = true
				i.Distro = ""
				i.WindowsObserver = `C:\Program Files\observer\observer.exe`
				i.TokenPath = `C:\Users\auzy_\.observer\process-bridge-token`
			}),
			wantOutcome: OutcomeManual,
			wantCmd: []string{
				`'C:\Program Files\observer\observer.exe'`,
				`--token-file C:\Users\auzy_\.observer\process-bridge-token`,
			},
		},
		{
			name: "wildcard bind is dialled as loopback",
			in: with(func(i *Inputs) {
				i.ListenAddr = "0.0.0.0:8823"
			}),
			wantOutcome: OutcomeManual,
			wantCmd:     []string{"--connect 127.0.0.1:8823"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanTask(tc.in)
			if got.Outcome != tc.wantOutcome {
				t.Fatalf("Outcome = %v, want %v (plan %+v)", got.Outcome, tc.wantOutcome, got)
			}
			for _, want := range tc.wantCmd {
				if !strings.Contains(got.Command, want) {
					t.Errorf("Command missing %q\ngot: %s", want, got.Command)
				}
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tc.wantReason)
			}
			if tc.wantOutcome == OutcomeSkip || tc.wantOutcome == OutcomePresent {
				if got.Command != "" {
					t.Errorf("a %v plan must carry no command, got %q", tc.wantOutcome, got.Command)
				}
			}
			if tc.wantOutcome == OutcomeBlocked && got.Command != "" {
				t.Errorf("a blocked plan must not emit a half-resolved command, got %q", got.Command)
			}
		})
	}
}

// TestPlanProcessBridgeTaskNoPlaceholders is the copy-paste-ready contract:
// the emitted command must contain no placeholder markers at all.
func TestPlanProcessBridgeTaskNoPlaceholders(t *testing.T) {
	plan := PlanTask(Inputs{
		Enabled: true, SchtasksPath: "schtasks.exe", Probe: ProbeAbsent,
		WindowsObserver: "/mnt/c/o/observer.exe", ListenAddr: "127.0.0.1:8823",
		TokenPath: "/home/u/.observer/process-bridge-token", Distro: "Ubuntu-20.04",
	})
	for _, bad := range []string{"<", ">", "PLACEHOLDER", "TODO", "%USERNAME%", "..."} {
		if strings.Contains(plan.Command, bad) {
			t.Errorf("emitted command contains placeholder-ish %q: %s", bad, plan.Command)
		}
	}
}

// TestProbeProcessBridgeTask pins the tri-state classification, including the
// measured wording schtasks uses for a task that does not exist.
func TestProbeProcessBridgeTask(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		err     error
		want    Probe
		wantErr string
	}{
		{name: "exit 0 means present", out: "TaskName  Next Run Time", want: ProbePresent},
		{name: "measured not-found wording means absent", out: schtasksNotFound, err: errors.New("exit status 1"), want: ProbeAbsent},
		{name: "does not exist wording means absent", out: "ERROR: The specified task name does not exist.", err: errors.New("exit status 1"), want: ProbeAbsent},
		{
			name: "any other failure is unknown, never absent",
			out:  "ERROR: Access is denied.", err: errors.New("exit status 1"),
			want: ProbeUnknown, wantErr: "Access is denied",
		},
		{
			name: "an exec failure with no output is unknown",
			out:  "", err: errors.New("context deadline exceeded"),
			want: ProbeUnknown, wantErr: "deadline",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := probeTask(context.Background(), "schtasks.exe",
				func(context.Context, string, ...string) (string, error) { return tc.out, tc.err })
			if got != tc.want {
				t.Fatalf("probe = %v, want %v", got, tc.want)
			}
			if tc.wantErr != "" && !strings.Contains(detail, tc.wantErr) {
				t.Fatalf("detail = %q, want it to mention %q", detail, tc.wantErr)
			}
		})
	}
}

// TestProcessBridgeTaskWindowsPath pins the cross-OS path translation.
func TestProcessBridgeTaskWindowsPath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		distro     string
		onWindows  bool
		want       string
		wantErrSub string
	}{
		{name: "wsl-native goes over the UNC share", path: "/home/u/.observer/tok", distro: "Ubuntu-20.04", want: `\\wsl.localhost\Ubuntu-20.04\home\u\.observer\tok`},
		{name: "mnt path is already on windows", path: "/mnt/c/Users/a/observer.exe", distro: "Ubuntu-20.04", want: `C:\Users\a\observer.exe`},
		{name: "mnt drive root", path: "/mnt/d/", distro: "x", want: `D:\`},
		{name: "windows host passes through", path: `C:\bin\observer.exe`, onWindows: true, want: `C:\bin\observer.exe`},
		{name: "empty is an error", path: "", distro: "x", wantErrSub: "empty"},
		{name: "relative is an error", path: "bin/observer.exe", distro: "x", wantErrSub: "absolute"},
		{name: "no distro is an error, not a guess", path: "/home/u/tok", wantErrSub: "WSL_DISTRO_NAME"},
		{name: "mnt-lookalike dir is not a drive", path: "/mnt/cfoo/tok", distro: "Ubuntu-20.04", want: `\\wsl.localhost\Ubuntu-20.04\mnt\cfoo\tok`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WindowsPath(tc.path, tc.distro, tc.onWindows)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestProcessBridgeConnectAddr pins the listen→dial address mapping.
func TestProcessBridgeConnectAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "127.0.0.1:8823"},
		{"127.0.0.1:8823", "127.0.0.1:8823"},
		{"0.0.0.0:9000", "127.0.0.1:9000"},
		{"[::]:9000", "127.0.0.1:9000"},
		{":9000", "127.0.0.1:9000"},
		{"192.168.1.5:9000", "192.168.1.5:9000"},
		{"garbage", "garbage"},
	}
	for _, tc := range tests {
		if got := ConnectAddr(tc.in); got != tc.want {
			t.Errorf("ConnectAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestProcessBridgeTaskFrozenContract pins the two literals another surface
// depends on: the task name and the action shape.
func TestProcessBridgeTaskFrozenContract(t *testing.T) {
	if TaskName != "SuperBasedObserverETW" {
		t.Fatalf("task name is a frozen contract, got %q", TaskName)
	}
	cmd, cmdOnly := TaskCommand(`C:\o\observer.exe`, "127.0.0.1:8823", `C:\t\tok`)
	want := `schtasks.exe /Create /TN "SuperBasedObserverETW" /SC ONLOGON /RL HIGHEST ` +
		`/TR "'C:\o\observer.exe' process-bridge --etw --connect 127.0.0.1:8823 --token-file C:\t\tok"`
	if cmd != want {
		t.Fatalf("action shape drifted:\n got %s\nwant %s", cmd, want)
	}
	if cmdOnly {
		t.Fatal("a space-free command must be valid in BOTH cmd.exe and PowerShell")
	}
}

// TestProcessBridgeTaskCommandQuoting pins the SHELL PORTABILITY of the /TR
// value. Measured 2026-07-26 against a spaced program path: the backslash-
// escaped form `"\"<prog>\" args"` parses in cmd.exe but is REJECTED by
// PowerShell ("Invalid argument/option - 'C:\Program'"), while the
// single-quoted form parses in both and schtasks normalizes it to the correct
// quoted stored action. An unquoted program would store a broken action that
// runs C:\Program. This test is the guard against someone "fixing" it back.
func TestProcessBridgeTaskCommandQuoting(t *testing.T) {
	t.Run("spaced program path stays one token in both shells", func(t *testing.T) {
		cmd, cmdOnly := TaskCommand(`C:\Program Files\SuperBased\observer.exe`,
			"127.0.0.1:8823", `\\wsl.localhost\Ubuntu-20.04\home\u\.observer\process-bridge-token`)
		if !strings.Contains(cmd, `'C:\Program Files\SuperBased\observer.exe'`) {
			t.Fatalf("program path must be SINGLE-quoted: %s", cmd)
		}
		if strings.Contains(cmd, `\"`) {
			t.Fatalf("the backslash-escaped form is PowerShell-invalid — do not emit it: %s", cmd)
		}
		if cmdOnly {
			t.Fatal("a space-free token path must not be flagged cmd.exe-only")
		}
	})

	t.Run("spaced token path forces the cmd.exe-only form and says so", func(t *testing.T) {
		cmd, cmdOnly := TaskCommand(`C:\obs\observer.exe`, "127.0.0.1:8823",
			`C:\Users\John Doe\.observer\process-bridge-token`)
		if !cmdOnly {
			t.Fatalf("a token path with a space cannot be shell-portable: %s", cmd)
		}
		if !strings.Contains(cmd, `\"C:\Users\John Doe\.observer\process-bridge-token\"`) {
			t.Fatalf("a spaced token path must be quoted or argv splits it: %s", cmd)
		}
	})

	// The 2026-07-27 review's finding 1: an APOSTROPHE in the program path
	// (C:\Users\O'Brien\… — an ordinary, legal Windows path) ends the
	// single-quoted program early, so schtasks stores a program that is a
	// PREFIX of the real one and the task silently runs the wrong thing.
	t.Run("apostrophe in the program path must not end the quoted program", func(t *testing.T) {
		const exe = `C:\Users\O'Brien\observer.exe`
		cmd, cmdOnly := TaskCommand(exe, "127.0.0.1:8823", `C:\obs\token`)
		if strings.Contains(cmd, `'C:\Users\O'`) {
			t.Fatalf("the program was truncated at the apostrophe: %s", cmd)
		}
		if !strings.Contains(cmd, `\"`+exe+`\"`) {
			t.Fatalf("an apostrophe path must fall back to the escaped form: %s", cmd)
		}
		if !cmdOnly {
			t.Fatal("the escaped form is PowerShell-invalid, so it must be flagged cmd.exe-only")
		}
	})

	// An apostrophe in the TOKEN path needs no special form: the token is an
	// ARGUMENT, and a quote is not the grouping character there.
	t.Run("apostrophe in the token path stays bare", func(t *testing.T) {
		const tok = `C:\Users\O'Brien\.observer\process-bridge-token`
		cmd, cmdOnly := TaskCommand(`C:\obs\observer.exe`, "127.0.0.1:8823", tok)
		if !strings.Contains(cmd, "--token-file "+tok+`"`) {
			t.Fatalf("token path should be emitted bare: %s", cmd)
		}
		if cmdOnly {
			t.Fatalf("nothing here forces the cmd.exe-only form: %s", cmd)
		}
	})
}

// TestValidateActionValueRefusesUncomposableValues is the other half of the
// review's finding 1: the values the /TR grammar cannot carry AT ALL must
// produce a named refusal, never a mis-parsing command.
//
// The dangerous case is the double quote: it ends the /TR value early, so
// everything after it is read by schtasks as further arguments to ITSELF —
// under elevation. Windows file names cannot contain one, but the daemon
// resolves a LINUX path first (a Linux file name can), and config is not
// validated by the filesystem at all.
func TestValidateActionValueRefusesUncomposableValues(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		addrLike bool
		wantErr  string
	}{
		{"double quote ends the /TR value", `C:\a"b\observer.exe`, false, "double quote"},
		{"argument injection into elevated schtasks", `C:\a" /F "\observer.exe`, false, "double quote"},
		{"newline cannot survive one line", "C:\\a\nb\\observer.exe", false, "control character"},
		{"carriage return likewise", "C:\\a\rb\\observer.exe", false, "control character"},
		{"trailing backslash escapes the closing quote", `C:\obs\`, false, "ends with a backslash"},
		{"empty", "   ", false, "is empty"},
		{"space in the connect address splits it", "127.0.0.1 :8823", true, "whitespace"},
		{"a space in a PATH is fine — it is quoted", `C:\Program Files\o.exe`, false, ""},
		{"an apostrophe in a path is fine — it re-quotes", `C:\Users\O'Brien\o.exe`, false, ""},
		{"a tab is space-like and quoted", "C:\\a\tb\\o.exe", false, ""},
		{"a percent sign is legal, and noted rather than refused", `C:\100%\o.exe`, false, ""},
		{"a plain host:port", "127.0.0.1:8823", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateActionValue(tt.value, tt.addrLike)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateActionValue(%q) = %v, want nil", tt.value, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateActionValue(%q) = nil, want an error naming %q", tt.value, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

// TestPlanBlocksOnUncomposableValues proves the gate is WIRED — a predicate
// nothing calls is the failure mode this repo has shipped before.
func TestPlanBlocksOnUncomposableValues(t *testing.T) {
	base := Inputs{
		Enabled: true, ProcessEnabled: true, ETWEnabled: true,
		SchtasksPath: "schtasks.exe", Probe: ProbeAbsent, HostIsWindows: true,
		WindowsObserver: `C:\obs\observer.exe`, TokenPath: `C:\obs\token`,
		ListenAddr: "127.0.0.1:8823",
	}
	t.Run("a quote in the resolved binary path", func(t *testing.T) {
		in := base
		in.WindowsObserver = `C:\a" /F "\observer.exe`
		plan := PlanTask(in)
		if plan.Outcome != OutcomeBlocked {
			t.Fatalf("outcome = %v, want blocked; command was %q", plan.Outcome, plan.Command)
		}
		if plan.Command != "" || plan.SchtasksArgs != "" {
			t.Fatalf("a blocked plan must carry no command: %+v", plan)
		}
		if !strings.Contains(plan.Reason, "double quote") {
			t.Fatalf("reason must name the character: %q", plan.Reason)
		}
	})
	t.Run("whitespace in the connect address", func(t *testing.T) {
		in := base
		in.ListenAddr = "not an addr"
		plan := PlanTask(in)
		if plan.Outcome != OutcomeBlocked || !strings.Contains(plan.Reason, "whitespace") {
			t.Fatalf("outcome=%v reason=%q, want a blocked plan naming the whitespace", plan.Outcome, plan.Reason)
		}
	})
	t.Run("an ordinary path composes as before", func(t *testing.T) {
		if plan := PlanTask(base); plan.Outcome != OutcomeManual {
			t.Fatalf("outcome = %v, want manual: %+v", plan.Outcome, plan)
		}
	})
}

// TestComposedCommandNotes covers the two caveats that are properties of the
// composed line rather than of the host: the documented schtasks /TR length
// ceiling, and cmd.exe/Task-Scheduler %VAR% expansion.
func TestComposedCommandNotes(t *testing.T) {
	t.Run("a long /TR value is NOTED, not refused", func(t *testing.T) {
		long := `\\wsl.localhost\Ubuntu\home\` + strings.Repeat("averylongdirectoryname", 12) + `\observer.exe`
		in := Inputs{
			Enabled: true, ProcessEnabled: true, ETWEnabled: true,
			SchtasksPath: "schtasks.exe", Probe: ProbeAbsent, HostIsWindows: true,
			WindowsObserver: long, TokenPath: `C:\obs\token`, ListenAddr: "127.0.0.1:8823",
		}
		plan := PlanTask(in)
		if plan.Outcome != OutcomeManual {
			t.Fatalf("a long path must still produce a command (the limit is documented, not measured here): %+v", plan)
		}
		if !strings.Contains(strings.Join(plan.Notes, "\n"), "262-character") {
			t.Fatalf("notes must name the documented ceiling: %q", plan.Notes)
		}
	})
	t.Run("a percent sign is noted", func(t *testing.T) {
		args, _ := TaskArgs(`C:\100%\observer.exe`, "127.0.0.1:8823", `C:\obs\token`)
		notes := composedCommandNotes(args)
		if !strings.Contains(strings.Join(notes, "\n"), "%") {
			t.Fatalf("a %% in a path must be noted: %q", notes)
		}
	})
	t.Run("an ordinary command gets neither note", func(t *testing.T) {
		args, _ := TaskArgs(`C:\obs\observer.exe`, "127.0.0.1:8823", `C:\obs\token`)
		if notes := composedCommandNotes(args); len(notes) != 0 {
			t.Fatalf("no note is owed for an ordinary command: %q", notes)
		}
	})
}

// TestResolveInputsRequiresBothSwitches closes the review's finding 7: the
// elevated feed needs the MASTER switch too. With [observer.process].enabled
// false, runProcessObserver returns before the accept listener is constructed,
// so a task registered in that state reconnects forever against nothing.
func TestResolveInputsRequiresBothSwitches(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name           string
		master, etw    bool
		wantEnabled    bool
		wantPlanIsSkip bool
	}{
		{"both on", true, true, true, false},
		{"master off, etw on", false, true, false, true},
		{"master on, etw off", true, false, false, true},
		{"both off", false, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeTaskEnv(schtasksNotFound, errors.New("exit status 1"))
			cfg := etwConfig(true)
			cfg.Enabled = tt.master
			cfg.ETW.Enabled = tt.etw

			in := ResolveInputs(ctx, cfg, "/home/u/.observer", f.env)
			if in.Enabled != tt.wantEnabled {
				t.Errorf("Enabled = %v, want %v", in.Enabled, tt.wantEnabled)
			}
			if in.ProcessEnabled != tt.master || in.ETWEnabled != tt.etw {
				t.Errorf("ProcessEnabled=%v ETWEnabled=%v, want %v/%v",
					in.ProcessEnabled, in.ETWEnabled, tt.master, tt.etw)
			}
			if got := PlanTask(in).Outcome == OutcomeSkip; got != tt.wantPlanIsSkip {
				t.Errorf("plan-is-skip = %v, want %v", got, tt.wantPlanIsSkip)
			}
			// A disabled feed must not pay for the probe either.
			if !tt.wantEnabled && len(f.schtasksCalls) != 0 {
				t.Errorf("probed schtasks %d times for a feed that cannot run", len(f.schtasksCalls))
			}
		})
	}
}

// TestResolveProcessBridgeTaskInputsCarriesTheConfiguredPath proves the hint
// is plumbed from config/env through the I/O layer — the planner can only be
// honest if the caller hands it the distinction.
func TestResolveProcessBridgeTaskInputsCarriesTheConfiguredPath(t *testing.T) {
	ctx := context.Background()

	t.Run("config key wins and is carried verbatim", func(t *testing.T) {
		f := newFakeTaskEnv(schtasksNotFound, errors.New("exit status 1"))
		f.env.ResolveObserver = func(string) (string, bool) { f.observerCalls++; return "", false }
		f.env.ObserverEnvPath = "/mnt/c/env/observer.exe"
		cfg := etwConfig(true)
		cfg.WindowsBinaryPath = "/mnt/c/nope/observer.exe"

		in := ResolveInputs(ctx, cfg, "/home/u/.observer", f.env)
		if in.WindowsObserverHint != "/mnt/c/nope/observer.exe" ||
			in.WindowsObserverHintSource != "[observer.process].windows_binary_path" {
			t.Fatalf("hint = %q from %q, want the config key", in.WindowsObserverHint, in.WindowsObserverHintSource)
		}
		plan := PlanTask(in)
		if plan.Outcome != OutcomeBlocked || !strings.Contains(plan.Reason, "/mnt/c/nope/observer.exe") {
			t.Fatalf("blocked reason must name the configured path, got %+v", plan)
		}
	})

	t.Run("env var is the fallback knob", func(t *testing.T) {
		f := newFakeTaskEnv(schtasksNotFound, errors.New("exit status 1"))
		f.env.ResolveObserver = func(string) (string, bool) { f.observerCalls++; return "", false }
		f.env.ObserverEnvPath = "/mnt/c/env/observer.exe"

		in := ResolveInputs(ctx, etwConfig(true), "/home/u/.observer", f.env)
		if in.WindowsObserverHint != "/mnt/c/env/observer.exe" ||
			in.WindowsObserverHintSource != "$OBSERVER_WINDOWS_BINARY" {
			t.Fatalf("hint = %q from %q, want the env var", in.WindowsObserverHint, in.WindowsObserverHintSource)
		}
	})

	t.Run("nothing configured carries no hint", func(t *testing.T) {
		f := newFakeTaskEnv(schtasksNotFound, errors.New("exit status 1"))
		f.env.ResolveObserver = func(string) (string, bool) { f.observerCalls++; return "", false }

		in := ResolveInputs(ctx, etwConfig(true), "/home/u/.observer", f.env)
		if in.WindowsObserverHint != "" {
			t.Fatalf("hint = %q, want empty when neither knob is set", in.WindowsObserverHint)
		}
		if r := PlanTask(in).Reason; !strings.Contains(r, "no Windows observer.exe is configured") {
			t.Fatalf("reason = %q, want the not-configured wording", r)
		}
	})
}

// --- the elevation broker (dashboard plan §E3) ---

// TestTaskArgsIsTheOnlyOwnerOfTheQuoting pins that TaskCommand adds nothing
// but the program name. If the two ever diverge, the line an operator pastes
// and the line the dashboard's broker runs stop being the same command — which
// is the whole reason the tail was split out.
func TestTaskArgsIsTheOnlyOwnerOfTheQuoting(t *testing.T) {
	cases := []struct{ name, exe, addr, token string }{
		{"plain", `C:\obs\observer.exe`, "127.0.0.1:8823", `C:\obs\tok`},
		{"spaced program", `C:\Program Files\obs\observer.exe`, "127.0.0.1:8823", `C:\obs\tok`},
		{"spaced token forces the cmd-only form", `C:\obs\observer.exe`, "127.0.0.1:8823", `C:\Program Files\tok`},
		{"unc token", `C:\obs\observer.exe`, "127.0.0.1:8823", `\\wsl.localhost\Ubuntu\home\u\.observer\tok`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, argsOnly := TaskArgs(tc.exe, tc.addr, tc.token)
			cmd, cmdOnly := TaskCommand(tc.exe, tc.addr, tc.token)
			if argsOnly != cmdOnly {
				t.Errorf("cmdOnly disagrees: TaskArgs=%v TaskCommand=%v", argsOnly, cmdOnly)
			}
			if want := SchtasksExe + " " + args; cmd != want {
				t.Errorf("TaskCommand = %q, want %q — it must be TaskArgs plus the program and nothing else", cmd, want)
			}
			if strings.HasPrefix(args, SchtasksExe) {
				t.Errorf("TaskArgs must be the TAIL only, got %q", args)
			}
		})
	}
}

// TestPlanCarriesTheCommandTail pins Plan.SchtasksArgs: exactly the tail of
// Plan.Command wherever a command exists, and EMPTY wherever one does not —
// so a caller cannot spawn an elevated helper off a plan that has no command.
func TestPlanCarriesTheCommandTail(t *testing.T) {
	withCommand := Inputs{
		Enabled: true, SchtasksPath: `C:\Windows\System32\schtasks.exe`,
		WindowsObserver: `/mnt/c/obs/observer.exe`, ListenAddr: "127.0.0.1:8823",
		TokenPath: "/home/u/.observer/tok", Distro: "Ubuntu",
	}
	for _, probe := range []Probe{ProbeAbsent, ProbeUnknown} {
		in := withCommand
		in.Probe = probe
		plan := PlanTask(in)
		if plan.SchtasksArgs == "" {
			t.Fatalf("probe %v: SchtasksArgs is empty but Command is %q", probe, plan.Command)
		}
		if got := SchtasksExe + " " + plan.SchtasksArgs; got != plan.Command {
			t.Errorf("probe %v: Command %q is not %q", probe, plan.Command, got)
		}
	}

	commandless := map[string]Inputs{
		"skip":    {Enabled: false},
		"present": {Enabled: true, SchtasksPath: `x`, Probe: ProbePresent},
		"blocked": {Enabled: true, SchtasksPath: `x`, Probe: ProbeAbsent},
	}
	for name, in := range commandless {
		plan := PlanTask(in)
		if plan.Command != "" || plan.SchtasksArgs != "" {
			t.Errorf("%s: must carry no command, got Command=%q SchtasksArgs=%q", name, plan.Command, plan.SchtasksArgs)
		}
	}
}

// TestElevatedRegisterArgvShape pins the broker argv: a fixed 5-element
// powershell invocation whose script elevates through UAC, embeds the planner's
// args as ONE PowerShell literal, and handles a dismissed prompt.
func TestElevatedRegisterArgvShape(t *testing.T) {
	args, _ := TaskArgs(`C:\Program Files\obs\observer.exe`, "127.0.0.1:8823", `\\wsl.localhost\Ubuntu\home\u\tok`)
	argv := ElevatedRegisterArgv(args)

	want := []string{PowerShellExe, "-NoProfile", "-NonInteractive", "-Command"}
	if len(argv) != 5 {
		t.Fatalf("argv = %#v, want 5 elements", argv)
	}
	for i, w := range want {
		if argv[i] != w {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], w)
		}
	}
	script := argv[4]

	if !strings.Contains(script, "-Verb RunAs") {
		t.Error("the script must elevate with -Verb RunAs — that IS the consent step")
	}
	if !strings.Contains(script, "-Wait -PassThru") {
		t.Error("the script must wait for the elevated child and read its exit code")
	}
	// -RedirectStandard* is a parameter-set conflict with -Verb, and would also
	// imply we can relay an elevated child's console. We cannot.
	if strings.Contains(script, "-RedirectStandard") {
		t.Error("the script must not claim to relay an elevated child's output")
	}
	// A dismissed UAC prompt throws; without a catch the PTY would show a raw
	// PowerShell stack instead of an explanation, and the card would not know
	// to fall back to the copyable command.
	if !strings.Contains(script, "catch {") || !strings.Contains(script, "exit 3") {
		t.Error("the script must catch a dismissed/failed elevation and exit non-zero")
	}
	// Measured: `try {…}; catch {…}` is a PowerShell PARSE error. The statement
	// separator must stay a newline.
	if strings.Contains(script, "}\ncatch") == false {
		t.Error("catch must follow the try block on the next LINE — a `;` between them is a parse error")
	}
	// The planner's args must appear as ONE PowerShell single-quoted literal
	// (with ' doubled), never split into an array — Start-Process joins an
	// array with bare spaces and would destroy the /TR grouping.
	lit := "'" + strings.ReplaceAll(args, "'", "''") + "'"
	if !strings.Contains(script, "-ArgumentList "+lit) {
		t.Errorf("the args must be one single-quoted literal.\nwant substring: -ArgumentList %s\nscript: %s", lit, script)
	}
	// And nothing may have mangled the measured /TR quoting on the way in.
	if !strings.Contains(args, `/TR "'C:\Program Files\obs\observer.exe'`) {
		t.Errorf("the single-quoted /TR form did not survive: %s", args)
	}
}

// TestPSQuoteDoublesSingleQuotes pins the one escape a PowerShell verbatim
// literal needs. The planner's own /TR form contains single quotes, so getting
// this wrong would terminate the literal early and change the command.
func TestPSQuoteDoublesSingleQuotes(t *testing.T) {
	cases := map[string]string{
		``:               `''`,
		`plain`:          `'plain'`,
		`it's`:           `'it''s'`,
		`'both'`:         `'''both'''`,
		`C:\a\b "q" 'r'`: `'C:\a\b "q" ''r'''`,
	}
	for in, want := range cases {
		if got := psQuote(in); got != want {
			t.Errorf("psQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
