package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// attach_client.go holds the PLATFORM-NEUTRAL half of session-attach's CLI
// client (design 2026-07-19, Phase 1): the socket-path formula, the resolved-
// input struct, the resume-hint composer, and the forwarded-argv builder. The
// interactive client itself — raw-mode terminal handling, signal plumbing, the
// /dev/tty reader, the stdio bridge — is POSIX-only and lives in
// attach_client_unix.go (`//go:build unix`); a `//go:build !unix` stub in
// attach_client_other.go returns an honest "Linux/WSL-only in v1" error so the
// tree cross-compiles (B2-1, design §6 decision 3). Everything a test needs on
// every platform (attachSocketPath / attachExtraArgs / nativeResumeHint) stays
// here so those tests keep running under Windows/darwin too.
//
// `observer <tool> --attach` becomes a thin PTY-proxy client: instead of
// exec'ing the tool as a child of the user's shell (the bare launcher), it asks
// the running daemon — over the owner-only AF_UNIX attach socket — to spawn the
// tool's PTY through the SAME termsession.Manager the dashboard drives. The
// operator's terminal is viewer #1; the dashboard can join as viewer #2 over
// the existing /ws/launch fan-out. Killing this client detaches (the child
// lives on under the daemon); Ctrl-C reaches the agent as a raw byte.

// attachSocketFile is the attach socket's basename inside the dedicated attach
// directory. The client and the `observer start` server MUST derive the path
// identically — attachSocketPath is the one formula both use.
const attachSocketFile = "attach.sock"

// attachSocketDir is the dedicated directory (under the DB dir) that holds the
// attach socket. It is created 0700 (owner-only search), which is what makes
// connect(2) OS-enforced owner-only with no race window regardless of the
// socket file's own mode (A1). See attachsock.ListenSocket.
const attachSocketDir = "attach"

// attachSocketPath returns the attach socket path for a given observer DB path:
// <dir(dbPath)>/attach/attach.sock. It is the single source of truth shared by
// the daemon's ListenSocket (cmd/observer/start.go) and the `--attach` client's
// Dial, so the two never drift. The socket lives in its own 0700 directory so
// the parent-dir permission — not a racy chmod-after-listen — enforces
// owner-only access (A1).
func attachSocketPath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), attachSocketDir, attachSocketFile)
}

// attachLaunch carries the resolved inputs for an `observer <tool> --attach`
// client session. proxyEnv is the tool-specific proxy-routing env the daemon
// forwards to the attached child ("KEY=VALUE" entries); it is nil/empty when
// the escape hatch (--no-proxy OR [terminal.attach].route_proxy=false) opts out
// OR when the tool routes without env vars (codex, whose inner launcher injects
// `-c openai_base_url`). The subcommand is resolved from the capability
// registry, never passed in.
type attachLaunch struct {
	// tool is the registry tool name (e.g. "claude-code", "codex").
	tool string
	// configPath is the --config override ("" = default).
	configPath string
	// proxyURL is the resolved proxy base URL, for the daemon-unreachable hint.
	proxyURL string
	// proxyEnv is the routing env forwarded to the attached child.
	proxyEnv []string
	// extraArgs are the allow-listed argv tokens forwarded to the inner
	// `observer <sub>` launcher: the routing escape hatch (--no-proxy-route),
	// the --proxy / --config overrides, forwarded wrapper flags (e.g.
	// --claude-path), and the operator's trailing tool args (`-- ...`).
	// Expressed in ARGV because the inner launcher self-routes regardless of
	// env, so an env-only escape hatch is a no-op (B2/B3).
	extraArgs []string
	// stderr receives human-facing status lines (never the child's output).
	stderr io.Writer
}

// nativeResumeHint composes the daemon-exit resume guidance from the tool's
// grounded ResumeSpec (session-attach design §2.4). When native resume is not
// yet grounded it degrades to a generic, honest phrase rather than asserting a
// resume command the tool may not support. Dispatch is on the ResumeSpec shape,
// never a tool-name branch (CLAUDE.md #3).
func nativeResumeHint(capab integration.Capability) string {
	if capab.Resume.Kind != integration.ResumeNative || capab.Resume.Subcommand == "" {
		return "resume it natively to continue"
	}
	switch capab.Resume.IDMechanism {
	case "flag:--resume":
		return fmt.Sprintf("resume it natively with `%s --resume <id>`", capab.Resume.Subcommand)
	case "subcommand:resume":
		return fmt.Sprintf("resume it natively with `%s resume <id>`", capab.Resume.Subcommand)
	case "positional":
		return fmt.Sprintf("resume it natively with `%s <id>`", capab.Resume.Subcommand)
	default:
		return fmt.Sprintf("resume it natively with `%s`", capab.Resume.Subcommand)
	}
}

// attachExtraArgs builds the allow-listed argv the CLI attach client forwards to
// the inner `observer <sub>` launcher (B2/B3). It is deliberately EXPLICIT — a
// fixed set of flags observer understands plus the operator's `--` tool
// remainder — never a blind copy of the outer argv:
//
//   - --no-proxy-route  when the escape hatch is engaged (the inner launcher
//     then skips base-URL / `-c openai_base_url` injection);
//   - --proxy <url>     when the operator overrode the proxy URL;
//   - --config <path>   when the outer invocation used a non-default config;
//   - passthrough...    launcher-specific wrapper flags the inner launcher
//     should honor (e.g. --claude-path/--codex-path, --no-app-server-check),
//     enumerated explicitly by each launcher (B2-6);
//   - -- <toolArgs...>  the operator's trailing tool args (`observer codex
//     --attach -- --model X`), so launcher state is not dropped.
func attachExtraArgs(noProxyRoute bool, proxyOverride, configPath string, passthrough, toolArgs []string) []string {
	var args []string
	if noProxyRoute {
		args = append(args, "--no-proxy-route")
	}
	if proxyOverride != "" {
		args = append(args, "--proxy", proxyOverride)
	}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	args = append(args, passthrough...)
	if len(toolArgs) > 0 {
		args = append(args, "--")
		args = append(args, toolArgs...)
	}
	return args
}
