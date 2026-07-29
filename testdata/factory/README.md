# Factory `droid` CLI — fixture inventory

Live-captured fixtures from Factory AI's `droid` CLI (binary at
`~/.local/bin/droid`; data dir `~/.factory/`), gathered 2026-07-29 for
Phase-0 adapter research. See
`docs/plans/factory-droid-adapter-plan-2026-07-29.md` for the full
schema analysis these fixtures support.

## Directory-name encoding

`droid` stores sessions under `~/.factory/sessions/<dash-encoded-cwd>/`.
The dash-encoding is **lossy** (a real path component containing a
dash is indistinguishable from a path separator) — e.g.
`-C-Users-auzy_-copilot-smoke` could be `C:\Users\auzy_\copilot-smoke`
or `C:\Users\auzy_\copilot\smoke`. The authoritative project root is
the inline `cwd` field on every session's `session_start` event, which
this fixture set confirms is accurate on **both** Linux and Windows
(see `plan.md` §Project-root resolution). The two subdirectories below
were renamed from their raw dash-encoded form to a readable OS-tagged
label; the original encoded name is noted per directory.

## Fixtures

### `sessions/linux-home-marmutapp-needlehaystack/`
(source directory: `~/.factory/sessions/-home-marmutapp-needlehaystack/`,
WSL, `cwd = /home/marmutapp/needlehaystack`)

- **`774f4bf6-8025-4790-95b6-e8f854f09891.{jsonl,settings.json,settings.json.bak}`**
  — minimal-shape fixture. The `.jsonl` has exactly one line
  (`session_start` only, 210 bytes, no messages ever sent). Useful for
  testing `IsSessionFile`/parser behavior on a just-created, empty
  session. Its `.settings.json` vs `.settings.json.bak` pair also
  demonstrates a **model switch mid-session-lifecycle**: `.bak` shows
  `model: "claude-opus-5"`, current shows
  `model: "custom:GPT-5.4-Mini-[OpenAI-BYOK]-0"` — `droid` writes the
  previous settings snapshot to `.bak` before persisting a change.

- **`11080800-d13f-4c9d-b6df-149ea74d7723.{jsonl,settings.json,settings.json.bak}`**
  — the richest fixture (17 JSONL lines, ~65KB). Real user/assistant
  turns under the OpenAI BYOK custom model
  (`custom:GPT-5.4-Mini-[OpenAI-BYOK]-0`, `providerLock:
  "generic-chat-completion-api"`), including:
  - `thinking` content blocks carrying an **encrypted OpenAI Responses
    reasoning signature** (`signature` = a JSON blob with
    `encrypted_content`, `signatureProvider: "openai"`) alongside a
    **plaintext `thinking` summary string** — droid persists both.
  - `tool_use` / `tool_result` pairs. The tool name is **`Read`**
    (Claude-Code-style capitalized taxonomy) even though the
    underlying model is an OpenAI BYOK model — droid normalizes tool
    names across providers.
  - one `compaction_state` event
    (`summaryKind: "provider_switch_serialization"`,
    `anchorMessage`, `removedCount`, `summaryTokens`, and an embedded
    `systemInfo` snapshot with `git status`/`git log`/`pwd`/`ls`
    command+output pairs captured at compaction time — this is a
    session-fact-injection mechanism, not just a text summary).
  - three `agent_turn_outcome` events (`reason`:
    `"error"|"completed"`, `resultKind: "text"`) — one per assistant
    turn boundary.
  - non-zero `tokenUsage` / `inclusiveTokenUsage` / `lastCallTokenUsage`
    in the sidecar `settings.json`, confirming
    `lastCallTokenUsage.inputTokens (3131) < cacheReadTokens (23040)`
    — i.e. droid's persisted `inputTokens` is already **NET** of cache
    reads, even for an OpenAI-shaped underlying call (OpenAI's own
    wire `prompt_tokens` is normally GROSS — see
    `feedback_openai_input_is_gross.md`). Flagged in the plan doc as
    needing further live cross-provider confirmation.

