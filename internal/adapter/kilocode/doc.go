// Package kilocode implements the Adapter interfaces for the two Kilo Code
// products that share a brand:
//
//   - kilo-code (legacy IDE extension, "kilocode.kilo-code") — a Cline + Roo
//     Code fork distributed as a VS Code / JetBrains extension. Persistence
//     is BYTE-IDENTICAL to Cline: per-task directories under
//     <vsCodeGlobalStorage>/kilocode.kilo-code/tasks/<taskId>/ carrying
//     api_conversation_history.json + ui_messages.json. The LegacyAdapter
//     delegates parsing to internal/adapter/cline (the format is the same
//     content-block JSON array) and re-tags emitted rows with
//     Tool = models.ToolKiloCode.
//
//   - kilo-code-cli (the new @kilocode/cli, a fork of sst/opencode) —
//     terminal TUI + headless CLI; the all-new IDE extension is rebuilt on
//     this same runtime. Persists at ~/.local/share/kilo/kilo.db (SQLite,
//     same path on Linux + macOS + Windows). Schema mirrors OpenCode's
//     message/part/todo tables with Kilo-specific additions
//     (project/workspace/event/session_message/account/permission/
//     session_share). The CLIAdapter is structurally a transposition of
//     internal/adapter/opencode — same loadXxxEvents topology, watermark on
//     MAX(time_updated), foreign-mount stageMirror, [kilo-code-cli]
//     placeholder cwd. Differences: JOIN project.worktree as a cwd
//     fallback past message.path.cwd; emit Tool = models.ToolKiloCodeCLI.
//
// See docs/plans/kilocode-adapter-plan-2026-06-06.md for the Phase-0
// research note (confirmed live capture from WSL + Windows 2026-06-06).
package kilocode
