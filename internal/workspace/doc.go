// Package workspace is the pure git workspace-preparation planner for B9
// sandboxed terminals
// (docs/plans/b9-sandboxed-terminals-implementation-plan-2026-08-08.md §4,
// unit U3).
//
// It decides WHAT git commands to run and WHERE, and validates every path
// and token that will reach a subprocess argv along the way — it never
// runs anything itself. The caller (U4 termsvc / U5 cmd wiring) supplies
// its own subprocess runner, invokes Plan's returned Steps in order, and
// stops at the first failure; a workspace-prep failure never orphans a
// terminal_runs row.
//
// Per CLAUDE.md's module-boundary discipline this package imports no
// os/exec, database/sql, net/http, or fsnotify (pinned by
// imports_test.go). Two things stay deliberately outside it:
//
//   - Filesystem canonicalization. ValidateManagedWorkspace checks two
//     already-cleaned absolute paths for containment; resolving symlinks
//     (filepath.EvalSymlinks) so a planted symlink can't point a
//     workspace path outside the managed root is the CALLER's job, done
//     once the git steps have actually run and the path exists on disk.
//   - Running git. Plan returns argv + a working directory per Step; the
//     runner itself (with GIT_TERMINAL_PROMPT=0, no GIT_ASKPASS, no
//     client -c, and the configured prep_timeout_seconds context) is
//     injected by the caller and lives in cmd/observer or
//     internal/termsvc, never here.
package workspace