### `sessions/windows-C-Users-auzy_-copilot-smoke/`
(source directory:
`C:\Users\auzy_\.factory\sessions\-C-Users-auzy_-copilot-smoke\`,
`cwd = C:\Users\auzy_\copilot-smoke`)

- **`7df5bcf0-6cd9-4c89-8925-1bf8b3fb061d.{jsonl,settings.json,settings.json.bak}`**
  — Windows-origin fixture (16 JSONL lines, ~29KB), chosen because it
  is the only captured session containing a **`todo_state`** event
  type (not present in any Linux session in this corpus):
  ```
  {"type":"todo_state","id":"...","timestamp":"...",
   "todos":{"todos":"1. [in_progress] Inspect repository contents\n2. [pending] Summarize folder structure and purpose"},
   "messageIndex":2}
  ```
  Also carries real non-zero token usage (`providerLock: "openai"`,
  `tokenUsage.inputTokens: 18320`, `cacheReadTokens: 67072`) and the
  same NET-input arithmetic pattern as the Linux rich fixture
  (`lastCallTokenUsage.inputTokens: 303 < cacheReadTokens: 16896`).
  Event-type sequence: `session_start, message×3, agent_turn_outcome,
  message×2, todo_state, message×2, todo_state, message×4,
  agent_turn_outcome`.

### `settings.json.example`
A **hand-reconstructed, redacted** version of the top-level
`~/.factory/settings.json` (global, one per install — distinct from
the per-session sidecar `.settings.json` files above). The real file
contains a live OpenAI API key under `customModels[0].apiKey`; this
example replaces it with the placeholder string
`"REDACTED-PLACEHOLDER-DO-NOT-USE"` and is NOT a byte-for-byte copy.
Confirmed shape: `customModels[]` (BYOK model entries with
`model`/`id`/`baseUrl`/`apiKey`/`displayName`/`maxOutputTokens`/
`provider`), `logoAnimation`, `trustedFolders` (map of path →
`{trustedAt}`), `ideExtensionPromptedAt`.

## Deliberately NOT copied

- **`~/.factory/settings.json`** (both OS) — real file, contains a
  live API key. See `settings.json.example` above instead.
- **`~/.factory/auth.v2.file`, `auth.v2.key`, `auth.json`, `*.key`,
  `certs/`** — credential material. Existence noted, contents never
  read or copied, per task constraints.
- **`~/.factory/history.json`** — a flat list of
  `{command, timestamp, type: "slash_command"|"message", mode}`
  entries recording every raw prompt/command the user ever typed
  (28 entries in the captured corpus, e.g. `{"command":"hi",...}`).
  Content was benign in this corpus (grepped clean of secrets/emails)
  but this file is the droid analogue of cline-cli's excluded
  `user_input_history.jsonl` — a raw prompt-history log, not a
  session/tool-call artifact. Following the cline-cli precedent
  (`docs/clinecli-adapter.md` "NEVER reads ... user_input_history.jsonl"),
  it is out of scope for the adapter and not copied here; shape
  documented above for completeness only.
- **`~/.factory/cache/session-discovery-index.json`** — regeneratable
  secondary cache (session id → path/title/owner/cwd/fingerprints).
  Not copied; not a durable schema surface, just a UI-speed index that
  can be rebuilt from the sessions directory itself.
- **`~/.factory/logs/`** — droid's own structured application log
  (`[timestamp] LEVEL: [Component] message | Context: {...}`), used
  only during Phase-0 research to confirm outbound hosts
  (`api.openai.com`, `auth.factory.ai`, `downloads.factory.ai`); not a
  session data source, not copied.

## Anonymization performed

All copied session files (`sessions/**/*.jsonl`, `*.settings.json`,
`*.settings.json.bak`) were grepped for `sk-`, `Bearer `, `api_key`,
and `@` before copying. The only hits were benign: a bibtex
`@software{...}` citation, and a `droid`-side already-redacted mention
of an env var name (`export ANTHROPIC_API_KEY=[REDACTED]`) inside
quoted documentation text the assistant had read from the target
repo. No further scrubbing was needed — files are byte-for-byte copies
(verified via `md5sum` against the source) of what droid itself wrote.
The only reconstructed (non-verbatim) file is `settings.json.example`,
for the reason stated above.
