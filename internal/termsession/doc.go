// Package termsession is the one-owner, in-memory registry of live PTY
// sessions behind the dashboard's "Launch <tool> here" embedded web
// terminal (docs/session-handoff.md launch section).
//
// A session is a single `observer <subcommand> --continue-from <id>`
// process running in a pseudo-terminal: the observer launcher seeds the
// distilled handover as the tool's first prompt and then execs the real AI
// tool with the PTY as its stdio, so the tool's TUI renders straight into
// the browser over a websocket. The argv is built SERVER-SIDE from a
// validated Spec — this package never accepts raw argv or paths from a
// caller.
//
// Reconnect model (Tier 2): each session runs an always-on pump goroutine
// that drains its PTY into a bounded per-session replay ring (outBuf) whether
// or not a client is attached, so a clientless PTY never blocks. A websocket
// disconnect DETACHES the client (unblocking its Read, keeping the child
// alive) rather than reaping it, so a browser tab-close/refresh survives: a
// reconnecting client re-attaches, replays the ring (recent scrollback), then
// tails live. Only an explicit Close (Stop & close / DELETE) or the idle /
// exit-linger reaper kills the process. Attachment is still single-client
// (the attached CAS); Read serves the attached client from the ring, not the
// PTY directly.
//
// Module discipline (CLAUDE.md): the PTY + process are behind an injected
// Spawner/PTY interface (rule 1) so the Manager's lifecycle logic is
// unit-testable with an in-memory stub; the package imports only stdlib +
// creack/pty and never dashboard/adapters/cmd (rule 2); it is the sole
// owner of the live-session map (rule 4). All creack/pty usage is confined
// to the unix build (spawn_unix.go) because the master-side resize ioctl is
// unix-only; a native-Windows observer daemon reports the embedded terminal
// as unsupported (run the daemon under WSL), which is the operator's actual
// deployment shape.
package termsession
