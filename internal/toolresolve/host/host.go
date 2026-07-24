// Package host builds the production toolresolve.Env: the real filesystem
// probes, WSL detection and the Windows homes reached over /mnt (via
// internal/platform/crossmount), and a one-shot login-shell PATH capture that
// surfaces binaries installed into a login-only prefix without a daemon
// restart. It is the impure boundary for the pure internal/toolresolve
// resolver — the only place os / os/exec / crossmount are touched — so the
// resolver itself stays testable with injected fakes.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/toolresolve"
)

// defaultLoginTimeout bounds the login-shell PATH capture. A login shell that
// sources a slow rc file must not stall a launch; on timeout the capture is
// skipped and resolution proceeds on the process PATH alone.
const defaultLoginTimeout = 1500 * time.Millisecond

// loginOutputCap bounds the captured stdout (a pathological shell must not OOM
// the daemon). A real $PATH is well under this.
const loginOutputCap = 64 * 1024

// ErrUnsupportedShell is returned by CaptureLoginPath when $SHELL is empty or
// its basename is not a known POSIX-style login shell (nushell, tcsh, …). The
// caller treats it as "no login merge", not a hard error.
var ErrUnsupportedShell = errors.New("toolresolve/host: unsupported or unknown login shell")

// Options tunes NewEnv. Zero values fall back to $SHELL and defaultLoginTimeout.
type Options struct {
	// Shell overrides the login shell to capture PATH from (default $SHELL).
	Shell string
	// Timeout bounds the login-shell capture (default 1500ms).
	Timeout time.Duration
}

// NewEnv builds the production toolresolve.Env. The login-shell PATH capture is
// memoized per returned Env via sync.OnceValues (one subprocess per process,
// no matter how many tools resolve); it is nil on a Windows daemon, where a
// POSIX login shell is not the PATH authority. Callers that resolve many tools
// should themselves memoize NewEnv (a package-level sync.OnceValue) so the
// crossmount walk and env reads happen once.
func NewEnv(opts Options) toolresolve.Env {
	shell := opts.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}

	wsl := crossmount.IsWSL()
	home, _ := os.UserHomeDir()

	var foreign []string
	if wsl {
		for _, h := range crossmount.AllHomes() {
			if h.OS == crossmount.OSWindows {
				foreign = append(foreign, h.Path)
			}
		}
	}

	env := toolresolve.Env{
		GOOS:         runtime.GOOS,
		WSL:          wsl,
		Home:         home,
		ForeignHomes: foreign,
		ProcessPath:  filepath.SplitList(os.Getenv("PATH")),
		PathExt:      windowsPathExt(),
		Stat:         os.Stat,
		EvalSymlinks: filepath.EvalSymlinks,
		Glob:         filepath.Glob,
	}

	// A Windows daemon has no POSIX login shell to consult; leave LoginPath nil.
	if runtime.GOOS != "windows" {
		once := sync.OnceValues(func() ([]string, error) {
			return CaptureLoginPath(shell, timeout)
		})
		env.LoginPath = func() ([]string, error) { return once() }
	}
	return env
}

// windowsPathExt reads the PATHEXT precedence list (used to order the Windows
// candidate spellings) ONLY on a Windows daemon: it splits %PATHEXT% on ';',
// uppercases each entry, and drops empties. It returns nil off Windows, where a
// POSIX daemon has no PATHEXT authority and the spec's candidate order stands.
func windowsPathExt() []string {
	if runtime.GOOS != "windows" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(os.Getenv("PATHEXT"), ";") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		out = append(out, strings.ToUpper(e))
	}
	return out
}

// CaptureLoginPath runs the login shell once and returns its $PATH split into
// dirs. It builds a per-shell command (bash/zsh/sh/ksh/dash print $PATH,
// fish joins its list), runs it with stdin detached and stdout bounded, and
// enforces timeout. An unknown/empty shell yields ErrUnsupportedShell; a
// timeout or non-zero exit yields a wrapped error. The caller merges the
// result behind the process PATH.
func CaptureLoginPath(shell string, timeout time.Duration) ([]string, error) {
	argv, err := loginArgv(shell)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultLoginTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) /* #nosec G204 -- argv[0] is the operator's own login shell ($SHELL / explicit Options.Shell) and the arguments are constants from loginArgv's per-shell table; nothing request- or network-derived reaches this exec */
	cmd.Stdin = nil                                       // exec wires /dev/null; no inherited terminal
	// WaitDelay bounds Wait after the context is canceled: a login shell that
	// backgrounds a slow child (which would otherwise hold the stdout pipe open)
	// is force-reaped instead of stalling the launch to the child's lifetime.
	cmd.WaitDelay = 500 * time.Millisecond
	out := &capWriter{limit: loginOutputCap}
	cmd.Stdout = out
	// stderr is intentionally discarded — rc-file chatter is not our concern.

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("toolresolve/host: login PATH capture timed out after %s: %w", timeout, ctx.Err())
		}
		return nil, fmt.Errorf("toolresolve/host: login PATH capture failed: %w", err)
	}

	raw := strings.TrimRight(out.String(), "\r\n")
	if raw == "" {
		return nil, nil
	}
	return filepath.SplitList(raw), nil
}

// loginArgv builds the per-shell PATH-print command, keyed on the shell's
// basename. Unknown or empty shells return ErrUnsupportedShell.
func loginArgv(shell string) ([]string, error) {
	if strings.TrimSpace(shell) == "" {
		return nil, ErrUnsupportedShell
	}
	switch filepath.Base(shell) {
	case "bash", "zsh", "sh", "ksh", "dash":
		return []string{shell, "-lc", `printf %s "$PATH"`}, nil
	case "fish":
		return []string{shell, "-lc", "string join : $PATH"}, nil
	default:
		return nil, ErrUnsupportedShell
	}
}

// capWriter buffers up to limit bytes and silently drops the rest, so a
// runaway shell cannot exhaust memory.
type capWriter struct {
	buf   bytes.Buffer
	limit int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.limit - w.buf.Len(); room > 0 {
		if room < len(p) {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	// Always report the full length so the child's writes never error/block.
	return len(p), nil
}

func (w *capWriter) String() string { return w.buf.String() }
