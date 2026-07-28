// Package setup plans the elevated Windows Scheduled Task that runs the ETW
// process capturer (`observer process-bridge --etw --connect …`), as specified
// by docs/plans/process-obs-etw-windows-parity-plan-2026-07-26.md §W4 and
// consumed by docs/plans/etw-dashboard-setup-plan-2026-07-27.md.
//
// The package answers one question — "what should we tell the operator about
// the elevated ETW Scheduled Task, and with exactly which command?" — and it
// answers it in two halves:
//
//   - PlanTask is PURE. Every input arrives already resolved in an Inputs
//     struct, so the whole decision table (skip / present / manual / unknown /
//     blocked) is unit-testable without ever touching a real schtasks.exe.
//     imports_test.go pins plan.go free of os, os/exec, database/sql,
//     net/http and fsnotify.
//   - ResolveInputs performs the step's I/O — a PATH lookup, a read-only
//     `schtasks /Query`, binary/token/user resolution — behind the injectable
//     Env seam, so tests inject fakes and NEVER touch the real Task Scheduler.
//
// Module-boundary discipline (CLAUDE.md §1/§2): the planner is the pure core
// and I/O is injected; callers reach it through ResolveInputs + PlanTask and
// get a plain Plan back. Presentation is NOT here — `observer init` owns its
// own printer in cmd/observer/process_bridge_task.go, and the dashboard renders
// the same Plan its own way.
//
// The package is deliberately BUILD-TAG-FREE. A Linux/WSL daemon must be able
// to plan the Windows command it cannot itself run: nothing here executes the
// planned command, and the only exec is the read-only /Query probe behind the
// seam. This package NEVER creates, changes or deletes a Scheduled Task — the
// WSL interop token is Medium Mandatory Level and `schtasks /Create` is refused
// from it, so the only honest surface is an exact, copy-paste-ready command.
package setup
