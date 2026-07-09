package termsession

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
)

// Spec is the fully server-derived launch specification. The Manager builds
// the launch argv from these fields alone — a caller NEVER passes raw argv,
// paths, or environment overrides sourced from the client. Every field is
// validated at Create.
type Spec struct {
	// BinPath is the observer binary to exec, from os.Executable(),
	// injected by cmd. Never client-supplied.
	BinPath string
	// Subcommand is the observer launcher verb (e.g. "claude", "codex").
	// It comes from the integration registry's LaunchSpec, validated by
	// the dashboard against the launchable capability set.
	Subcommand string
	// SessionID is the source session to continue from (--continue-from).
	SessionID string
	// Carry is the handoff carry mode (--carry); "" omits the flag so the
	// launcher uses its configured default.
	Carry string
	// FromMessage forks the handover after this 1-based transcript message
	// (--from-message); 0 forks at the last message (the flag is omitted).
	FromMessage int
	// Rows and Cols are the initial PTY window size; 0 lets the OS pick a
	// default (80x24-ish) until the client sends its first resize.
	Rows uint16
	Cols uint16
	// Env is the child environment; nil means inherit the daemon's
	// os.Environ() at spawn time. The dashboard leaves this nil — the
	// spawned launcher needs the daemon's own env (proxy port, HOME, …).
	Env []string
}

// argv builds the exec argv from the validated Spec. Server-derived only:
// [bin, subcommand, "--continue-from", id] plus "--carry <c>" and
// "--from-message <n>" when set.
func (s Spec) argv() []string {
	a := []string{s.BinPath, s.Subcommand, "--continue-from", s.SessionID}
	if s.Carry != "" {
		a = append(a, "--carry", s.Carry)
	}
	if s.FromMessage > 0 {
		a = append(a, "--from-message", strconv.Itoa(s.FromMessage))
	}
	return a
}

// PTY is a live pseudo-terminal bound to a running process. It is injected
// into the Manager so the lifecycle logic is testable with an in-memory
// stub — the real implementation (unix only) wraps a creack/pty master.
type PTY interface {
	// Read/Write move bytes to/from the terminal (client-visible I/O).
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	// Resize sets the terminal window size.
	Resize(rows, cols uint16) error
	// Wait blocks until the process exits and returns its exit code.
	Wait() (exitCode int, err error)
	// Kill force-reaps the whole process tree (process group on unix) and
	// closes the master fd. It must be idempotent.
	Kill() error
	// Close releases the master fd without necessarily killing the child
	// (used when detaching); Kill is the authoritative teardown.
	Close() error
}

// Spawner turns a validated Spec into a live PTY. The default OS
// implementation is platform-specific (unix: creack/pty; windows:
// unsupported). Tests inject a fake.
type Spawner interface {
	Spawn(spec Spec) (PTY, error)
}

// ErrPlatformUnsupported is returned by the OS spawner on a platform where
// an in-process PTY cannot be created (a native-Windows observer daemon).
// The dashboard surfaces it as an honest terminal message, not a hang.
var ErrPlatformUnsupported = errors.New("termsession: embedded terminal is not supported on this OS — run the observer daemon under WSL/Linux")

// NewOSSpawner returns the platform's real PTY spawner (creack/pty on
// unix). Injected by cmd into NewManager; tests bypass it with a fake.
func NewOSSpawner() Spawner { return newOSSpawner() }

// PTYSupported reports whether this OS can host the embedded web terminal
// (an in-process PTY). It is true on unix and false on a native-Windows
// daemon. cmd consults it to leave the dashboard launch seam unwired on an
// unsupported OS — hiding the "Launch here" affordance up front instead of
// letting it fail on click — while the platform-independent handoff-doc
// migration ("Write handover doc") remains available everywhere.
func PTYSupported() bool { return ptySupported() }

// secretBytes is the entropy of a session handle (32 bytes → 256 bits,
// base64url-encoded to a 43-char opaque string). The handle is the only
// thing the websocket route authenticates against, so it must be
// unguessable; it is minted only by the Origin-checked launch POST.
const secretBytes = 32

// newToken mints an opaque base64url session identifier from crypto/rand.
func newToken() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("termsession: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
