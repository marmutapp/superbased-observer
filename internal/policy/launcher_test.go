package policy

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// Regression suite for the launcher-wrapper bypass of R-155
// (docs/security.md H5) and the `-ec` payload bypass
// found beside it (H6).
//
// METHOD NOTE, and the reason this file is shaped the way it is.
// docs/plans/process-obs-etw-windows-parity-plan-2026-07-26.md §W4.6
// records that R-155 shipped a CRITICAL bypass TWICE, each time with a
// passing test — because the test asserted the ONE literal string its
// own doc comment named, so it defended that string and nothing else.
// Every table here is therefore written against the CLASS: one row per
// SHAPE, each row run in all three dialects, plus an equivalence
// property (TestLauncher_WrappedVerdictEqualsDirect) that holds for
// inner commands the table never enumerates.

// launcherEvalAllDialects evaluates one command line in ModeEnforce in
// every dialect and returns the per-dialect verdicts.
func launcherEvalAllDialects(t *testing.T, line, home, root string) map[Dialect]Verdict {
	t.Helper()
	out := map[Dialect]Verdict{}
	for _, d := range []Dialect{DialectPosix, DialectPowerShell, DialectCmd} {
		e, err := New(Config{Mode: ModeEnforce, Home: home})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		out[d] = e.Evaluate(Event{
			Kind: KindShellExec, ActionType: "run_command",
			Target: line, Dialect: d,
			Cwd: root, ProjectRoot: root, SessionID: "s1",
			Now: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
		})
	}
	return out
}

// launcherCase is one command shape and the verdict it must produce in
// EVERY dialect. A shape whose verdict is dialect-dependent is a bug
// in this area, not a row to special-case: the three dialects are
// three spellings of the same Windows command, and the two bypasses in
// §W4.6 both hid in a dialect that behaved differently.
type launcherCase struct {
	name string
	cmd  string
	// rule is the expected rule ID; "" means no rule may fire.
	rule string
	// why documents rows that deliberately do NOT deny.
	why string
}

const (
	launcherHome = `C:\Users\u`
	launcherRoot = `C:\Users\u\proj`
)

func runLauncherCases(t *testing.T, cases []launcherCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for d, v := range launcherEvalAllDialects(t, tc.cmd, launcherHome, launcherRoot) {
				switch {
				case tc.rule == "" && v.RuleID != "":
					t.Errorf("[%s] %s\n  got %s/%s (%q), want no rule hit%s",
						d, tc.cmd, v.Decision, v.RuleID, v.Reason, launcherWhy(tc.why))
				case tc.rule != "" && v.RuleID != tc.rule:
					t.Errorf("[%s] %s\n  got %s/%s (%q), want %s/deny",
						d, tc.cmd, v.Decision, v.RuleID, v.Reason, tc.rule)
				case tc.rule != "" && v.Decision != DecisionDeny:
					t.Errorf("[%s] %s\n  got decision %s, want deny", d, tc.cmd, v.Decision)
				}
			}
		})
	}
}

func launcherWhy(why string) string {
	if why == "" {
		return ""
	}
	return " (" + why + ")"
}

