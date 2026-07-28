package setup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/processobs/bridge"
)

// ProbeTimeout bounds the read-only `schtasks /Query`. The
// probe is advisory: a slow or wedged interop shell must never hang an
// `observer init`.
const ProbeTimeout = 10 * time.Second

// TokenFileName is the default basename of the shared-token file
// the daemon generates and the capturer reads (`--token-file`). It sits next
// to the observer DB, the same place the browser rail persists its own
// ingress token.
const TokenFileName = "process-bridge-token" //nolint:gosec // G101: a FILENAME, not a credential.

// TokenPath resolves where the generated token lives: the
// configured override, else <observer-db-dir>/process-bridge-token.
func TokenPath(pc config.ProcessConfig, observerDir string) string {
	if p := pc.ETW.TokenPath; p != "" {
		return p
	}
	if observerDir == "" {
		return ""
	}
	return filepath.Join(observerDir, TokenFileName)
}

// Env is the I/O seam for the setup step. Every side effect
// the step can have — a PATH lookup, a `schtasks /Query` exec, an interop
// shell for the Windows username — enters through this struct, so tests inject
// fakes and NEVER touch the real Task Scheduler (the browserhost-tests-hit-the-
// real-registry class of mistake).
type Env struct {
	// LookPath resolves schtasks.exe (exec.LookPath in production).
	LookPath func(string) (string, error)
	// RunSchtasks executes schtasks.exe read-only and returns its combined
	// output. It is ONLY ever called with /Query arguments.
	RunSchtasks func(ctx context.Context, exe string, args ...string) (string, error)
	// ResolveObserver finds the Windows observer.exe
	// (bridge.ResolveWindowsObserver in production) — the SAME resolution the
	// bridge backend uses, never a second one.
	ResolveObserver func(explicit string) (string, bool)
	// ObserverEnvPath is $OBSERVER_WINDOWS_BINARY. ResolveObserver consults
	// the same variable internally; we read it here only to be able to SAY
	// which knob was set when resolution fails (it never selects anything).
	ObserverEnvPath string
	// Distro is $WSL_DISTRO_NAME.
	Distro string
	// WindowsUser is lazy on purpose: crossmount.WindowsUserName shells out to
	// cmd.exe, and a run where the step does not apply must not pay for it.
	WindowsUser func() string
	// HostIsWindows reports whether the daemon itself runs on Windows.
	HostIsWindows bool
}

// ProductionEnv wires the real probes.
func ProductionEnv() Env {
	return Env{
		LookPath:        exec.LookPath,
		RunSchtasks:     runSchtasksQuery,
		ResolveObserver: bridge.ResolveWindowsObserver,
		ObserverEnvPath: os.Getenv("OBSERVER_WINDOWS_BINARY"),
		Distro:          os.Getenv("WSL_DISTRO_NAME"),
		WindowsUser:     crossmount.WindowsUserName,
		HostIsWindows:   runtime.GOOS == "windows",
	}
}

// runSchtasksQuery execs schtasks.exe and returns its combined output. Only
// read-only /Query invocations reach it — this package never creates, changes
// or deletes a Scheduled Task.
func runSchtasksQuery(ctx context.Context, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()
	return string(out), err
}

