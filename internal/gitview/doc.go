// Package gitview is a read-only wrapper over the git command-line tool. It
// produces a single snapshot of a project's git state — current branch,
// upstream tracking and ahead/behind counts, working-tree status, and a bounded
// commit graph — for the dashboard's per-terminal project panel
// (docs/plans/terminal-project-panels-and-org-sweep-plan-2026-07-23.md).
//
// It is deliberately separate from internal/git, which stays pure (os.Stat
// walks, no subprocess). gitview is the ONLY package in the tree that shells out
// to git, and it does so read-only: it never mutates a repository.
//
// Every invocation runs through runGit, which pins the safety envelope:
// exec.CommandContext with a per-command 3s timeout, LC_ALL=C for stable
// parsing, --no-optional-locks so a read never contends with a concurrent
// editor, stdout AND stderr byte caps so a pathological repository can't exhaust
// memory, a WaitDelay so a descendant holding the output pipes can't block Wait
// forever, the repository root passed via -C from server-held state only (never
// a client value), and "--" separators to keep refs and paths unambiguous. A
// missing git binary surfaces as ErrGitUnavailable, which the caller maps to a
// friendly "git unavailable" payload.
//
// Hostile-repository hardening: a read-only dashboard request must never cause
// git to EXECUTE a program configured by the repository under inspection.
// runGit forces safe command-line config (-c) that overrides any repo/global/
// system config — core.fsmonitor=false (no per-status helper), core.hookspath=
// empty (no hooks), core.pager=cat + --no-pager (no pager program) — and sets
// GIT_TERMINAL_PROMPT=0 + GIT_ASKPASS so git never spawns a credential helper or
// blocks on a prompt. The status set is capped (StatusTruncated) and the commit
// graph is bounded (LogTruncated) independently of the byte cap.
package gitview