// psEncode renders text the way `powershell -EncodedCommand` takes it:
// base64 over UTF-16LE.
func psEncode(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(u)*2)
	for _, r := range u {
		b = append(b, byte(r), byte(r>>8))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// TestLauncher_PersistenceThroughWrappers is the bypass table. Every
// row was measured ALLOW in ModeEnforce, in all three dialects, before
// the fix.
//
// The wrapper shapes are enumerated because the launcher's own syntax
// is where the resolution can go wrong: named vs positional binding,
// parameter abbreviations, the `-Name:Value` spelling, array literals,
// aliases, `--` terminators, and wrappers nested inside wrappers.
func TestLauncher_PersistenceThroughWrappers(t *testing.T) {
	t.Parallel()
	const evilTR = `C:\evil\payload.exe`
	runLauncherCases(t, []launcherCase{
		// --- CONTROLS: the shapes that always denied, pinned so a
		// regression in the direct path is not mistaken for a
		// launcher problem. ---
		{
			name: "control/bare schtasks", rule: "R-155",
			cmd: `schtasks /Create /TN evil /TR ` + evilTR + ` /SC ONLOGON /RL HIGHEST /F`,
		},
		{
			name: "control/cmd slash-c", rule: "R-155",
			cmd: `cmd.exe /c schtasks /Create /TN evil /TR ` + evilTR + ` /SC ONLOGON`,
		},
		{
			name: "control/powershell -Command", rule: "R-155",
			cmd: `powershell.exe -NoProfile -Command "schtasks /Create /TN evil /TR ` + evilTR + ` /SC ONLOGON"`,
		},
		{name: "control/sh -c crontab", rule: "R-155", cmd: `sh -c "crontab evil.cron"`},

		// --- Start-Process: the headline bypass and its spellings ---
		{
			name: "start-process/inside powershell -Command", rule: "R-155",
			cmd: `powershell.exe -NoProfile -Command "Start-Process schtasks.exe -Verb RunAs -Wait ` +
				`-ArgumentList '/Create','/TN','evil','/TR','` + evilTR + `','/SC','ONLOGON'"`,
		},
		{
			name: "start-process/positional filepath", rule: "R-155",
			cmd: `Start-Process schtasks.exe -Verb RunAs -ArgumentList '/Create','/TN','evil','/TR','` + evilTR + `'`,
		},
		{
			name: "start-process/named -FilePath", rule: "R-155",
			cmd: `Start-Process -FilePath schtasks.exe -ArgumentList '/Create','/TN','evil'`,
		},
		{
			name: "start-process/alias saps", rule: "R-155",
			cmd: `saps schtasks.exe -ArgumentList '/Create','/TN','evil'`,
		},
		{
			name: "start-process/alias start", rule: "R-155",
			cmd: `start schtasks.exe -ArgumentList '/Create','/TN','evil'`,
		},
		{
			name: "start-process/crontab arm", rule: "R-155",
			cmd: `powershell -Command "Start-Process crontab -ArgumentList 'evil.cron'"`,
		},
		{
			name: "start-process/systemctl arm", rule: "R-155",
			cmd: `Start-Process systemctl -ArgumentList 'enable','evil.service'`,
		},
		{
			name: "start-process/run key via reg.exe", rule: "R-155",
			cmd: `Start-Process reg.exe -ArgumentList 'add','HKCU\Software\Microsoft\Windows\CurrentVersion\Run','/v','x','/d','evil.exe'`,
		},
		{
			name: "start-process/wraps cmd.exe which wraps schtasks", rule: "R-155",
			cmd: `Start-Process cmd.exe -ArgumentList '/c','schtasks /Create /TN evil /TR ` + evilTR + `'`,
		},
		{
			name: "start-process/nested in itself", rule: "R-155",
			cmd: `Start-Process Start-Process -ArgumentList 'schtasks','/Create'`,
		},
		{
			name: "start-process/wraps wsl", rule: "R-155",
			cmd: `Start-Process wsl.exe -ArgumentList '-d','Ubuntu','--','crontab','evil.cron'`,
		},

		// --- Start-Process: parameter-binding games. PowerShell
		// PREFIX-BINDS, so none of these may be recognised by a
		// string comparison against a parameter name. ---
		{
			name: "params/abbreviated -fi -arg", rule: "R-155",
			cmd: `Start-Process -fi schtasks.exe -arg '/Create','/TN','evil'`,
		},
		{
			name: "params/abbreviated -wo consumes its value", rule: "R-155",
			cmd: `Start-Process -wo C:\x -fi schtasks.exe -arg '/Create'`,
		},
		{
			name: "params/full -WorkingDirectory consumes its value", rule: "R-155",
			cmd: `Start-Process -WorkingDirectory C:\x schtasks.exe -ArgumentList '/Create','/TN','e'`,
		},
		{
			name: "params/colon spelling", rule: "R-155",
			cmd: `Start-Process -FilePath:schtasks.exe -ArgumentList:'/Create'`,
		},
		{
			name: "params/-Verb wins over -Verbose on exact match", rule: "R-155",
			cmd: `Start-Process -Verb RunAs schtasks.exe -ArgumentList '/Create'`,
		},
		{
			name: "params/ambiguous -w must not hide the program", rule: "R-155",
			cmd: `Start-Process -w C:\x schtasks.exe -ArgumentList '/Create'`,
			why: "-w binds to nothing (Wait/WindowStyle/WorkingDirectory); both readings are analyzed",
		},
		{
			name: "params/unknown valued parameter must not hide the program", rule: "R-155",
			cmd: `Start-Process -NotARealParam C:\x schtasks.exe -ArgumentList '/Create'`,
		},
		{
			name: "params/-Args alias is not a prefix of -ArgumentList", rule: "R-155",
			cmd: `Start-Process schtasks.exe -Args '/Create','/TN','evil'`,
		},
		{
			name: "params/array literal with a variable element", rule: "R-155",
			cmd: `Start-Process schtasks.exe -ArgumentList @('/Create','/TN',$e)`,
		},
		{
			name: "params/single string argument list", rule: "R-155",
			cmd: `Start-Process schtasks.exe -ArgumentList '/Create /TN evil /TR ` + evilTR + `'`,
		},
		{
			name: "params/array spelled with spaces after commas", rule: "R-155",
			cmd: `Start-Process schtasks.exe -ArgumentList '/Create', '/TN', 'evil'`,
		},
		{
			name: "params/positional argument list", rule: "R-155",
			cmd: `Start-Process schtasks.exe '/Create /TN evil'`,
		},

		// --- cmd.exe START ---
		{
			name: "cmd-start/empty title placeholder", rule: "R-155",
			cmd: `start "" schtasks /Create /TN evil /TR ` + evilTR,
		},
		{
			name: "cmd-start/background switch", rule: "R-155",
			cmd: `start /b schtasks /Create /TN evil /TR ` + evilTR,
		},
		{
			name: "cmd-start/valued switch consumes its value", rule: "R-155",
			cmd: `start /d C:\tmp schtasks /Create /TN evil`,
		},
		{
			name: "cmd-start/quoted window title", rule: "R-155",
			cmd: `start "My Title" schtasks /Create /TN evil`,
			why: "quoting is lost at lex time, so both operand readings are analyzed",
		},

		// --- wsl.exe ---
		{
			name: "wsl/double-dash terminator", rule: "R-155",
			cmd: `wsl.exe -d Ubuntu -- schtasks /Create /TN evil /TR ` + evilTR,
		},
		{name: "wsl/no terminator", rule: "R-155", cmd: `wsl -u root crontab evil.cron`},
		{
			name: "wsl/long option with a separate value", rule: "R-155",
			cmd: `wsl --distribution Ubuntu -- systemctl enable evil.service`,
		},
		{
			name: "wsl/long option with an inline value", rule: "R-155",
			cmd: `wsl --distribution=Ubuntu crontab evil.cron`,
		},
		{name: "wsl/--exec", rule: "R-155", cmd: `wsl -e crontab evil.cron`},
		{
			name: "wsl/carries sudo through", rule: "R-155",
			cmd: `wsl.exe -d Ubuntu sudo systemctl enable evil.service`,
		},

		// --- the sudo/cmd-dialect asymmetry (same finding) ---
		{
			name: "sudo/systemctl enable in every dialect", rule: "R-155",
			cmd: `sudo systemctl enable evil.service`,
		},

		// --- -EncodedCommand and every documented abbreviation ---
		{
			name: "encoded/-EncodedCommand", rule: "R-155",
			cmd: `powershell -EncodedCommand ` + psEncode(`Start-Process schtasks.exe -ArgumentList '/Create','/TN','evil'`),
		},
		{
			name: "encoded/-e", rule: "R-155",
			cmd: `powershell -e ` + psEncode(`schtasks /Create /TN evil`),
		},
		{
			name: "encoded/-en", rule: "R-155",
			cmd: `powershell -en ` + psEncode(`schtasks /Create /TN evil`),
		},
		{
			name: "encoded/-enc", rule: "R-155",
			cmd: `powershell -enc ` + psEncode(`schtasks /Create /TN evil`),
		},
		{
			name: "encoded/-encod", rule: "R-155",
			cmd: `powershell -encod ` + psEncode(`schtasks /Create /TN evil`),
		},
		{
			name: "encoded/-ec is an alias, not a prefix", rule: "R-155",
			cmd: `powershell -ec ` + psEncode(`schtasks /Create /TN evil`),
		},
		{
			name: "encoded/-EncodedCommand with -NoProfile first", rule: "R-155",
			cmd: `powershell.exe -NoProfile -NonInteractive -ec ` + psEncode(`crontab evil.cron`),
		},
		{
			name: "command/-Com abbreviation", rule: "R-155",
			cmd: `powershell -Com "Start-Process schtasks.exe -ArgumentList '/Create','/TN','evil'"`,
		},
		{
			name: "command/-c abbreviation", rule: "R-155",
			cmd: `powershell -c "Start-Process schtasks.exe -ArgumentList '/Create','/TN','evil'"`,
		},
	})
}

// TestLauncher_BenignCommandsUnaffected pins the other half: a
// launcher is not itself suspicious, and resolving one must not invent
// a rule hit. A guard that denies `Start-Process notepad.exe` is a
// guard the operator turns off.
func TestLauncher_BenignCommandsUnaffected(t *testing.T) {
	t.Parallel()
	runLauncherCases(t, []launcherCase{
		{name: "benign/start-process bare", cmd: `Start-Process notepad.exe`},
		{name: "benign/start-process with args", cmd: `Start-Process notepad.exe -ArgumentList 'foo.txt'`},
		{name: "benign/start-process non-persistence filepath", cmd: `Start-Process -FilePath C:\bin\tool.exe -ArgumentList 'x'`},
		{name: "benign/start-process elevated non-persistence", cmd: `Start-Process -Verb RunAs C:\bin\tool.exe -ArgumentList 'install'`},
		{name: "benign/cmd start", cmd: `start notepad.exe`},
		{name: "benign/cmd start with title", cmd: `start "Build" npm.cmd -ArgumentList 'run','build'`},
		{name: "benign/wsl interactive", cmd: `wsl`},
		{name: "benign/wsl command", cmd: `wsl -d Ubuntu ls -la`},
		{name: "benign/wsl exec", cmd: `wsl.exe -d Ubuntu -- git status`},
		{name: "benign/schtasks query is not a create", cmd: `schtasks /Query /TN evil`},
		{name: "benign/start-process schtasks query", cmd: `Start-Process schtasks.exe -ArgumentList '/Query','/TN','x'`},
	})
}

// TestLauncher_StaticallyUnknowableResiduals states, rather than
// hides, what this resolution CANNOT see. Each row is a shape whose
// launched command is built at RUNTIME from a variable, so no static
// analyzer can name the program or its arguments — the same documented
// approximation ParseCommand already makes for `rm -rf $(pwd)`.
//
// They are pinned as ALLOW so that a future change which happens to
// close one is a loud test failure that gets the residual list
// updated, and so that nobody reads the deny table above as a claim
// that the class is fully closed.
func TestLauncher_StaticallyUnknowableResiduals(t *testing.T) {
	t.Parallel()
	runLauncherCases(t, []launcherCase{
		{
			name: "residual/argument list is a variable",
			cmd:  `Start-Process schtasks.exe -ArgumentList $args`,
			why:  "the arguments exist only at runtime; /Create is not in the text",
		},
		{
			name: "residual/splatted parameter hashtable",
			cmd:  `Start-Process @params`,
			why:  "both the program and the arguments are in the hashtable",
		},
		{
			name: "residual/splatted argument array",
			cmd:  `Start-Process schtasks.exe @moreArgs`,
			why:  "the program is visible but the /Create that makes it persistence is not",
		},
	})
}

// TestLauncher_WrappedVerdictEqualsDirect is the property test, and it
// is the one that defends the CLASS rather than the rows above: for
// every (wrapper, inner command) pair, wrapping must not change the
// verdict. It covers inner commands the bypass table never enumerates,
// and it fails for a wrapper that resolves the program to the wrong
// token just as loudly as for one that resolves nothing.
func TestLauncher_WrappedVerdictEqualsDirect(t *testing.T) {
	t.Parallel()
	// Each wrapper renders an inner (program, args) pair. comma is the
	// PowerShell array spelling; space is the plain one.
	wrappers := []struct {
		name string
		// render builds the wrapped line from the program and its
		// arguments.
		render func(prog string, args []string) string
		// directDialect, when set, is the dialect the DIRECT form must
		// be evaluated in — wsl always runs a POSIX command line.
		directDialect Dialect
	}{
		{name: "start-process positional", render: func(p string, a []string) string {
			return `Start-Process ` + p + ` -ArgumentList '` + strings.Join(a, `','`) + `'`
		}},
		{name: "start-process named", render: func(p string, a []string) string {
			return `Start-Process -FilePath ` + p + ` -ArgumentList '` + strings.Join(a, `','`) + `'`
		}},
		{name: "start-process elevated in powershell -Command", render: func(p string, a []string) string {
			return `powershell.exe -NoProfile -Command "Start-Process ` + p +
				` -Verb RunAs -Wait -ArgumentList '` + strings.Join(a, `','`) + `'"`
		}},
		{name: "saps alias", render: func(p string, a []string) string {
			return `saps ` + p + ` -ArgumentList '` + strings.Join(a, `','`) + `'`
		}},
		{name: "cmd start", render: func(p string, a []string) string {
			return `start /b ` + p + ` ` + strings.Join(a, " ")
		}},
		{name: "wsl", directDialect: DialectPosix, render: func(p string, a []string) string {
			return `wsl.exe -d Ubuntu -- ` + p + ` ` + strings.Join(a, " ")
		}},
	}
	inners := []struct {
		name string
		prog string
		args []string
	}{
		{name: "schtasks create", prog: "schtasks", args: []string{"/Create", "/TN", "evil", "/TR", `C:\evil\p.exe`}},
		{name: "schtasks query", prog: "schtasks", args: []string{"/Query", "/TN", "evil"}},
		{name: "crontab install", prog: "crontab", args: []string{"evil.cron"}},
		{name: "crontab list", prog: "crontab", args: []string{"-l"}},
		{name: "systemctl enable", prog: "systemctl", args: []string{"enable", "evil.service"}},
		{name: "systemctl status", prog: "systemctl", args: []string{"status", "evil.service"}},
		// The registry rows use FORWARD slashes (runKeyToken accepts
		// both) so the comparison isolates the wrapper. A backslashed
		// key is confounded by a pre-existing and unrelated lexer
		// fact: `\` is the POSIX escape character, so an UNQUOTED
		// `HKCU\Software\…` loses its separators under POSIX lexing
		// while the same key inside a PowerShell array literal keeps
		// them. That difference is quoting, not wrapping, and pinning
		// it here would test the lexer instead of this change.
		{name: "reg add run key", prog: "reg", args: []string{
			"add", "HKCU/Software/Microsoft/Windows/CurrentVersion/Run", "/v", "x", "/d", "evil.exe",
		}},
		{name: "reg query run key", prog: "reg", args: []string{
			"query", "HKCU/Software/Microsoft/Windows/CurrentVersion/Run",
		}},
		{name: "git force push", prog: "git", args: []string{"push", "--force", "origin", "main"}},
		{name: "git status", prog: "git", args: []string{"status"}},
	}
	for _, w := range wrappers {
		for _, in := range inners {
			t.Run(w.name+"/"+in.name, func(t *testing.T) {
				t.Parallel()
				direct := launcherEvalAllDialects(t, in.prog+" "+strings.Join(in.args, " "), launcherHome, launcherRoot)
				wrapped := launcherEvalAllDialects(t, w.render(in.prog, in.args), launcherHome, launcherRoot)
				for _, d := range []Dialect{DialectPosix, DialectPowerShell, DialectCmd} {
					dd := d
					if w.directDialect != "" {
						dd = w.directDialect
					}
					want, got := direct[dd], wrapped[d]
					if want.RuleID != got.RuleID || want.Decision != got.Decision {
						t.Errorf("[%s] wrapping changed the verdict\n  direct  (%s): %s/%s\n  wrapped (%s): %s/%s\n  line: %s",
							d, dd, want.Decision, want.RuleID, d, got.Decision, got.RuleID, w.render(in.prog, in.args))
					}
				}
			})
		}
	}
}

// TestLauncher_ETWCarveOutNotWidened pins BOTH halves of the
// carve-out's interaction with this change, which the
// etw-dashboard plan §E4 asked for explicitly ("prefer NOT to widen
// it"):
//
//   - the line `observer init` prints — a BARE schtasks registration —
//     is still allowed, so an agent helping the operator run it is not
//     hard-denied; and
//   - the same registration behind ANY launcher is denied, including
//     the `powershell -Command Start-Process … -Verb RunAs` shape the
//     dashboard's own broker will spawn. That spawn does not pass
//     through this engine (SetupSpec argv is built server-side), so
//     denying it here costs the product feature nothing.
func TestLauncher_ETWCarveOutNotWidened(t *testing.T) {
	t.Parallel()
	const action = `'C:\Program Files\SuperBased\observer.exe' process-bridge --etw ` +
		`--connect 127.0.0.1:8823 --token-file C:\Users\u\.observer\etw-token`
	bare := `schtasks.exe /Create /TN "SuperBasedObserverETW" /SC ONLOGON /RL HIGHEST /TR "` + action + `"`

	t.Run("bare registration still allowed", func(t *testing.T) {
		t.Parallel()
		for d, v := range launcherEvalAllDialects(t, bare, launcherHome, launcherRoot) {
			if v.Decision != DecisionAllow || v.RuleID != "" {
				t.Errorf("[%s] got %s/%s (%q), want allow with no rule hit", d, v.Decision, v.RuleID, v.Reason)
			}
		}
	})

	wrapped := []launcherCase{
		{
			name: "wrapped/powershell Start-Process -Verb RunAs (the dashboard broker shape)", rule: "R-155",
			cmd: `powershell.exe -NoProfile -Command "Start-Process schtasks.exe -Verb RunAs -Wait ` +
				`-ArgumentList '/Create','/TN','SuperBasedObserverETW','/SC','ONLOGON','/RL','HIGHEST','/TR','` + action + `'"`,
		},
		{
			name: "wrapped/start-process direct", rule: "R-155",
			cmd: `Start-Process schtasks.exe -ArgumentList '/Create','/TN','SuperBasedObserverETW','/TR','` + action + `'`,
		},
		{
			name: "wrapped/cmd start", rule: "R-155",
			cmd: `start "" schtasks.exe /Create /TN SuperBasedObserverETW /SC ONLOGON /RL HIGHEST /TR "` + action + `"`,
		},
		{
			name: "wrapped/wsl", rule: "R-155",
			cmd: `wsl.exe -d Ubuntu -- schtasks.exe /Create /TN SuperBasedObserverETW /TR "` + action + `"`,
		},
	}
	runLauncherCases(t, wrapped)
}

// --- unit tests for the resolution primitives --------------------------

// TestPSResolveParam pins the binding rules the whole Start-Process
// resolution rests on. The exact-beats-prefix row is the load-bearing
// one: without it `-Verb` is ambiguous with `-Verbose` and an elevated
// registration resolves its program to `RunAs`.
func TestPSResolveParam(t *testing.T) {
	t.Parallel()
	cases := []struct {
		arg  string
		want string // "" = binds to nothing
		note string
	}{
		{arg: "FilePath", want: "filepath"},
		{arg: "filepath", want: "filepath", note: "case-insensitive"},
		{arg: "FILEPATH", want: "filepath"},
		{arg: "fi", want: "filepath", note: "unique prefix"},
		{arg: "f", want: "filepath"},
		{arg: "ArgumentList", want: "argumentlist"},
		{arg: "argu", want: "argumentlist"},
		{arg: "arg", want: "argumentlist", note: "ArgumentList and its Args alias agree"},
		{arg: "Args", want: "args", note: "an alias, not a prefix of argumentlist"},
		{arg: "Verb", want: "verb", note: "exact beats the Verbose prefix"},
		{arg: "Verbose", want: "verbose"},
		{arg: "verbo", want: "verbose"},
		{arg: "v", want: "", note: "verb valued vs verbose switch disagree"},
		{arg: "w", want: "", note: "wait switch vs windowstyle workingdirectory valued"},
		{arg: "wo", want: "workingdirectory"},
		{arg: "wa", want: "", note: "wait switch vs warningaction warningvariable valued"},
		// H5 round 4: an abbreviation matching SEVERAL DIFFERENT
		// parameters binds to nothing, the way PowerShell refuses it.
		// Agreeing on valued-ness is NOT enough — the old rule bound
		// each of these to whichever row happened to come first while
		// its comment claimed PowerShell-equivalent behaviour.
		{arg: "e", want: "", note: "environment erroraction errorvariable are three parameters"},
		{arg: "env", want: "environment", note: "unique once it is longer"},
		{arg: "i", want: "", note: "informationaction informationvariable"},
		{arg: "o", want: "", note: "outbuffer outvariable"},
		{arg: "r", want: "", note: "the three redirectstandard*"},
		{arg: "war", want: "", note: "warningaction warningvariable"},
		{arg: "c", want: "", note: "credential vs the confirm shouldprocess switch"},
		{arg: "p", want: "", note: "passthru pipelinevariable progressaction"},
		{arg: "cred", want: "credential"},
		{arg: "progressaction", want: "progressaction", note: "common parameter, 7.4+"},
		{arg: "wh", want: "whatif", note: "shouldprocess switch"},
		{arg: "conf", want: "confirm"},
		{arg: "NotAParameter", want: ""},
		{arg: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.arg+"/"+tc.note, func(t *testing.T) {
			t.Parallel()
			b := psResolveParam(tc.arg, psStartProcessParams)
			if tc.want == "" {
				if b.bound {
					t.Fatalf("-%s bound to %q, want no binding", tc.arg, b.param.name)
				}
				return
			}
			if !b.bound || b.param.name != tc.want {
				t.Fatalf("-%s bound to %q/%v, want %q", tc.arg, b.param.name, b.bound, tc.want)
			}
			if wantExact := strings.EqualFold(tc.arg, tc.want); b.exact != wantExact {
				t.Fatalf("-%s exact=%v, want %v", tc.arg, b.exact, wantExact)
			}
		})
	}
}

// TestPSSplitArguments pins the array spellings that reach the
// resolver after lexing has stripped the quotes.
func TestPSSplitArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "comma array", in: []string{"/Create,/TN,evil"}, want: []string{"/Create", "/TN", "evil"}},
		{name: "single string", in: []string{"/Create /TN evil"}, want: []string{"/Create", "/TN", "evil"}},
		{name: "spaced commas", in: []string{"/Create,", "/TN,", "evil"}, want: []string{"/Create", "/TN", "evil"}},
		{name: "array literal", in: []string{"@(/Create,/TN,$e)"}, want: []string{"/Create", "/TN", "$e"}},
		{name: "quoted leftovers", in: []string{"'/Create','/TN'"}, want: []string{"/Create", "/TN"}},
		{name: "empty", in: []string{",,"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := psSplitArguments(tc.in)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWSLInnerArgv pins where the Linux command begins for each of
// wsl.exe's option spellings.
func TestWSLInnerArgv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		want []string
	}{
		{name: "terminator", argv: []string{"wsl.exe", "-d", "Ubuntu", "--", "crontab", "e"}, want: []string{"crontab", "e"}},
		{name: "no terminator", argv: []string{"wsl", "-u", "root", "crontab", "e"}, want: []string{"crontab", "e"}},
		{name: "long option", argv: []string{"wsl", "--distribution", "Ubuntu", "crontab"}, want: []string{"crontab"}},
		{name: "inline value", argv: []string{"wsl", "--distribution=Ubuntu", "crontab"}, want: []string{"crontab"}},
		{name: "exec", argv: []string{"wsl", "-e", "crontab", "e"}, want: []string{"crontab", "e"}},
		{name: "switch only", argv: []string{"wsl", "--system", "crontab"}, want: []string{"crontab"}},
		{name: "no command", argv: []string{"wsl"}, want: nil},
		{name: "options but no command", argv: []string{"wsl", "-d", "Ubuntu"}, want: nil},
		{name: "dangling terminator", argv: []string{"wsl", "--"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := wslInnerArgv(tc.argv)
			got := res.candidates
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("got %q, want no candidate", got)
				}
				if res.named {
					t.Fatalf("no candidate but named=true: a wsl that names no command must not fail closed")
				}
				return
			}
			if len(got) != 1 || strings.Join(got[0], "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %q, want one candidate %q", got, tc.want)
			}
			if !res.named {
				t.Fatalf("resolved %q but named=false", got[0])
			}
		})
	}
}

