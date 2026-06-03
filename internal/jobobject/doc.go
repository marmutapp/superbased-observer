// Package jobobject attaches a child process to a Windows Job Object
// with KILL_ON_JOB_CLOSE so the child cascades to a clean kill when
// the parent observer process exits — even unexpectedly.
//
// V7-1 (from the V4 codex compression issues doc): the v1.7.5
// codex-watchdog Stop-Process path leaves a codex.exe outer zombie
// when the watchdog fires. The zombie continues to write to its
// rollout-*.jsonl, can collide with later cell scans, and on Codex
// Desktop has crashed the GUI on cleanup. Operator-side workaround:
// `observer codex --exclusive` enumerates and terminates them before
// exec. Observer-side fix (this package): wrap codex.exe in a Job
// Object owned by the observer wrapper; if the wrapper dies (clean
// exit, SIGKILL, or watchdog hammer), Windows automatically closes
// the wrapper's handles, and KILL_ON_JOB_CLOSE forces the inner
// codex.exe to die too.
//
// Non-Windows builds compile to a no-op stub (returns a nil Closer
// from AttachProcess) so the call site doesn't need build tags.
package jobobject