// ResolveInputs performs the step's I/O — PATH lookup, the
// read-only task probe, binary/token/user resolution — and hands the planner a
// plain struct. Short-circuits before every probe it does not need.
func ResolveInputs(ctx context.Context, pc config.ProcessConfig, observerDir string, env Env) Inputs {
	in := Inputs{
		// BOTH switches, because both are load-bearing. [observer.process.etw]
		// opens the accept listener, but runProcessObserver returns before it
		// is ever constructed when the MASTER switch [observer.process].enabled
		// is off — so an elevated task registered in that state installs
		// successfully and then reconnects forever against a listener that
		// never starts. Treating ETW-on-master-off as "the feed is on" is what
		// let this surface authorise exactly that.
		ProcessEnabled: pc.Enabled,
		ETWEnabled:     pc.ETW.Enabled,
		Enabled:        pc.Enabled && pc.ETW.Enabled,
		ListenAddr:     pc.ETW.ListenAddr,
		TokenPath:      TokenPath(pc, observerDir),
		Distro:         env.Distro,
		HostIsWindows:  env.HostIsWindows,
	}
	// Pure data (no I/O): what the operator ASKED for, in the same precedence
	// order bridge.ResolveWindowsObserver applies, so a failed resolution can
	// name the knob that was actually set instead of guessing.
	if hint := strings.TrimSpace(pc.WindowsBinaryPath); hint != "" {
		in.WindowsObserverHint = hint
		in.WindowsObserverHintSource = "[observer.process].windows_binary_path"
	} else if hint := strings.TrimSpace(env.ObserverEnvPath); hint != "" {
		in.WindowsObserverHint = hint
		in.WindowsObserverHintSource = "$OBSERVER_WINDOWS_BINARY"
	}
	if !in.Enabled || env.LookPath == nil {
		return in
	}
	exe, err := env.LookPath("schtasks.exe")
	if err != nil || exe == "" {
		return in // no Windows Scheduled Tasks here → silent skip
	}
	in.SchtasksPath = exe
	in.Probe, in.ProbeErr = probeTask(ctx, exe, env.RunSchtasks)
	if in.Probe == ProbePresent {
		return in // nothing else needs resolving
	}
	if env.ResolveObserver != nil {
		if p, ok := env.ResolveObserver(pc.WindowsBinaryPath); ok {
			in.WindowsObserver = p
		}
	}
	if env.WindowsUser != nil {
		in.WindowsUser = env.WindowsUser()
	}
	return in
}

// probeTask runs the read-only `schtasks /Query /TN <name>`.
//
// Exit 0 means the task exists. A non-zero exit whose output says the task
// cannot be found means it does not (measured: "ERROR: The system cannot find
// the file specified.", exit 1). ANY other failure is Unknown — reported as
// such, never silently folded into "absent".
func probeTask(ctx context.Context, exe string, run func(context.Context, string, ...string) (string, error)) (Probe, string) {
	if run == nil {
		return ProbeUnknown, "no schtasks runner configured"
	}
	out, err := run(ctx, exe, "/Query", "/TN", TaskName)
	if err == nil {
		return ProbePresent, ""
	}
	low := strings.ToLower(out)
	if strings.Contains(low, "cannot find the file specified") ||
		strings.Contains(low, "does not exist") ||
		strings.Contains(low, "cannot find the path specified") {
		return ProbeAbsent, ""
	}
	detail := strings.TrimSpace(firstNonEmptyLine(out))
	if detail == "" {
		detail = err.Error()
	}
	return ProbeUnknown, detail
}

// firstNonEmptyLine returns the first non-blank line of s, or "".
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// HasSchtasks reports whether this host has a Windows Task Scheduler CLI at
// all — a native Windows box, or WSL with interop. It is the "does this feature
// apply here?" question, asked INDEPENDENTLY of whether the feature is turned
// on.
//
// ResolveInputs cannot answer it: it short-circuits before the PATH lookup when
// the ETW feed is disabled, which is the default, so its Inputs.SchtasksPath is
// empty on a Windows host with the feature off exactly as it is on a Linux
// laptop. PlanTask then collapses both into OutcomeSkip and — by its documented
// gate order — reports the DISABLED reason for both, because that is the
// actionable one on Windows.
//
// A surface that must decide whether to render at all needs the other half of
// that fact: offering to switch on a Windows-only feed on a Linux laptop is
// noise, and it is the one thing the skip reason cannot tell you.
//
// A nil LookPath is FALSE, not true: a seam nobody wired has not found a
// schtasks.exe, and the safe default for "we did not look" is "this does not
// apply", which renders nothing.
func HasSchtasks(env Env) bool {
	if env.LookPath == nil {
		return false
	}
	p, err := env.LookPath("schtasks.exe")
	return err == nil && p != ""
}