// TestCmdStartCandidates pins that BOTH operand readings are produced
// for cmd's START, which is what closes the quoted-window-title hole
// without a lexer change.
func TestCmdStartCandidates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
		want [][]string
	}{
		{
			name: "title and program", argv: []string{"start", "My Title", "schtasks", "/Create"},
			want: [][]string{{"My Title", "schtasks", "/Create"}, {"schtasks", "/Create"}},
		},
		{
			name: "empty title", argv: []string{"start", "", "schtasks", "/Create"},
			want: [][]string{{"schtasks", "/Create"}},
		},
		{
			name: "switch then program", argv: []string{"start", "/b", "schtasks", "/Create"},
			want: [][]string{{"schtasks", "/Create"}},
		},
		{
			name: "valued switch", argv: []string{"start", "/d", `C:\tmp`, "schtasks", "/Create"},
			want: [][]string{{"schtasks", "/Create"}},
		},
		{name: "nothing to launch", argv: []string{"start"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := cmdStartCandidates(tc.argv)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if strings.Join(got[i], "|") != strings.Join(tc.want[i], "|") {
					t.Fatalf("candidate %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestLauncherWrapperNamesRecorded pins the fact the ETW carve-out
// depends on: a launched unit carries its launcher on Wrappers, so
// safeObserverETWTaskRegistration can fail closed on it. A launcher
// added to the table without this property would silently re-widen the
// carve-out.
func TestLauncherWrapperNamesRecorded(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		`Start-Process schtasks.exe -ArgumentList '/Create'`: "start-process",
		`saps schtasks.exe -ArgumentList '/Create'`:          "start-process",
		`start /b schtasks /Create`:                          "start-process",
		`wsl.exe -d Ubuntu -- crontab e`:                     "wsl",
	}
	for line, want := range cases {
		t.Run(line, func(t *testing.T) {
			t.Parallel()
			cmds := ParseCommand(line, DialectPowerShell)
			found := false
			for i := range cmds {
				if cmds[i].Depth == 0 {
					continue
				}
				found = true
				if !viaLauncher(&cmds[i]) {
					t.Errorf("unit %q has wrappers %q, want a launcher name", cmds[i].Base, cmds[i].Wrappers)
				}
				if !hasWrapper(&cmds[i], want) {
					t.Errorf("unit %q has wrappers %q, want %q", cmds[i].Base, cmds[i].Wrappers, want)
				}
			}
			if !found {
				t.Fatalf("no launched unit was produced for %q", line)
			}
		})
	}
	t.Run("a directly typed command is not via a launcher", func(t *testing.T) {
		t.Parallel()
		for _, c := range ParseCommand(`schtasks /Create /TN x`, DialectCmd) {
			if viaLauncher(&c) {
				t.Fatalf("direct command reported as launcher-wrapped: %q", c.Wrappers)
			}
		}
	})
}

// TestLauncherDepthBound pins that launcher nesting cannot recurse
// without bound: the depth limit turns the innermost payload opaque
// rather than looping.
func TestLauncherDepthBound(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("Start-Process ", 12) + "schtasks"
	cmds := ParseCommand(line, DialectPowerShell)
	for _, c := range cmds {
		if c.Depth > maxUnwrapDepth {
			t.Fatalf("unit %q parsed at depth %d, past the %d limit", c.Base, c.Depth, maxUnwrapDepth)
		}
	}
}

// --- H5 ROUND 2: the class, not the enumeration ------------------------
//
// docs/security.md H5 shipped a critical bypass TWICE more after the
// tables above were written, and the reason both times was the same
// one §W4.6 names: THE TEST ASSERTED THE ENUMERATION IN THE AUTHOR'S
// HEAD, NEVER THE CLASS. The tables above test `-e`/`-ec`/`-c` for a
// BARE `powershell`, and they test `start /b` for a DASH-FREE inner
// command — and the hole was exactly the cross-product neither row
// visits: `start powershell -c "<persistence>"`, `start /b crontab
// -e`. Adding an argument to a denied command made it ALLOW.
//
// So the defence below is generated, not listed. It walks
// {launcher spelling} × {launcher switch/parameter form} × {inner-host
// spelling} × {inner command}, and asserts the ONE property that
// matters over combinations no table enumerates:
//
//	WRAPPING NEVER TURNS A DENY INTO AN ALLOW.
//
// A row that needs a literal is a REGRESSION row below, not a
// substitute for this.

// launcherWrap is one way of wrapping a command line in a launcher.
type launcherWrap struct {
	name string
	// wrap renders the launcher invocation around an inner line.
	wrap func(inner string) string
	// plainInner marks a wrapper whose inner line is a QUOTED
	// operand, so it can only be combined with inner spellings that
	// carry no quotes of their own (nested quoting is a lexer
	// question, not a policy one).
	plainInner bool
}

// launcherWraps enumerates the launcher SHAPES: the two grammars the
// word `start` can name, their aliases, `/`-switches (valued and not),
// the quoted window title, named cmdlet parameters (exact, abbreviated
// and unbindable), wsl's option spellings, and the privilege wrappers.
func launcherWraps() []launcherWrap {
	f := func(name, format string) launcherWrap {
		return launcherWrap{name: name, wrap: func(in string) string {
			return strings.Replace(format, "%INNER%", in, 1)
		}}
	}
	out := []launcherWrap{
		{name: "direct", wrap: func(in string) string { return in }},
		f("start", "start %INNER%"),
		f("start/b", "start /b %INNER%"),
		f("start/min", "start /min %INNER%"),
		f("start/wait", "start /wait %INNER%"),
		f("start/d valued", `start /d C:\x %INNER%`),
		f("start/affinity valued", "start /affinity 1 %INNER%"),
		f("start/node valued", "start /node 0 %INNER%"),
		f("start title", `start "t" %INNER%`),
		f("start empty title", `start "" %INNER%`),
		f("start switch+title", `start /b "t" %INNER%`),
		f("Start-Process", "Start-Process %INNER%"),
		f("start-process lower", "start-process %INNER%"),
		f("saps", "saps %INNER%"),
		f("Start-Process -Verb", "Start-Process -Verb RunAs %INNER%"),
		f("Start-Process -WorkingDirectory", `Start-Process -WorkingDirectory C:\x %INNER%`),
		f("Start-Process -NoNewWindow", "Start-Process -NoNewWindow %INNER%"),
		f("Start-Process -WindowStyle", "Start-Process -WindowStyle Hidden %INNER%"),
		f("Start-Process -Wait", "Start-Process -Wait %INNER%"),
		f("Start-Process -w ambiguous", `Start-Process -w C:\x %INNER%`),
		f("Start-Process -verb:inline", "Start-Process -Verb:RunAs %INNER%"),
		f("wsl --", "wsl -- %INNER%"),
		f("wsl -d --", "wsl -d Ubuntu -- %INNER%"),
		f("wsl -u", "wsl -u root %INNER%"),
		f("wsl -e", "wsl -e %INNER%"),
		f("wsl --distribution=", "wsl --distribution=Ubuntu %INNER%"),
		f("sudo", "sudo %INNER%"),
		f("sudo -u", "sudo -u root %INNER%"),
		f("gsudo", "gsudo %INNER%"),
		f("nested start+wsl", "start wsl -- %INNER%"),
		f("nested wsl+start", "wsl -- start %INNER%"),
	}
	out = append(
		out,
		launcherWrap{name: "runas", plainInner: true, wrap: func(in string) string {
			return `runas /user:admin "` + in + `"`
		}},
		launcherWrap{name: "runas /savecred", plainInner: true, wrap: func(in string) string {
			return `runas /savecred /user:admin "` + in + `"`
		}},
	)
	return out
}

// launcherHost is one way of spelling "run this command line through
// an interpreter". These are the spellings the bypass hid in: the
// abbreviations of PowerShell's -Command and -EncodedCommand, the
// POSIX shells' -c, and cmd's three run switches.
type launcherHost struct {
	name string
	// host renders an inner command line as a hosted invocation.
	host func(inner string) string
	// quoted marks a spelling that introduces quotes of its own.
	quoted bool
}

func launcherHosts() []launcherHost {
	q := func(name, format string) launcherHost {
		return launcherHost{name: name, quoted: true, host: func(in string) string {
			return strings.Replace(format, "%INNER%", in, 1)
		}}
	}
	enc := func(name, prefix, flag string) launcherHost {
		return launcherHost{name: name, host: func(in string) string {
			return prefix + " " + flag + " " + psEncode(in)
		}}
	}
	return []launcherHost{
		{name: "bare", host: func(in string) string { return in }},
		q("powershell -c", `powershell -c "%INNER%"`),
		q("powershell -Command", `powershell -Command "%INNER%"`),
		q("powershell -com", `powershell -com "%INNER%"`),
		q("pwsh -Command", `pwsh -Command "%INNER%"`),
		// H5 round 4: -CommandWithArgs was absent from the host
		// vocabulary entirely, so every spelling of it ALLOWED. The
		// abbreviations are here because the payload rows are matched
		// by PREFIX and by documented alias, and both paths have shipped
		// a bypass (-ec in H6, -cwa here).
		q("pwsh -CommandWithArgs", `pwsh -CommandWithArgs "%INNER%"`),
		q("pwsh -cwa", `pwsh -cwa "%INNER%"`),
		q("powershell -cwa", `powershell -cwa "%INNER%"`),
		q("pwsh -commandwith", `pwsh -commandwith "%INNER%"`),
		// A SWITCH row, a VALUED row and a non-prefix VALUED alias
		// standing between the executable and its payload. Each is a
		// place the walk can lose the payload by consuming a token it
		// should not (or failing to consume one it should).
		q("powershell -NoProfile -Command", `powershell -NoProfile -Command "%INNER%"`),
		q("powershell -ExecutionPolicy -Command", `powershell -ExecutionPolicy Bypass -Command "%INNER%"`),
		q("powershell -wd -Command", `powershell -wd C:\x -Command "%INNER%"`),
		q("pwsh -nop -noni -cwa", `pwsh -nop -noni -cwa "%INNER%"`),
		enc("powershell -e", "powershell", "-e"),
		enc("powershell -ec", "powershell", "-ec"),
		enc("powershell -enc", "powershell", "-enc"),
		enc("powershell -EncodedCommand", "powershell", "-EncodedCommand"),
		enc("pwsh -nop -e", "pwsh -nop", "-e"),
		enc("powershell -ea then -e", `powershell -ea AAA=`, "-e"),
		// .NET's Convert.FromBase64String IGNORES whitespace; Go's
		// decoder rejects it. A quoted base64 blob with spaces is one
		// argv token that PowerShell runs and we used to drop. Marked
		// quoted: it brings quotes of its own, so like every other
		// quoted host it is not combined with the plainInner wrappers
		// whose operand is itself one quoted string (runas re-splits
		// that operand on whitespace by design — a documented
		// approximation, and a nested-quoting question rather than a
		// policy one).
		{name: "powershell -e spaced b64", quoted: true, host: func(in string) string {
			return `powershell -e "` + spaceOutBase64(psEncode(in)) + `"`
		}},
		q("sh -c", `sh -c "%INNER%"`),
		q("bash -c", `bash -c "%INNER%"`),
		{name: "cmd /c", host: func(in string) string { return "cmd /c " + in }},
		{name: "cmd /k", host: func(in string) string { return "cmd /k " + in }},
		{name: "cmd /r", host: func(in string) string { return "cmd /r " + in }},
	}
}

// spaceOutBase64 injects the whitespace .NET's base64 decoder ignores.
func spaceOutBase64(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%8 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// launcherDenyInners are inner commands that MUST deny when typed
// directly. The property is asserted relative to that, so a rule
// change that stopped denying one of them fails here loudly rather
// than silently weakening the property to "nothing denies".
func launcherDenyInners() []struct{ name, cmd string } {
	return []struct{ name, cmd string }{
		{"crontab -e", `crontab -e`},
		{"schtasks /Create", `schtasks /Create /TN evil /TR C:\evil.exe /SC ONLOGON`},
		{"systemctl enable", `systemctl enable evil`},
	}
}

// launcherEngines builds one engine per dialect, reused across the
// whole cross-product (the property test evaluates thousands of
// lines; a per-line engine would dominate its runtime).
func launcherEngines(t *testing.T) map[Dialect]*Engine {
	t.Helper()
	m := map[Dialect]*Engine{}
	for _, d := range []Dialect{DialectPosix, DialectPowerShell, DialectCmd} {
		e, err := New(Config{Mode: ModeEnforce, Home: launcherHome})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		m[d] = e
	}
	return m
}

func launcherEvalWith(e *Engine, line string, d Dialect) Verdict {
	return e.Evaluate(Event{
		Kind: KindShellExec, ActionType: "run_command",
		Target: line, Dialect: d,
		Cwd: launcherRoot, ProjectRoot: launcherRoot, SessionID: "s1",
		Now: time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	})
}

// TestLauncher_WrappingNeverRelaxes is THE class test: the full
// cross-product of launcher shapes, inner-host spellings and inner
// commands, asserting that no combination of wrappers turns a
// directly-denied command into an allow, in any dialect.
func TestLauncher_WrappingNeverRelaxes(t *testing.T) {
	t.Parallel()
	engines := launcherEngines(t)
	dialects := []Dialect{DialectPosix, DialectPowerShell, DialectCmd}
	combos, checked := 0, 0
	for _, inner := range launcherDenyInners() {
		// The premise: the bare command denies in every dialect.
		for _, d := range dialects {
			if v := launcherEvalWith(engines[d], inner.cmd, d); v.Decision != DecisionDeny {
				t.Fatalf("premise broken: [%s] %s is %s/%s, want deny",
					d, inner.cmd, v.Decision, v.RuleID)
			}
		}
		for _, h := range launcherHosts() {
			hosted := h.host(inner.cmd)
			for _, w := range launcherWraps() {
				if w.plainInner && h.quoted {
					continue // nested quoting is a lexer question
				}
				line := w.wrap(hosted)
				combos++
				for _, d := range dialects {
					checked++
					v := launcherEvalWith(engines[d], line, d)
					if v.Decision != DecisionDeny {
						t.Errorf("[%s] wrapping RELAXED a deny\n  wrapper %s / host %s / inner %s\n  line: %s\n  got:  %s/%s",
							d, w.name, h.name, inner.name, line, v.Decision, v.RuleID)
					}
				}
			}
		}
	}
	t.Logf("cross-product: %d command lines, %d verdicts", combos, checked)
}

// TestLauncher_H5Round2Repros are the exact lines an independent
// adversarial review measured as ALLOW in ModeEnforce, in all three
// dialects, after the FIRST H5 fix had shipped and its own mutation
// proofs had passed. Each is paired with the control that already
// denied — the pairs are the evidence that a READING was being
// deleted rather than a rule being missed.
func TestLauncher_H5Round2Repros(t *testing.T) {
	t.Parallel()
	const evil = `schtasks /Create /TN evil /TR C:\evil.exe /SC ONLOGON`
	runLauncherCases(t, []launcherCase{
		// --- CRITICAL: `bound` suppressed the cmd-START readings. ---
		{name: "start powershell -c", rule: "R-155", cmd: `start powershell -c "` + evil + `"`},
		{name: "control/start powershell -Command", rule: "R-155", cmd: `start powershell -Command "` + evil + `"`},
		{name: "start powershell -e", rule: "R-155", cmd: `start powershell -e ` + psEncode(evil)},
		{name: "control/start powershell -enc", rule: "R-155", cmd: `start powershell -enc ` + psEncode(evil)},
		{name: "start /b crontab -e", rule: "R-155", cmd: `start /b crontab -e`},
		{name: "control/start /b crontab", rule: "R-155", cmd: `start /b crontab`},
		{name: "start sh -c", rule: "R-155", cmd: `start sh -c "crontab -e"`},
		{name: "control/sh -c", rule: "R-155", cmd: `sh -c "crontab -e"`},
		{name: "start title powershell -c", rule: "R-155", cmd: `start "t" powershell -c "` + evil + `"`},
		{name: "start /min crontab -e", rule: "R-155", cmd: `start /min crontab -e`},
		{name: "start /d crontab -e", rule: "R-155", cmd: `start /d C:\x crontab -e`},
		{name: "start /affinity crontab -e", rule: "R-155", cmd: `start /affinity 1 crontab -e`},

		// --- HIGH: the no-candidate path failed OPEN. A trailing
		// comma is legal in an NTFS directory name, so psCollectValue's
		// comma-run join ate the only positional and the resolver
		// produced nothing at all. ---
		{
			name: "trailing-comma working directory", rule: "R-155",
			cmd: `Start-Process -WorkingDirectory 'C:\temp,' schtasks.exe -ArgumentList '/Create','/TN','evil','/TR','C:\evil.exe'`,
		},
		{
			name: "control/no trailing comma", rule: "R-155",
			cmd: `Start-Process -WorkingDirectory 'C:\temp' schtasks.exe -ArgumentList '/Create','/TN','evil','/TR','C:\evil.exe'`,
		},

		// --- HIGH: the depth bound failed OPEN. Five levels denied,
		// six ALLOWED, because the limit recorded the payload on a
		// field no rule reads. ---
		{name: "wsl x5 at the limit", rule: "R-155", cmd: strings.Repeat("wsl -- ", 5) + "crontab -e"},
		{name: "wsl x6 past the limit", rule: "R-157", cmd: strings.Repeat("wsl -- ", 6) + "crontab -e"},
		{name: "cmd /c x7 past the limit", rule: "R-157", cmd: strings.Repeat(`cmd /c `, 7) + "crontab"},

		// --- MEDIUM launcher-coverage gaps closed in this round. ---
		{name: "runas", rule: "R-155", cmd: `runas /user:admin "crontab -e"`},
		{name: "runas /savecred", rule: "R-155", cmd: `runas /savecred /user:admin "crontab -e"`},
		{name: "gsudo", rule: "R-155", cmd: `gsudo crontab -e`},
		{name: "cmd /r", rule: "R-155", cmd: `cmd /r crontab -e`},
		{name: "control/cmd /c", rule: "R-155", cmd: `cmd /c crontab -e`},
		{name: "Invoke-Expression", rule: "R-155", cmd: `Invoke-Expression "crontab -e"`},
		{name: "iex alias", rule: "R-155", cmd: `iex "crontab -e"`},
	})
}

// TestLauncher_H5Round4Repros are the lines a FOURTH independent
// adversarial review measured against the real engine after round 3
// had shipped the never-suppress design and a 4,050-verdict property
// test. The design held; the VOCABULARY it operates on did not, which
// is why the fixes are table rows and the tests are cross-product
// vocabulary rather than more literals (docs/security.md H5 round 4).
func TestLauncher_H5Round4Repros(t *testing.T) {
	t.Parallel()
	const evil = `schtasks /Create /TN evil /TR C:\evil.exe`
	runLauncherCases(t, []launcherCase{
		// --- CRITICAL: -CommandWithArgs was not in the host
		// vocabulary, so pwsh's third command-text parameter carried
		// an unanalysed payload in every dialect. ---
		{name: "pwsh -CommandWithArgs", rule: "R-155", cmd: `pwsh -CommandWithArgs "` + evil + `"`},
		{name: "pwsh -cwa", rule: "R-155", cmd: `pwsh -cwa "` + evil + `"`},
		{name: "powershell -cwa", rule: "R-155", cmd: `powershell -cwa "` + evil + `"`},
		{name: "control/pwsh -Command", rule: "R-155", cmd: `pwsh -Command "` + evil + `"`},

		// --- The vocabulary audit that came with it: a switch, a
		// valued row and a non-prefix valued alias between the
		// executable and its payload; and the .NET whitespace-
		// tolerant base64 Go's decoder rejects. ---
		{name: "host switch before payload", rule: "R-155", cmd: `pwsh -nop -noni -cwa "` + evil + `"`},
		{name: "host valued before payload", rule: "R-155", cmd: `powershell -ExecutionPolicy Bypass -Command "` + evil + `"`},
		{name: "host valued alias before payload", rule: "R-155", cmd: `powershell -wd C:\x -Command "` + evil + `"`},
		{
			name: "valued row must not swallow -Command", rule: "R-155",
			cmd: `powershell -ConfigurationName -f -Command "` + evil + `"`,
			why: "-ConfigurationName eats the -f; the -Command after it still runs",
		},
		{name: "encoded with whitespace", rule: "R-155", cmd: `powershell -e "` + spaceOutBase64(psEncode(evil)) + `"`},

		{
			name: "substitution below the cap", rule: "R-155",
			cmd: `cmd /c cmd /c sh -c echo$(crontab)`,
			why: "three levels: the substitution is still analysed",
		},

		// --- MEDIUM: an ambiguous cmdlet abbreviation used to bind to
		// whichever row came first. It now binds to NOTHING, which
		// routes it into the unknown-parameter cross-product — so the
		// deny survives, on the reading where schtasks.exe is the
		// program. Allowing instead (because PowerShell would refuse
		// the line) would make "spell a parameter this table is
		// missing" a way to buy an allow. ---
		{name: "ambiguous -e", rule: "R-155", cmd: `Start-Process -e Stop schtasks.exe -ArgumentList '/Create'`},
		{name: "ambiguous -i", rule: "R-155", cmd: `Start-Process -i Stop schtasks.exe -ArgumentList '/Create'`},
		{name: "ambiguous -o", rule: "R-155", cmd: `Start-Process -o Stop schtasks.exe -ArgumentList '/Create'`},
		{name: "ambiguous -r", rule: "R-155", cmd: `Start-Process -r Stop schtasks.exe -ArgumentList '/Create'`},
		{name: "ambiguous -war", rule: "R-155", cmd: `Start-Process -war Stop schtasks.exe -ArgumentList '/Create'`},
		{name: "ambiguous -c", rule: "R-155", cmd: `Start-Process -c Stop schtasks.exe -ArgumentList '/Create'`},
		{
			name: "-arg is one parameter, not two", rule: "R-155",
			cmd: `Start-Process schtasks.exe -arg '/Create','/TN','evil','/TR','C:\e.exe'`,
			why: "-Args aliases -ArgumentList, so the abbreviation is NOT ambiguous",
		},

		// --- The reviewer's #7, and it was live: a candidate whose
		// program word normalizes to "" produced a unit with Base ""
		// that matched every rule exactly never. ---
		{name: "wsl empty operand", rule: "R-155", cmd: `wsl '' crontab -e`},
		{name: "wsl -- empty operand", rule: "R-155", cmd: `wsl -- '' crontab -e`},
		{name: "start empty operand", rule: "R-155", cmd: `start '' schtasks /Create /TN evil /TR C:\evil.exe`},

		// --- ACCEPTED COLLATERAL, kept deliberately (see the
		// TestLauncher_NoFalsePositives note): six nested `cmd /c`
		// exceeds the depth bound and denies with the reason attached,
		// even around `echo hello`. The bound is not raised to make a
		// contrived line pass. ---
		{
			name: "benign line past the depth bound", rule: "R-157",
			cmd: strings.Repeat("cmd /c ", 6) + "echo hello",
			why: "fail-closed depth bound; six interpreter levels is not a real developer command",
		},
		{
			name: "control/five levels still analysed", rule: "",
			cmd: strings.Repeat("cmd /c ", 5) + "echo hello",
			why: "the bound is at six, and everything under it analyses normally",
		},
	})
}

// TestSubstitutionsFailClosed covers the round-4 HIGH and the wider
// fail-open that understanding it exposed. Two defects, one family:
//
//   - parseDepth stopped stripping command substitutions AT
//     maxUnwrapDepth without recording anything, so the nested command
//     vanished with no R-157;
//   - and it never stripped them under the CMD dialect at all. cmd.exe
//     has no `$( )` of its own, but a cmd-tagged line routinely
//     carries a command for another shell, and the payload re-join
//     does not preserve quoting — so `cmd /c sh -c "$(crontab -e)"`
//     tore into `$(crontab` + `-e` and ALLOWED under cmd while denying
//     under the other two, at ANY depth (measured 2026-07-27).
//
// The assertion is "denies in every dialect", not "denies with rule X
// in every dialect": below the cap the substitution is analysed and
// R-155 fires on its merits, at the cap the honest answer is R-157's
// "this was not analysed", and which one a line reaches is a property
// of the line. Pretending otherwise would be the overclaim this round
// exists to remove. What must NOT differ by dialect, and used to, is
// the DECISION.
func TestSubstitutionsFailClosed(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, cmd string }{
		{"cmd chain, no space in the substitution", `cmd /c cmd /c cmd /c cmd /c sh -c echo$(crontab)`},
		{"cmd chain, space in the substitution", `cmd /c cmd /c cmd /c cmd /c sh -c "$(crontab -e)"`},
		{"one cmd level", `cmd /c sh -c "$(crontab -e)"`},
		{"one cmd level, embedded", `cmd /c bash -c "echo $(crontab -e)"`},
		{"powershell host, quoted inner", `powershell -Command "sh -c \"$(crontab -e)\""`},
		{"backtick spelling", "cmd /c cmd /c cmd /c cmd /c sh -c echo`crontab -e`"},
		// Substitutions NESTED past the cap: the only way to reach
		// maxUnwrapDepth now that every level strips. The innermost
		// command is never analysed, so R-157 is the honest verdict —
		// and it is a verdict, not a silent allow.
		{"nested substitutions past the cap", `$($($($($($(crontab -e)))))) `},
		{"launcher chain to the cap", strings.Repeat("wsl -- ", 5) + "sh -c echo$(crontab)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for d, v := range launcherEvalAllDialects(t, tc.cmd, launcherHome, launcherRoot) {
				if v.Decision != DecisionDeny {
					t.Errorf("[%s] %s\n  got %s/%s, want deny (R-155 where the substitution is analysed, R-157 where it is only reached at the cap)",
						d, tc.cmd, v.Decision, v.RuleID)
				}
				if v.RuleID != "R-155" && v.RuleID != "R-157" {
					t.Errorf("[%s] %s\n  got rule %s, want R-155 or R-157", d, tc.cmd, v.RuleID)
				}
			}
		})
	}
}

// TestLauncher_NoFalsePositives re-pins the benign shapes the review
// measured as clean. A fix that closes the bypass by denying more
// broadly is not a fix; these must keep allowing.
func TestLauncher_NoFalsePositives(t *testing.T) {
	t.Parallel()
	runLauncherCases(t, []launcherCase{
		{name: "start dot", cmd: `start .`, why: "opens the working directory"},
		{name: "start url", cmd: `start http://example.com`},
		{name: "start /wait notepad", cmd: `start /wait notepad.exe`},
		{name: "start title notepad", cmd: `start "editor" notepad.exe`},
		{name: "sudo -u root ls", cmd: `sudo -u root ls`},
		{name: "gsudo ls", cmd: `gsudo ls`},
		{name: "runas ls", cmd: `runas /user:admin "ls -la"`},
		{name: "wsl listing", cmd: `wsl -l -v`},
		{name: "wsl interactive", cmd: `wsl -d Ubuntu`},
		{name: "Start-Process explorer", cmd: `Start-Process explorer.exe "C:\Users\u\Documents"`},
		{name: "Start-Process notepad -Wait", cmd: `Start-Process notepad.exe -Wait -WindowStyle Hidden`},
		{name: "crontab -l bare", cmd: `crontab -l`, why: "read-only listing"},
		{name: "crontab -l wrapped", cmd: `Start-Process crontab -ArgumentList '-l'`, why: "safe pattern survives the wrapper"},
		{name: "crontab -l via start", cmd: `start crontab -l`},
		{name: "crontab -l via wsl", cmd: `wsl -- crontab -l`},
		{name: "crontab -l via runas", cmd: `runas /user:admin "crontab -l"`},
		{name: "cmd /c dir", cmd: `cmd /c dir`},
		{name: "cmd /r echo", cmd: `cmd /r echo hi`},
		{name: "iex benign", cmd: `iex "Get-ChildItem"`},
		{name: "nested but shallow", cmd: strings.Repeat("wsl -- ", 4) + "ls"},

		// --- H5 round 4: -File TERMINATES the host-parameter walk.
		// Everything after it is the SCRIPT's argv, so a script's own
		// -Command argument is not host command text. The classifier
		// used to keep scanning and deny a command pwsh never runs. ---
		{
			name: "pwsh -File with a script -Command argument",
			cmd:  `pwsh -File .\analyze.ps1 -Command "schtasks /Create"`,
			why:  "the -Command belongs to analyze.ps1, not to pwsh",
		},
		{
			name: "pwsh -f abbreviation terminates too",
			cmd:  `pwsh -f .\analyze.ps1 -Command "schtasks /Create"`,
		},
		{
			name: "powershell -File after host parameters",
			cmd:  `powershell -NoProfile -ExecutionPolicy Bypass -File .\build.ps1 -Command "crontab -e"`,
		},
		{
			name: "pwsh -File is a documented gap, not a deny",
			cmd:  `pwsh -File .\deploy.ps1`,
			why:  "gap F1: a script file this pure package cannot read",
		},
	})
}

// TestUnwrapLauncher_FailsClosed exercises the four exits that do not
// analyse the launched command. Each used to `return nil` with nothing
// recorded, which made a launcher we could NOT read strictly safer
// than one we could. They are driven through stub resolvers because
// the shipped resolvers are now specifically built not to reach them —
// the point is that the backstop holds if a future narrowing does.
func TestUnwrapLauncher_FailsClosed(t *testing.T) {
	t.Parallel()
	stub := func(res launchResult) launcherSpec {
		return launcherSpec{name: "stub", resolve: func([]string) launchResult { return res }}
	}
	newCmd := func() *Command {
		return &Command{Base: "stub", Argv: []string{"stub", "x"}, Raw: "stub x", Dialect: DialectPosix}
	}
	t.Run("named but unresolvable denies", func(t *testing.T) {
		t.Parallel()
		c := newCmd()
		if got := unwrapLauncher(c, stub(launchResult{named: true}), newParseState()); got != nil {
			t.Fatalf("got %d units, want none", len(got))
		}
		if c.Unanalyzed != unanalyzedLauncherTarget {
			t.Fatalf("Unanalyzed = %q, want %q", c.Unanalyzed, unanalyzedLauncherTarget)
		}
	})
	t.Run("nothing named stays silent", func(t *testing.T) {
		t.Parallel()
		c := newCmd()
		unwrapLauncher(c, stub(launchResult{}), newParseState())
		if c.Unanalyzed != "" {
			t.Fatalf("Unanalyzed = %q, want empty: an interactive wsl names no command and must not deny", c.Unanalyzed)
		}
	})
	t.Run("depth limit denies", func(t *testing.T) {
		t.Parallel()
		c := newCmd()
		st := newParseState()
		st.depth = maxUnwrapDepth
		unwrapLauncher(c, stub(launchResult{candidates: [][]string{{"crontab"}}, named: true}), st)
		if c.Unanalyzed != unanalyzedNestingLimit {
			t.Fatalf("Unanalyzed = %q, want %q", c.Unanalyzed, unanalyzedNestingLimit)
		}
	})
	t.Run("fan-out bound denies", func(t *testing.T) {
		t.Parallel()
		var many [][]string
		for i := 0; i < maxLauncherCandidates+2; i++ {
			many = append(many, []string{"prog" + string(rune('a'+i))})
		}
		c := newCmd()
		got := unwrapLauncher(c, stub(launchResult{candidates: many, named: true}), newParseState())
		if len(got) != maxLauncherCandidates {
			t.Fatalf("parsed %d units, want %d", len(got), maxLauncherCandidates)
		}
		if c.Unanalyzed != unanalyzedLauncherFanOut {
			t.Fatalf("Unanalyzed = %q, want %q", c.Unanalyzed, unanalyzedLauncherFanOut)
		}
	})
	t.Run("expansion budget denies", func(t *testing.T) {
		t.Parallel()
		c := newCmd()
		spent := 0
		st := parseState{budget: &spent}
		unwrapLauncher(c, stub(launchResult{candidates: [][]string{{"crontab"}}, named: true}), st)
		if c.Unanalyzed != unanalyzedExpansionBudget {
			t.Fatalf("Unanalyzed = %q, want %q", c.Unanalyzed, unanalyzedExpansionBudget)
		}
	})
}

// TestParseExpansionIsBounded pins that a crafted chain of ambiguous
// launchers cannot fan out without limit. Unwrapping is
// MULTIPLICATIVE (candidates^depth), the guard hook's watchdog fails
// OPEN on timeout, and an unbounded parse is therefore a bypass with
// extra steps.
func TestParseExpansionIsBounded(t *testing.T) {
	t.Parallel()
	// Maximally ambiguous at every level: an unbindable parameter, an
	// abbreviated valued one, and a comma run.
	line := strings.Repeat(`start /b -w C:\x, -c q `, 8) + "crontab -e"
	for _, d := range []Dialect{DialectPosix, DialectPowerShell, DialectCmd} {
		cmds := ParseCommand(line, d)
		if len(cmds) > maxExpansionUnits+8 {
			t.Fatalf("[%s] parsed %d units, want <= %d", d, len(cmds), maxExpansionUnits+8)
		}
		for _, c := range cmds {
			if c.Depth > maxUnwrapDepth {
				t.Fatalf("[%s] unit %q at depth %d", d, c.Base, c.Depth)
			}
		}
	}
}

// BenchmarkLauncherWorstCase measures the crafted-chain cost the
// budget exists to bound.
func BenchmarkLauncherWorstCase(b *testing.B) {
	line := strings.Repeat(`start /b -w C:\x, -c q `, 8) + "crontab -e"
	for i := 0; i < b.N; i++ {
		ParseCommand(line, DialectCmd)
	}
}

// TestUnwrapPayload_FailsClosed is the same backstop for the NON-
// launcher nesting path (`cmd /c`, `bash -c`, `eval`, PowerShell
// -Command). It used to record the text on Payload and return nil,
// which reads as preservation but is a fail-OPEN, because no rule
// reads Payload.
func TestUnwrapPayload_FailsClosed(t *testing.T) {
	t.Parallel()
	t.Run("depth limit", func(t *testing.T) {
		t.Parallel()
		c := &Command{Base: "sh", Argv: []string{"sh", "-c", "crontab"}, Dialect: DialectPosix}
		st := newParseState()
		st.depth = maxUnwrapDepth
		if got := unwrapPayload(c, "crontab", DialectPosix, "shell", st); got != nil {
			t.Fatalf("got %d units, want none", len(got))
		}
		if c.Unanalyzed != unanalyzedNestingLimit {
			t.Fatalf("Unanalyzed = %q, want %q", c.Unanalyzed, unanalyzedNestingLimit)
		}
		if c.Payload != "crontab" {
			t.Fatalf("Payload = %q, want the text still recorded for the operator", c.Payload)
		}
	})
	t.Run("expansion budget", func(t *testing.T) {
		t.Parallel()
		c := &Command{Base: "sh", Argv: []string{"sh", "-c", "crontab"}, Dialect: DialectPosix}
		spent := 0
		if got := unwrapPayload(c, "crontab", DialectPosix, "shell", parseState{budget: &spent}); got != nil {
			t.Fatalf("got %d units, want none", len(got))
		}
		if c.Unanalyzed != unanalyzedExpansionBudget {
			t.Fatalf("Unanalyzed = %q, want %q", c.Unanalyzed, unanalyzedExpansionBudget)
		}
	})
	t.Run("interpreter payloads are not wrappers", func(t *testing.T) {
		t.Parallel()
		// `python -c` sets Payload by design and at any depth; it is
		// NOT an un-analysed wrapper and must not trip R-157.
		for _, c := range ParseCommand(`python -c "import os; os.system('ls')"`, DialectPosix) {
			if c.Unanalyzed != "" {
				t.Fatalf("unit %q marked %q; an interpreter one-liner is a documented gap, not a fail-closed case",
					c.Base, c.Unanalyzed)
			}
		}
	})
}
