// Package fsview provides read-only, symlink-escape-safe directory listing and
// file reading rooted at a caller-supplied project root.
//
// It is a small, dependency-free primitive used by the dashboard's per-terminal
// project panel (docs/plans/terminal-project-panels-and-org-sweep-plan-2026-07-23.md).
// The containment model mirrors internal/mcp's get_file path-safety semantics —
// clean+join, EvalSymlinks on BOTH the root and the candidate, containment
// checked AFTER symlink resolution — but is reimplemented here rather than
// imported: the MCP helpers are package-private and MCP-flavored, and this
// package must stay free of the MCP import graph.
//
// Every path a caller supplies is project-relative; an absolute path or a
// ".."-escape is rejected. The package never writes, never follows a symlinked
// directory into a listing, caps directory listings and file reads, and sniffs
// for binary content so a viewer never renders raw bytes.
//
// TOCTOU (accepted residual risk): path containment is check-then-use. To
// narrow the window, List and Read re-run EvalSymlinks and re-verify containment
// immediately before the os.Open/f.ReadDir call (reverifyContained), Read
// additionally opens non-blocking and re-checks IsRegular on the open
// descriptor, and List rejects a rel whose own final component is a symlink. A
// same-host attacker who can win the sub-microsecond race between that
// re-verification and the syscall could still swap a component to an
// outside-pointing symlink. This narrow residual is an ACCEPTED RISK: a local
// writer inside the project root already owns the terminal's PTY and thus has
// equivalent (or greater) access to that content directly — the same trust-
// domain acceptance the internal/mcp get_file path documents. A fully
// race-free guarantee would need openat2(RESOLVE_BENEATH)-style descriptor-
// relative traversal, which is deliberately NOT used here for Windows
// portability.
package fsview
