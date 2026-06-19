// Package codexipc detects and (optionally) terminates long-running
// codex `app-server` processes that intercept `codex exec` calls via
// codex's global IPC discovery mechanism.
//
// Background — V5-1 (docs/observer-platform-issues-v5.md): codex-cli
// uses a fixed-name named pipe `\\.\pipe\codex-ipc` on Windows or a
// `app-server-control.sock` AF_UNIX socket on POSIX for inter-process
// discovery. The pipe/socket name is NOT parameterized by CODEX_HOME,
// model, account, or any per-invocation state. Whichever codex
// `app-server` process binds the pipe/socket first becomes the
// de-facto app-server for ALL subsequent `codex exec` invocations on
// the host. Long-running app-server processes (VS Code Codex
// extension, Codex Desktop) therefore silently intercept `codex exec`
// calls and route their HTTP traffic through the holder's preloaded
// config — bypassing observer's `-c openai_base_url=…` proxy override
// entirely.
//
// This package provides the observer-side mitigation primitives that
// the `observer codex` wrapper uses:
//
//   - Detect(ctx) — enumerate matching app-server processes.
//   - Terminate(ctx, pid) — kill a specific PID (used by --exclusive).
//
// True fix requires one of the upstream codex-side changes proposed in
// docs/observer-platform-issues-v5.md §V5-1. This package only
// detects + offers a workaround.
package codexipc
