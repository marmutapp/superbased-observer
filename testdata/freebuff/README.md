# testdata/freebuff — Freebuff (CodebuffAI) fixtures

**Captured**: 2026-08 against a live `freebuff` (CodebuffAI, `deepseek/deepseek-v4-flash`,
`agentTemplateId: base2-free-deepseek-flash`) session on WSL2 Ubuntu, in
`~/.config/manicode/projects/<slug>/chats/<RFC3339-dir>/`, plus a same-day
check of a native Windows install (`.config\manicode\freebuff.exe`) to
confirm the storage path is identical cross-OS.

**Anonymisation**: the real project slug/path (`needlehaystack` was itself
a synthetic benchmark repo, not sensitive) and `hostname`/`userId`/
`userEmail` fields visible in the app's own `log.jsonl` debug log (never
read by the adapter — see the off-limits list in
[`docs/freebuff-adapter.md`](../../docs/freebuff-adapter.md)) are NOT
reproduced here at all; only `chat-messages.json` / `run-state.json` /
`chat-meta.json` shapes are captured, with real file bodies replaced by
short synthetic stand-ins. Real `output` strings (e.g. file contents,
command stdout) were shortened but kept as **plain strings** — the real
shape, not JSON-encoded strings-of-strings.

## File inventory

| Path | Purpose |
|------|---------|
| `manicode/projects/needlehaystack/chats/2026-08-11T07-07-38.552Z/chat-messages.json` | Primary fixture. 6 messages: 2 mode-dividers, 2 user prompts, 2 `ai` messages. Exercises: reasoning-then-text threading, `read_files`/`write_file`/`run_terminal_command`/`glob`/`write_todos`/`ask_user` tool blocks, ONE `agent` block (`agentName`/`agentType` both `"basher"`, `params.command` carrying the real invocation args since `initialPrompt` is empty in practice) with its own nested `blocks` (a further `run_terminal_command` + `set_output`), and one genuinely-unmapped real tool name (`suggest_followups`) landing on `ActionUnknown`. |
| `manicode/projects/needlehaystack/chats/2026-08-11T07-07-38.552Z/run-state.json` | Sibling state file. `sessionState.fileContext.{projectRoot,cwd}` is the real-cwd source; `sessionState.mainAgentState.{contextTokenCount,creditsUsed,directCreditsUsed}` — the evidence that `contextTokenCount` is a context-window size (grows monotonically across a session) and CodebuffAI's actual billing unit is the separate `credits` currency, never token counts. |
| `manicode/projects/needlehaystack/chats/2026-08-11T07-07-38.552Z/chat-meta.json` | Undocumented-but-harmless corroborating sidecar (`messageCount`/`firstPrompt`/`messagesSize`/`messagesMtimeMs`) the app also writes per chat dir. NOT read by the adapter; included only so the fixture directory matches the real shape of a chat dir. |
| `manicode/projects/otherslug/chats/2026-07-09T00-12-09.857Z/chat-messages.json` | One-message fixture with **no sibling `run-state.json`** — pins the `resolveProjectRoot` fallback to `"[freebuff]"` when the state file is absent (e.g. a launch where no message was ever sent, matching a real observed `log.jsonl`-only chat dir). |
| `manicode/projects/mismatched-slug/chats/2026-07-10T00-00-00.000Z/{chat-messages.json,run-state.json}` | Project slug (`mismatched-slug`) deliberately does NOT match the real cwd's basename (`actual-project-name`) in `run-state.json` — pins that project-root resolution reads `run-state.json`, never the manicode project-directory slug. |

## What the captured data covers

- Whole-file-rewrite / message-count cursor semantics (grounded separately
  against three live chat dirs' `chat-meta.json.messageCount`, shared
  sibling mtimes, and a `--continue` invocation's `log.jsonl` line
  explicitly logging `"Loaded chat state from chat directory"` against the
  ORIGINAL dir).
- Nested-agent block recursion: real `agent` blocks carry their own
  `blocks` array (the subagent's private tool-call transcript) which is
  walked with a depth cap, not just the agent block itself.
- The full real `toolNames` capability-list line from a live `log.jsonl`
  (`spawn_agents, read_files, read_subtree, write_todos, suggest_followups,
  str_replace, write_file, ask_user, read_url, skill, set_output,
  list_directory, glob, render_ui, gravity_index, file_picker,
  code_searcher, researcher_web, researcher_docs, basher, tmux_cli,
  browser_use, code_reviewer_deepseek_flash, context_pruner`) — cross-
  referenced against every observed real `toolCall` to separate genuine
  TOOL names (mapped in `mapFreebuffTool`) from AGENT TYPE names
  (`basher`, `code_searcher`, `researcher_web`, `researcher_docs`,
  `code_reviewer_deepseek_flash` — these are `agentType` values on
  `agent` blocks, not `toolName`s, and need no separate mapping since
  `RawToolName` already carries the real `agentType` string).
- A real, observed-but-deliberately-unmapped tool name (`suggest_followups`
  — a "propose next prompts" UI hint with no corresponding normalized
  action) landing honestly on `ActionUnknown`, instead of a synthetic
  placeholder name.

## Known gap: names in the capability list with no captured invocation

`tmux_cli`, `read_subtree`, `render_ui`, `gravity_index`, `file_picker`,
`context_pruner` appear in the real `toolNames` list but were never
actually invoked in the captured session, so their `input`/`output`
shapes are ungrounded. `mapFreebuffTool` deliberately leaves them
unmapped (`ActionUnknown`) rather than guessing — see
`docs/freebuff-adapter.md`'s known-gaps section.
