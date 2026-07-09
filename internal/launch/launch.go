package launch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Environment describes the host context a launch plan is built
// against. Detect fills it from the running daemon's surroundings;
// tests construct literals.
type Environment struct {
	// GOOS is runtime.GOOS ("windows", "linux", "darwin", ...).
	GOOS string
	// IsWSL marks a linux daemon running inside WSL (Windows desktop
	// reachable via interop).
	IsWSL bool
	// WSLDistro is $WSL_DISTRO_NAME when known; empty falls back to
	// the default distro (wsl.exe without -d).
	WSLDistro string
	// Display marks a non-WSL linux host with a reachable display.
	Display bool
	// WTPath is the resolved Windows Terminal launcher (native PATH or
	// WSL interop PATH); empty when absent.
	WTPath string
	// CmdPath is the resolved cmd.exe; empty when absent.
	CmdPath string
	// TermPath/TermName are the first resolved linux terminal emulator
	// and its table name; empty when absent.
	TermPath string
	TermName string
	// OsaPath is the resolved osascript on darwin; empty when absent.
	OsaPath string
	// ExePath is this observer binary, used for wrapper launches.
	ExePath string
}

// Request is one launch ask: a tool from the hardcoded allow-list and
// whether durable proxy routing is already in place for it.
type Request struct {
	Tool string
	// Routed: the tool's own config already points at the proxy, so a
	// plain launch is proxied AND keeps the tool's native auth refresh
	// in charge (the D13 lesson). Unrouted launches use the observer
	// wrapper, which routes ad hoc.
	Routed bool
}

// Spec is the launch plan. Command (the copy-paste fallback) is
// always set; Argv is empty when no spawn mechanism exists on this
// host, with Reason saying why.
type Spec struct {
	// Argv is the full spawn vector; empty = nothing to spawn.
	Argv []string
	// Command is what a user pastes into their own terminal to get the
	// same session (always set, spawn or not).
	Command string
	// Method names the chosen mechanism ("wt", "cmd-start", "wsl-wt",
	// "wsl-cmd-start", "terminal", "osascript", "none").
	Method string
	// Reason explains an empty Argv.
	Reason string
}

// inner is the resolved tool command in the three shapes mechanisms
// need: argv elements (Windows-native), one shell string (bash -lc),
// and the user-facing copy-paste form.
type inner struct {
	argv    []string
	shell   string
	display string
}

// launchTools is the hardcoded allow-list: tool → the plain binary a
// routed install launches, and the observer subcommand the wrapper
// path uses. Nothing outside this map ever reaches a spawn argv.
var launchTools = map[string]struct{ plain, sub string }{
	"claude-code": {plain: "claude", sub: "claude"},
	"codex":       {plain: "codex", sub: "codex"},
}

// Tools reports the allow-listed tool names (for handlers' validation
// messages).
func Tools() []string {
	return []string{"claude-code", "codex"}
}

// mechanism is one row of the spawn decision table: the first row
// whose match holds builds the argv.
type mechanism struct {
	name  string
	match func(Environment) bool
	argv  func(Environment, inner) []string
}

// mechanisms is the ordered decision table (rule: table-driven, not
// nested conditionals). Windows-native prefers Windows Terminal over
// a plain cmd window; a WSL daemon reaches the Windows desktop
// through interop (proven by the P4.6 prototype: the daemon's
// inherited WSL_INTEROP socket outlives its launching session on
// current WSL — and when it has gone stale, the exec fails fast and
// Spawn reports it honestly); native linux walks the terminal table
// only when a display exists; darwin scripts Terminal.app.
var mechanisms = []mechanism{
	{
		name:  "wt",
		match: func(e Environment) bool { return e.GOOS == "windows" && e.WTPath != "" },
		argv: func(e Environment, in inner) []string {
			// cmd /k keeps the window open if the tool exits with an
			// error — the failure stays readable instead of flashing.
			return append([]string{e.WTPath, "cmd", "/k"}, in.argv...)
		},
	},
	{
		name:  "cmd-start",
		match: func(e Environment) bool { return e.GOOS == "windows" && e.CmdPath != "" },
		argv: func(e Environment, in inner) []string {
			// The empty string after start is the window title slot —
			// without it, start eats the first quoted arg as a title.
			return append([]string{e.CmdPath, "/c", "start", "", "cmd", "/k"}, in.argv...)
		},
	},
	{
		name:  "wsl-wt",
		match: func(e Environment) bool { return e.GOOS == "linux" && e.IsWSL && e.WTPath != "" },
		argv: func(e Environment, in inner) []string {
			// bash -lc: claude/codex typically live on login-shell
			// PATH only (~/.local/bin, nvm).
			return append(append([]string{e.WTPath}, wslReentry(e)...), "bash", "-lc", in.shell)
		},
	},
	{
		name:  "wsl-cmd-start",
		match: func(e Environment) bool { return e.GOOS == "linux" && e.IsWSL && e.CmdPath != "" },
		argv: func(e Environment, in inner) []string {
			return append(append([]string{e.CmdPath, "/c", "start", ""}, wslReentry(e)...), "bash", "-lc", in.shell)
		},
	},
	{
		name: "terminal",
		match: func(e Environment) bool {
			return e.GOOS == "linux" && !e.IsWSL && e.Display && e.TermPath != ""
		},
		argv: func(e Environment, in inner) []string {
			if e.TermName == "gnome-terminal" {
				return []string{e.TermPath, "--", "bash", "-lc", in.shell}
			}
			return []string{e.TermPath, "-e", "bash", "-lc", in.shell}
		},
	},
	{
		name:  "osascript",
		match: func(e Environment) bool { return e.GOOS == "darwin" && e.OsaPath != "" },
		argv: func(e Environment, in inner) []string {
			script := fmt.Sprintf("tell application %q to do script %q", "Terminal", in.shell)
			return []string{e.OsaPath, "-e", script, "-e", `tell application "Terminal" to activate`}
		},
	},
}

// Plan builds the launch Spec for one request. Pure: no I/O, no
// environment reads — everything comes in via env.
func Plan(req Request, env Environment) (Spec, error) {
	in, err := innerFor(req, env)
	if err != nil {
		return Spec{}, err
	}
	for _, m := range mechanisms {
		if m.match(env) {
			return Spec{Argv: m.argv(env, in), Command: in.display, Method: m.name}, nil
		}
	}
	return Spec{Command: in.display, Method: "none", Reason: noMechanismReason(env)}, nil
}

// innerFor resolves the tool command. Routed installs launch the
// plain tool (proxied via its own config, native auth refresh — D13);
// unrouted installs launch the observer wrapper by absolute path so
// the spawn works even when observer is not on PATH.
func innerFor(req Request, env Environment) (inner, error) {
	entry, ok := launchTools[req.Tool]
	if !ok {
		return inner{}, fmt.Errorf("launch: unknown tool %q (allowed: %s)", req.Tool, strings.Join(Tools(), ", "))
	}
	var argv []string
	if req.Routed {
		argv = []string{entry.plain}
	} else {
		if env.ExePath == "" {
			return inner{}, errors.New("launch: observer executable path unknown for wrapper launch")
		}
		argv = []string{env.ExePath, entry.sub}
	}
	return inner{argv: argv, shell: shellJoin(argv), display: shellJoin(argv)}, nil
}

// noMechanismReason words the honest empty-Argv cases.
func noMechanismReason(env Environment) string {
	switch {
	case env.GOOS == "linux" && env.IsWSL:
		return "no Windows interop launcher reachable from this WSL daemon (wt.exe / cmd.exe not on PATH)"
	case env.GOOS == "linux" && !env.Display:
		return "headless host: no display to open a terminal on"
	case env.GOOS == "linux":
		return "no known terminal emulator found (x-terminal-emulator, gnome-terminal, konsole, xfce4-terminal, xterm)"
	default:
		return "no terminal launch mechanism on this platform"
	}
}

// wslReentry builds the wsl.exe re-entry prefix, with -d only when
// the distro is known.
func wslReentry(e Environment) []string {
	if e.WSLDistro != "" {
		return []string{"wsl.exe", "-d", e.WSLDistro, "--"}
	}
	return []string{"wsl.exe", "--"}
}

// shellJoin renders argv as one POSIX shell command, single-quoting
// any element that needs it (also the user-facing copy-paste form, so
// space-bearing exe paths paste correctly).
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"\\$`&|;<>(){}*?#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Detect resolves the real host Environment for the running daemon.
// LookPath only ever resolves hardcoded well-known names.
func Detect() Environment {
	env := Environment{GOOS: runtime.GOOS}
	env.ExePath, _ = os.Executable()
	switch runtime.GOOS {
	case "windows":
		env.WTPath, _ = exec.LookPath("wt.exe")
		env.CmdPath, _ = exec.LookPath("cmd.exe")
	case "darwin":
		env.OsaPath, _ = exec.LookPath("osascript")
	case "linux":
		if d := os.Getenv("WSL_DISTRO_NAME"); d != "" {
			env.IsWSL, env.WSLDistro = true, d
		} else if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
			env.IsWSL = true
		}
		if env.IsWSL {
			// Resolvable only when WSL appends the Windows PATH (the
			// default); otherwise Plan degrades to copy-paste.
			env.WTPath, _ = exec.LookPath("wt.exe")
			env.CmdPath, _ = exec.LookPath("cmd.exe")
		} else {
			env.Display = os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
			for _, name := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xfce4-terminal", "xterm"} {
				if p, err := exec.LookPath(name); err == nil {
					env.TermPath, env.TermName = p, name
					break
				}
			}
		}
	}
	return env
}

// spawnSettleWindow is how long Spawn watches a launcher for an early
// failure exit before declaring the window up. Dispatch-style
// launchers (wt, cmd start, osascript) exit well inside it; a stale
// WSL interop socket also fails inside it.
const spawnSettleWindow = 1500 * time.Millisecond

// Spawn executes a Spec's Argv detached from the request lifecycle
// (the terminal must outlive the HTTP call, so the context governs
// only the settle wait, never the child). A clean fast exit counts as
// dispatched; a fast non-zero exit is an honest failure; still
// running after the settle window means the terminal itself is up.
func Spawn(ctx context.Context, spec Spec) error {
	if len(spec.Argv) == 0 {
		return errors.New("launch.Spawn: empty argv")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...) //nolint:gosec // G204: launching the caller-specified argv is this package's purpose; callers own the command table.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch.Spawn: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("launch.Spawn: launcher exited: %w", err)
		}
		return nil
	case <-time.After(spawnSettleWindow):
		return nil
	case <-ctx.Done():
		return fmt.Errorf("launch.Spawn: %w", ctx.Err())
	}
}
