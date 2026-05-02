# Changelog

All notable changes to SuperBased Observer are documented here.

## [Unreleased]

## [1.4.28] — 2026-05-03

User-flagged correctness batch: project attribution for Windows-side
Codex data, missing time-elapsed metric on the per-message timeline,
broken days filter on the Sessions tab, and same-timestamp ordering.

### Fixed

- **Codex Windows-cwd → cross-mount translation.** Codex on Windows
  records `cwd` as a Windows path like `c:\programsx\regulation`. On
  WSL2, `filepath.Abs` doesn't recognise the drive prefix and treats
  the string as a relative path, prepending the observer's CWD. Then
  `findGitRoot` walks UP that bogus path looking for `.git` — and in
  the worst case lands on observer's own repo. The user's live DB had
  all 1,350 Codex actions misattributed to
  `/home/marmutapp/superbased-observer`. The codex adapter now
  translates Windows-style cwds via the new
  `crossmount.TranslateForeignPath` helper before resolving (`c:\…` →
  `/mnt/c/…` on Linux). Post-fix smoke: all 1,350 actions correctly
  attributed to `/mnt/c/programsx/regulation`.

- **Backfill: `--codex-project-root`.** New backfill (also wired into
  `--all`) re-reads each codex rollout's first `session_meta` line,
  applies the cwd translation, resolves the new project, and UPDATEs
  every cascaded session/action row whose current `project_id`
  differs. Walks crossmount-resolved homes so Windows-side rollouts
  reachable via `/mnt/c/Users/*/.codex` are picked up. Idempotent.

- **Sessions tab now honours the global days filter.** `/api/sessions`
  accepted only `tool` and `project` — the dropdown's `days=N` was
  silently dropped, so a 30-day window still rendered every session
  in history. Endpoint now accepts `days` (alongside the existing
  filters); `loadSessions()` forwards the global window. Total /
  scored counts share the same WHERE clause so pagination stays
  coherent. `days=0` (or omitted) preserves the prior "no time
  filter" behaviour for older callers. Smoke: 30-day window on the
  user's DB returned 157 of 210 total sessions; 7-day returned 23.

- **Per-message timeline: tie-break ordering on equal timestamp.**
  When a synthesized user_prompt and its assistant turn shared a
  wall-clock (the proxy / adapter often stamps both with the same
  millisecond), `sort.SliceStable` preserved insertion order, which
  was non-deterministic w.r.t. role. Comparator now explicitly
  prefers `role=user` on ties so the timeline reads "user said X →
  assistant did Y" consistently.

### Added

- **Per-message wall-clock elapsed metric.** New `elapsed_ms` field
  on `/api/session/<id>/messages` rows: gap from this message's
  timestamp to the next message's, computed across the full sorted
  timeline (so rows near a page boundary still get a correct
  successor). Surfaced as a new "Elapsed" column on the per-message
  timeline. For user rows it approximates "time the assistant took to
  respond"; for assistant rows it approximates "time the user took
  before the next prompt". `null` on the final message in a session.

- **Per-message tool-execution time + per-tool-call duration.**
  Companion field `tool_duration_ms` sums the contained tool_calls'
  `actions.duration_ms`, surfaced as a "Tool time" column. Per-tool
  rows in the expand panel now show their individual duration in
  square brackets. Differs from `elapsed_ms` (wall-clock between
  messages) by excluding model-think time and user typing time.

- **Adapter coverage for `actions.duration_ms`.** Pre-fix only
  copilot-modern (via `elapsedMs`) and a subset of codex paths
  (legacy `duration: {secs, nanos}` struct) populated `duration_ms`.
  Live coverage: claude-code 0/42,338; codex 0/1,350. Two
  fixes:

  - **claude-code:** when matching a `tool_use` block to its later
    `tool_result`, compute the wall-clock gap and write it to
    `DurationMs`. Anthropic's JSONL has no structured per-tool
    elapsed field so the timestamp delta is the only signal.
  - **codex `response_item`:** when matching `function_call` (or
    `custom_tool_call`) to its `function_call_output`, fall back to
    the timestamp gap when no structured duration is on the row
    yet. Newer codex builds bury the duration in flat-text output
    (`Wall time: 32.3 seconds`) or in nested JSON metadata
    (`{"output":"…","metadata":{"duration_seconds":1.1}}`); the gap
    works for both without parsing variant-specific fields.

  Post-fix coverage on the user's live DB: claude-code 91.1%
  (34,965/38,383), codex 86.3% (1,165/1,350).

- **`actions.duration_ms` refreshes on conflict.** Adapter
  improvements above only populate `duration_ms` on INSERT; the
  pre-existing `INSERT OR IGNORE` semantics meant historical
  rows would stay at zero forever even after a `backfill --all`
  rescan. The insert now uses `ON CONFLICT(source_file,
  source_event_id) DO UPDATE SET duration_ms = CASE WHEN
  excluded > 0 AND existing IS NULL/0 THEN excluded ELSE
  existing END` — picks up new values from a re-parse while
  protecting any value that's already populated. Smoke
  simulation: zeroed durations on a session went from 0/210 →
  286/294 (97.3%) after `backfill --all`. Pattern mirrors
  v1.4.27's `token_usage.model` upsert. Side-effect: the
  `InsertActions` return value (and the `ActionsInserted`
  metric on `IngestResult`) now reports "rows touched" rather
  than "actually new rows" because SQLite's `RowsAffected`
  doesn't distinguish insert from update on conflict; the
  contract remained "no row duplication", which is now
  asserted via `SELECT COUNT(*)` in tests.

- **"All time" option on the global window dropdown.** The
  dashboard toolbar previously offered only 7 / 30 / 90 / 365
  days. The new option sends `days=36500`. Every relevant
  endpoint's `intArg(r, "days", 30, 1, 365)` clamp had its
  upper bound bumped to `36500` (~100 years) so the value
  passes through unmodified. `/api/sessions` also accepts
  `days=0` as an explicit "no time filter" sentinel for older
  CLI / API consumers.

### Schema

No migrations.

## [1.4.27] — 2026-05-02

Modern VS Code Copilot Chat support. v1.4.26 noted that the legacy
debug-log auto-flip is irrelevant to users on Copilot Chat ≥0.45,
which writes to entirely different files in a snapshot+patches JSONL
format. This release ingests that format directly.

### Added

- **Modern Copilot Chat parser.** Single Copilot adapter now dispatches
  on path shape between the existing legacy `debug-logs/.../main.jsonl`
  scanner and a new snapshot+patches parser for:
  - `<workspaceStorage>/<ws>/chatSessions/<sessionId>.jsonl` —
    workspace-bound chats.
  - `<globalStorage>/emptyWindowChatSessions/<sessionId>.jsonl` —
    chats opened with no folder attached.

  Each line is one JSON object with a `kind` discriminator. Three line
  types are handled:

  - `kind=0` — full session snapshot. Multiple snapshots can appear in
    one file (VS Code rewrites the snapshot when a session reopens);
    the parser keeps only the LAST one and discards earlier ones'
    accumulated patches.
  - `kind=1` — replace the value at JSON-pointer-style path `k` with
    `v`.
  - `kind=2` — splice insert: insert the elements of `v[*]` into the
    array at path `k` starting at index `i`, or append when `i` is
    omitted. This is how VS Code adds NEW turns to `requests[]` after
    the snapshot was written; without handling it, every turn after
    the first would be silently lost.

  All patch lines are filtered on `k[0]=="requests"` so
  `inputState/attachments` patches drop without ever materializing
  multi-MB inline image payloads (the user's data has 7.8 MB and 14.8
  MB attachment patches that would otherwise dominate memory).

  Idempotence keys on `requestId` (stable across snapshot rewrites)
  via the existing `(source_file, source_event_id)` UNIQUE constraint;
  `fromOffset` is intentionally ignored because snapshot replay has no
  meaningful resumption point.

  **Model resolution.** `Model` now prefers
  `requests[].result.metadata.resolvedModel` — the actual upstream
  model that processed the turn (e.g. `claude-haiku-4-5-20251001`,
  `grok-code-fast-1`) — over `requests[].modelId`, which usually
  carries the user-facing routing choice (`copilot/auto`) when the
  user hasn't pinned a specific model. `modelId` is the fallback when
  `resolvedModel` is absent (e.g. snapshot written before the result
  patch landed).

  **Per-turn token mapping.**
  - `result.metadata.promptTokens` → `InputTokens`
  - `completionTokens` (fallback `result.metadata.outputTokens`) →
    `OutputTokens`
  - Sum of `result.metadata.toolCallRounds[*].thinking.tokens` →
    `ReasoningTokens`
  - Cache token counts are NOT exposed by this format (VS Code only
    emits `cacheType:"ephemeral"` markers on rendered context blocks,
    no count) — same gap as legacy.

### Changed

- **Token usage rows refresh `model` on conflict.** Previously
  `INSERT OR IGNORE INTO token_usage` silently dropped re-inserts,
  so a row first written with a placeholder model
  (`copilot/auto`) kept that label even after a re-scan with an
  improved adapter resolved the actual upstream model
  (`claude-haiku-4-5-20251001`). The insert now uses
  `ON CONFLICT(source_file, source_event_id) DO UPDATE SET
  model = COALESCE(NULLIF(excluded.model, ''), token_usage.model)`,
  which propagates new model values without ever clobbering an
  existing one with an empty string. Counts, cost, source, and
  reliability are still preserved on conflict — they are
  quality-sensitive and a re-parse must not overwrite a
  proxy-accurate value.

- **Dashboard messages: user_prompt rows show per-turn model.**
  When the dashboard's `/api/session/<id>/messages` endpoint
  synthesizes a row for a `user_prompt` action (which has no
  `token_usage` row to join against), it now looks up the peer
  `assistant:<requestId>` row and inherits its model. Falls back
  to `sessions.model` only when the peer's model is empty. Pre-fix
  every user prompt in a multi-turn session displayed the FIRST
  turn's model (`sessions.model` is set once on session creation),
  which surfaced as a real bug whenever Copilot Auto routed
  different turns to different upstream models.

- **Tool-name normalization for camelCase.** `mapToolName` now
  collapses camelCase (`runInTerminal`, `replaceStringInFile`,
  `editFiles`, `viewImage`, `fileSearch`, ...) and snake_case
  (`run_in_terminal`, `replace_string_in_file`, ...) variants into
  a single key by lowercasing and stripping underscores, so the
  legacy and modern formats share one mapping table without parallel
  entries. New entry: `viewImage` → `read_file`.

- **`globalStorage/emptyWindowChatSessions` watch root.** Per-OS
  variant added to `defaultRoots` alongside the existing
  `workspaceStorage` root.

### Smoke

Live test against the user's WSL2 host on 2026-05-02 against
`/mnt/c/Users/auzy_/AppData/Roaming/Code/User/...`:

- 5 modern session files cursored: 3 workspace `chatSessions`, 2
  `emptyWindowChatSessions` — including a 7.8 MB and a 14.8 MB
  attachment-heavy file, both processed without crash.
- 13 Copilot actions captured (2 user prompts + 2 task_complete + 7
  read_file + 2 search_files), 2 token rows.
- 0 Copilot parse warnings.

Recipe in `docs/copilot-modern-smoke-test.md`.

### Schema

No migrations.

## [1.4.26] — 2026-05-02

WSL2/Windows quality bundle: cross-platform tool discovery, watcher
self-recovery, and one-shot enabling of VS Code's legacy Copilot
debug-log writer.

Note on Copilot: the auto-flip targets the legacy `debug-logs/...
main.jsonl` writer. Modern VS Code Copilot Chat (≥0.45) writes to
`workspaceStorage/<ws>/chatSessions/*.jsonl` and
`globalStorage/emptyWindowChatSessions/*.jsonl` in a snapshot+patches
format observer doesn't yet parse — this is queued for v1.4.27. See
`docs/copilot-modern-format.md`.

### Added

- **Cross-mount adapter discovery for WSL2 ↔ Windows installs.** Pre-fix
  observer running in WSL2 only inspected `/home/<u>/...`; tools
  running on the Windows side (Codex, Claude Code, Copilot Chat,
  Cline, Roo Code, OpenClaw, Pi) were silently invisible because
  their data lives at `/mnt/c/Users/<u>/...`. Symmetric problem on
  the reverse: observer in Windows native couldn't see WSL distro
  homes. With this fix:

  - On WSL2 (any Linux host where `/mnt/c/Users` is statable), every
    directory under `/mnt/c/Users` becomes a candidate Windows home.
  - On Windows (any host where `\\wsl.localhost\` is enumerable),
    every `<distro>/home/<user>` becomes a candidate Linux home.
  - On macOS / pure Linux / pure Windows hosts, no extras are
    detected — behavior is identical to pre-fix.

  Each adapter's `WatchPaths` / `defaultRoots` now expands to the
  appropriate subpath under every detected home, with per-OS
  branching where the subpath differs (Copilot/Cline VS Code
  globalStorage, OpenCode desktop variants). Adapters with uniform
  subpaths (Codex, Claude Code, OpenClaw, Pi) just iterate.

  Auto-detection by default; no config knob needed for the common
  case. Detected extras are logged at INFO at startup so the user
  can confirm what was picked up:

      crossmount: detected extra home path=/mnt/c/Users/<u> os=windows origin=wsl-mnt:<u>

  Verified end-to-end on a WSL2/Windows install: 173 of 525
  ingested files came from cross-mount paths post-fix; codex went
  from 0 → 1,350 actions captured, claude-code grew by ~6,500
  Windows-side actions.

  New package `internal/platform/crossmount` with full table-driven
  tests for both bridge directions (using a fakeFS seam — no host
  dependencies).

  **Copilot debug flag is now auto-enabled.** VS Code only writes
  substantive Copilot Chat debug records when
  `github.copilot.chat.advanced.debug` is `true` — pre-fix this was
  off by default on most installs and observer captured nothing
  even after cross-mount discovery found the file. Observer now
  flips the flag automatically across every cross-mount-resolved
  VS Code install at startup, with a loud INFO log identifying the
  exact file and prior value:

      copilot.setup: enabled github.copilot.chat.advanced.debug path=/mnt/c/Users/<u>/.../settings.json prior_value=missing

  Idempotent: subsequent starts log nothing because the flag is
  already true. JSONC-aware (preserves comments and trailing commas
  via `github.com/tailscale/hujson`); preserves the user's existing
  indent style (4-space, 2-space, or tab — auto-detected from the
  file). Tail formatting and member ordering unchanged. Atomic
  write via temp file + rename. See `docs/copilot-setup.md`.

### Fixed

- **Watcher polling fallback recovers from fsnotify event drops.**
  fsnotify is documented to drop Write/Create events on busy or
  virtualized filesystems — WSL2 reading a Windows NTFS mount,
  network FUSE, certain editor write patterns. When that happened,
  the watcher silently sat behind a growing JSONL until the user
  clicked Run All. The dashboard's `⚠ Watcher is behind on N
  file(s)` banner surfaced the state but didn't fix it.

  Fix: a polling goroutine runs alongside the fsnotify event loop
  inside `Watcher.Watch`. Every `[observer.watch].poll_interval_seconds`
  tick (default `2`) it stat()s every known `parse_cursors` row and
  re-runs `processFile` whenever the file has grown past the saved
  offset. Every 15th tick (~30s by default) it does a full root
  walk to also catch never-seen-before files (the same bug class
  for fsnotify Create drops). Disabled by setting
  `poll_interval_seconds = 0`.

  The poll path reuses the existing idempotent ingest:
  `(source_file, source_event_id)` UNIQUE keeps duplicate inserts
  no-ops; `SetCursor` uses MAX() so a poll and an fsnotify-debounced
  fire racing on the same file can never regress the cursor. Logs
  at INFO only when a poll actually advances a cursor — steady-
  state polling is silent. Closes the kickoff's "live-watcher
  reliability on Windows/WSL2" item.

  New: `store.ListCursors` / `store.CursorEntry` + new
  `watcher.Options.PollInterval`. Threaded from
  `cfg.Observer.Watch.PollIntervalSeconds`, which already existed
  in config but wasn't wired anywhere.

## [1.4.25] — 2026-05-01

Three real-data correctness fixes user-flagged after testing v1.4.24
on live codex sessions and the dashboard.

### Fixed

- **Codex `event_msg/token_count` dedup by `total_token_usage`.**
  User-reported inflation: Observer's per-session JSONL token sums
  exceeded Codex's own final cumulative `total_token_usage` figure
  by ~12% on a real rollout (input +122,680, cache_read +88,704,
  output +731, reasoning +279). Root cause: Codex's runtime
  re-emits identical `event_msg/token_count` records (same
  `last_token_usage` AND `total_token_usage`, 2-3s apart). Pre-fix
  the adapter summed both; the second event double-counts. Total is
  monotonic, so any non-advancing total is a re-emission. The
  modern dispatch now tracks the last total fingerprint per
  `SessionID` and skips duplicates. Verified on the user's
  rollout-2026-04-23T00-29-51-019db690 file: post-fix sums match
  Codex's own final cumulative for all four buckets exactly.

- **Watcher-behind banner no longer shows orphan parse_cursors.**
  Pre-fix the `⚠ Watcher is behind on N file(s)` banner included
  paths whose adapter version had been tightened (e.g. older
  copilot adapter matched any `.log` under `GitHub.copilot-chat/`,
  current adapter narrowed to `/debug-logs/main.jsonl`). The
  parse_cursors rows from the broader-match era stayed in the DB;
  the health endpoint reported them as "behind"; Run All couldn't
  recover them because Rescan only walks paths a CURRENT adapter
  recognises. Banner sat forever.

  Fix: `dashboard.Options.RecognizesSessionFile` is a new predicate
  built from the unified `defaultAdapters()` list (extracted from
  `buildWatcher`). The health endpoint tags rows the predicate
  rejects with `orphan_unmatched: true`, lists them in the response
  but EXCLUDES them from `behind_count`. The JS banner filters them
  out. Banner now only fires on genuinely recoverable issues.

- **Sessions-tab + Analysis-Top-sessions ID columns gained the
  v1.4.24 click-to-copy affordance.** v1.4.24 only updated four of
  six ID renders; the main `#sessions-table` ID column rendered
  `<code>aabbcc…</code>` with no `title` and no idCopy, and the
  Analysis tab's `#analysis-top-sessions-table` had a hand-built
  title without idCopy. Both now use `idCopy()` for consistent
  hover + click-to-copy behaviour.

- **Modal overflow belt-and-braces.** Added `overflow-x: hidden` on
  `.modal` and explicit `width: 100%` (not just `max-width`) on
  `.session-messages-scroller`. The previous fix only added
  `max-width: 100%` which has no effect when the parent container
  has no definite width — the scroller followed the table's
  intrinsic width, defeating the scroll constraint.

### Added

- **Codex `response_item.message.role=user` envelope capture.** User
  pointed out a payload with body
  `<environment_context>\n  <cwd>...</cwd>\n  ...\n</environment_context>`
  was silently dropped. v1.4.23 only handled `role=developer` because
  `role=user` overlapped with `event_msg/user_message` (real user
  prompts). Corpus analysis: 118 user-role response_items split
  ~80% plain text (real prompts already covered) / ~17%
  XML-envelope synthetic context injections that look like user
  messages but originate from the runtime. New discriminator:
  trimmed body must start with `<` to qualify as system-prompt-shaped;
  otherwise stays with `event_msg/user_message`. Rows tag with
  `role=user-envelope` so analysts can split synthetic context
  from real user input.

## [1.4.24] — 2026-05-01

UX + reliability: dashboard affordances for truncated content,
session-detail pagination, npm install clarification, and a
proxy-side fix for the "connection reset by peer" upstream errors.

### Fixed

- **Proxy retries once on transient transport errors.** The user
  reported `write tcp ...: connection reset by peer` from the proxy
  forwarding to api.anthropic.com. Root cause: stale keep-alive
  entry in the http.Transport pool — WSL2 / corporate-firewall /
  mobile-hotspot NATs close idle TCP streams faster than our
  pre-fix 90s `IdleConnTimeout`. Two-part fix: (a) tighten
  `IdleConnTimeout` 90s → 30s + cap `MaxIdleConnsPerHost=16`;
  (b) new `doWithRetry` helper retries exactly once on
  {connection reset by peer, broken pipe, use of closed network
  connection, EOF} after closing pooled idle conns. Non-transient
  errors (TLS handshake, dial timeout) bypass retry.

- **Session-detail messages table no longer pushes the modal past
  its bounds.** Wrapped the table in a `.session-messages-scroller`
  with `overflow-x: auto`; expand row (the "N ▾" drop-down's
  content) gets `white-space: normal; word-break: break-word` so
  long target / error text wraps inside the cell instead of
  inheriting the global `td { white-space: nowrap }` and forcing
  horizontal overflow.

### Added

- **Session-detail Messages pagination.** Pre-v1.4.24 the panel
  rendered every message in one go — for large sessions this was
  crashing the browser tab. `handleSessionMessages` now accepts
  `?limit=N&offset=M` (default limit=100, limit=0 for unlimited);
  response shape gains `{total, limit, offset}`. Frontend gets
  prev/next buttons + a 50/100/200 page-size selector. Filter
  ("Tool messages only" vs "All messages") stays client-side and
  applies after server pagination.

- **Click-to-copy on truncated IDs.** Every truncated session_id /
  message_id render across the dashboard (Sessions tab, Actions
  tab, Compression-events tab, Session-detail Messages panel) gets
  a hover-discoverable affordance (dotted underline, cursor:copy)
  and a click handler that writes the full value to the clipboard.
  Single delegated handler keeps the per-row JS work negligible
  even on tables with hundreds of rows.

- **Click-to-expand on truncated text.** Long Target / Error /
  Command / File-path values in the Actions, Discovery, and
  Session-detail tabs render with a `.expandable` wrapper. Click
  toggles between truncated and full text in-place; full value
  lives in `data-full` so no extra fetch is needed.

### Documentation

- **npm README clarifies global-vs-local install.** Pre-fix the
  Install section showed `npm install -g` followed by `observer
  --version` without explaining what happens for users who install
  locally (`npm install` without `-g`) — in that case the binary
  lives at `./node_modules/.bin/observer` and isn't on `$PATH`,
  so the `observer init` / `observer start` examples fail with
  "command not found". README now lists both forms (global +
  `npx observer ...` for local) and cross-links the Troubleshooting
  → EACCES fix for shared / CI machines.

## [1.4.23] — 2026-05-01

Cross-adapter message normalization, Tier 3: system-prompt and
bootstrap-context capture across codex and openclaw. One new
ActionType (`ActionSystemPrompt`); content-hash dedup keeps the DB
size bounded despite codex's 9-18KB prompts being repeated across
nearly every session_meta and turn_context record.

### Added

- **New ActionSystemPrompt constant** in `internal/models/models.go`,
  symmetric to ActionUserPrompt. Adapters emit one row per unique
  prompt body per session; MessageID is "system:<content_hash>" so
  cross-row queries can group occurrences of the same prompt.
  Adapters MUST hash-dedup or the DB would gain hundreds of
  identical rows per session.

- **Codex system prompt capture (3 sources, hash-dedup'd within parse).**
  - `session_meta.base_instructions.text` — the base Codex system
    prompt (~18KB), repeated verbatim in every session_meta record.
    Emit once per unique body, role="base".
  - `turn_context.developer_instructions` — per-turn permissions /
    sandbox / context envelope (~9KB), nearly identical across all
    turns in a session. Emit once per unique body, role="developer".
  - `response_item.payload.type=message` + `payload.role="developer"`
    — mid-turn system instruction injections. Same dedup behaviour;
    assistant + user roles still skipped here because event_msg/
    agent_message + event_msg/user_message already cover those.

- **OpenClaw bootstrap-context custom event.** Pre-v1.4.23 the
  adapter had no `case "custom"` at all — both customType variants
  in real corpora were silently dropped. This commit handles
  customType="openclaw:bootstrap-context:full" by emitting an
  ActionSystemPrompt row carrying the marker payload; "model-snapshot"
  stays no-op'd because model_change already lifts that info.

### Smoke results vs real samples

The 1614-event codex session captures **+11 unique system_prompt
rows** despite the underlying source repeating identical prompts
across hundreds of records. Smaller sessions: 2-3 system_prompt rows
each. Zero parse warnings across all 9 sample rollouts.

## [1.4.22] — 2026-05-01

Cross-adapter message normalization, Tier 2: feature-parity work
that captures meaningful action/event types previously dropped on
the floor. No schema changes; two new ActionType constants
(`ActionTurnAborted`, `ActionContextCompacted`).

### Added

- **Codex `event_msg/mcp_tool_call_end` capture.** Executor side-
  channel for MCP tool calls (server, tool, structured arguments,
  duration, result.Ok|Err). Pre-v1.4.22 the adapter's event_msg
  switch had no case for it: paired calls (Tier 1
  response_item.function_call(list_mcp_resources*)) kept the call's
  terse data; unpaired calls were dropped entirely. Now merges into
  the pending row with Target="server:tool", ToolOutput from
  Ok.content[*].text, Success from Ok.isError=false vs Err.message,
  DurationMs from secs+nanos. Standalone path emits a fresh row.

- **Codex `event_msg/turn_aborted` → ActionTurnAborted.** New
  ActionType for turns interrupted before the model finishes
  generating (user pressed esc / cancelled). Distinct from
  task_complete with success=false: aborted turns never finished, so
  output is partial — the discriminator matters for cost analysis
  (aborts still consumed input/output tokens up to the abort point).

- **Codex `event_msg/view_image_tool_call` capture (merge +
  standalone).** Pre-v1.4.22 the paired form had stale RawToolName
  and the standalone form was dropped. Also fixes a Tier 1
  oversight: `view_image` was in actionMap but missing from
  extractTarget, so Tier 1 view_image rows had empty Target.

- **Codex `event_msg/dynamic_tool_call_request` + response
  pairing.** Runtime-loaded tools (e.g. load_workspace_dependencies).
  Field-name quirk: request uses camelCase (callId, turnId), response
  uses snake_case (call_id, turn_id) — both spellings tolerated.
  Same pattern as exec_command_end / patch_apply_end / mcp_tool_call_end:
  request creates a row, response merges in success / error /
  duration / content_items text body.

- **Codex `compacted` events → ActionContextCompacted (non-
  searchable).** Top-level type="compacted" event Codex emits when
  the model decides to summarize earlier turns. New ActionType,
  distinct from the searchable file-edit set so dashboard filters
  can suppress these rows from action-type browsers while keeping
  them queryable for cost / compaction-frequency analytics. Row
  Target is "<N> msgs, ~<T> tokens"; RawToolInput is JSON
  {messages, bytes_estimate, tokens_estimate} for analytics. The
  paired event_msg/context_compacted is no-op'd to avoid double-
  emission. Per user direction (2026-05-01): "doesn't need to be
  searchable like file edits" — but DO capture token/event info.

- **Codex `response_item.reasoning` forward-compat capture.**
  Current Codex Desktop builds always emit summary:[] (838 reasoning
  items, 0% non-empty in the corpus). The adapter now extracts text
  from summary[*].text when present and threads it into the turn's
  agentMessages cache so future builds (or summary-populating
  variants) inherit it as PrecedingReasoning without further
  changes.

- **OpenClaw `stopReason='error'` → ActionAPIError.** OpenClaw
  assistants emit empty-content messages with stopReason="error"
  + an errorMessage carrying the upstream provider's verbatim
  response (e.g. `400 {"error":"...does not support tools"}`).
  Pre-v1.4.22 the adapter's stop-reason gate only fired on "stop"
  so these were silently dropped. Now emits an ActionAPIError row
  with status-code-prefix discriminator (`http_400` etc.).

### Smoke results vs real samples

The largest inspected codex session (1586 events) now captures
**+17 previously-dropped rows**: 9 context_compacted, 2 turn_aborted,
6 api_error. Zero parse warnings across all 9 codex sample rollouts.

## [1.4.21] — 2026-05-01

Cross-adapter message normalization, Tier 1: closes data-loss bugs
in the codex and cursor adapters that were silently dropping
significant fractions of real-session activity. No schema changes;
new actions enrich what was already a partial view.

### Added

- **Codex `response_item` envelope dispatch.** Codex Desktop wraps
  every assistant tool intent in a `response_item` envelope
  (`payload.type` discriminates `function_call`, `function_call_output`,
  `reasoning`, `message`, `custom_tool_call`, `custom_tool_call_output`,
  `web_search_call`). The adapter previously had no `case "response_item"`
  at all, so on real Desktop sessions the entire `function_call` /
  `function_call_output` stream (~1613/1612 events per inspected
  corpus) was silently dropped — only the side-channel
  `event_msg/exec_command_end` caught ~⅔ of shell calls, leaving
  every `update_plan`, `view_image`, and `list_mcp_resources` call
  missing entirely.

  New `case "response_item"` routes function_call through the same
  `pending[call_id]` machinery as the legacy top-level dispatch.
  Dedup logic in `event_msg/exec_command_end` and
  `event_msg/web_search_end`: when a response_item.function_call
  already landed for the same call_id, the side-channel merges its
  richer fields (command, exit_code, duration, stdout, query) into
  the existing row instead of emitting a duplicate. If only the call
  landed (mid-session truncation, user interrupt) the row stands
  alone; if only the side-channel landed (resume mid-file) the
  legacy emit path handles it. **No double-counting in either
  direction.**

- **Codex `apply_patch` capture (`custom_tool_call` +
  `patch_apply_end`).** Codex Desktop's apply_patch flow writes
  through three separate JSONL events sharing a `call_id`:
  response_item/custom_tool_call (assistant intent + raw patch text),
  event_msg/patch_apply_end (executor result with structured
  `changes` map), and response_item/custom_tool_call_output (string-
  wrapped {output, metadata}). In real corpora `patch_apply_end` lands
  BEFORE `custom_tool_call_output` so the previous step's pattern of
  deleting the pending entry on the *_output event was wrong here —
  patch_apply_end is now treated as the terminal event for apply_patch
  and merges in its structured fields. ~166 patch_apply_end events per
  representative corpus now land as ActionEditFile rows.

- **Codex `event_msg/error` → ActionAPIError.** Codex emits a
  structured event when an upstream API call fails before a turn can
  complete (`usage_limit_exceeded`, rate limit, content-policy block,
  etc.). Pre-v1.4.21 these were silently dropped — same gap claudecode
  had pre-v1.4.20. Mirrors claudecode's ActionAPIError shape.

- **Cursor user_prompt emission from JSONL transcripts.** Cursor's
  agent runtime wraps user prompts in `<user_query>...</user_query>`
  XML before passing them to the model; this wrapper landed verbatim in
  rows on the live-hook path (`beforeSubmitPrompt`). New
  `stripUserQueryWrapper` helper applied in both the live-hook path
  and a new `BuildTranscriptUserPromptEvent` exported function. Strip
  ONLY when both opening and closing tags are present so partial-
  wrapper text (a user typing `<user_query>`) is not damaged.

- **Cursor sub-agent transcript ingestion.** Cursor writes a separate
  JSONL when the parent agent spawns a sub-agent
  (`agent-transcripts/<parent>/subagents/<sub>.jsonl`). Pre-v1.4.21
  these were explicitly skipped — the parent transcript only recorded
  a `Subagent` tool_use; the sub-agent's actual fan-out work
  (WebFetch, ReadFile, sub-prompts) was lost. Now ingested as
  IsSidechain=true rows under the parent session_id, mirroring
  claudecode's sidechain semantics.

- **Cursor tool-name normalizer extensions.** Real cursor transcripts
  contain tool names previously routed to ActionUnknown:
  `ReadLints` → ActionReadFile, `StrReplace` → ActionEditFile,
  `Subagent` (capitalized) → ActionSpawnSubagent (case-insensitive),
  `call_mcp_tool` → ActionMCPCall (parity with the live-hook MCP path),
  `Await` → ActionUnknown (kept Unknown intentionally — control-flow
  primitive with no file/command target).

### Changed

- **CLI flattened**: `observer scan`, `observer watch`, `observer init`,
  `observer uninstall`, `observer serve`, `observer doctor`,
  `observer status`, `observer tail`, `observer prune`, `observer cost`,
  `observer score`, `observer discover`, `observer patterns`,
  `observer learn`, `observer suggest`, `observer dashboard`,
  `observer metrics`, `observer summarize`, and `observer export` are
  now top-level subcommands. The legacy `observer observer <sub>`
  nesting is preserved as a hidden alias group so installed hooks and
  MCP entries from earlier versions keep working without re-init.
- **MCP registration writes `["serve"]` instead of `["observer","serve"]`.**
  Existing entries continue to work via the alias; to migrate to the
  flat form, run `observer uninstall && observer init` (or
  `observer init --force`).

### Fixed

- `internal/adapter/copilot/adapter.go`: `IsSessionFile` now matches
  Windows-formatted fixture paths on Linux hosts (the `filepath.ToSlash`
  no-op on `\` separators caused `TestAdapter_IsSessionFile` to fail
  under `make test`).
- `gofmt -w` on five files dirty since `bb815b5`.

## [1.4.20] — 2026-04-30

Long-context (LC) pricing modeling, full Analysis dashboard tab, and
fully-editable Settings tab with backfill controls. Largest single
release since v1.0 — see PROGRESS.md for the per-section detail.

### Added

- **Long-context pricing tier in the cost engine.** `Pricing` struct
  gains `LongContextThreshold` + per-dimension LC rates (Input,
  Output, CacheRead, CacheCreation, CacheCreation1h). `Compute()`
  now reprices an entire turn at the LC tier when its prompt window
  (`Input + CacheRead + CacheCreation`) exceeds the threshold —
  closes the under-billing gap for Anthropic Sonnet 4 / 4.5 (>200K),
  OpenAI gpt-5.4 / 5.5 (>272K), and Gemini 2.5 Pro / 3.1 Pro Preview
  (>200K). Defaults baked in for every affected SKU; user overrides
  via TOML or the new Settings → Pricing form. Each LC field falls
  back to its standard counterpart when zero so an entry can pin
  only the dimensions that actually change at the LC tier.

- **Analysis dashboard tab.** New tab between Cost and Sessions with
  four bands keyed off a single time-window picker:
  - **Headline KPIs** (6 tiles) — period vs prior period (with Δ%
    colour-coded), month-to-date + linear projection (+ optional
    budget % when `intelligence.monthly_budget_usd` is set),
    effective rate ($/1M output + cache-write tokens), cache
    efficacy (`cache_read / (cache_read + cache_creation)`), LC tier
    surcharge attribution (turns + extra $ from LC repricing), and
    waste $ (Discovery stale-read tokens × blended input rate).
  - **Trend / cross-session deep-dive** — daily stacked-bar with a
    Model / Project / Tool dimension toggle. New cost-engine
    groupings `GroupByDayProject` + `GroupByDayTool` mirror the
    existing `GroupByDayModel`.
  - **What changed / movers** — top 5 cost increases, top 5
    decreases, and new entrants for the chosen dimension.
    `cost.Options.Until` adds a closed-window upper bound so the
    prior-period query is a clean `[Since, Until)` window.
  - **Top expensive sessions + routing efficiency** — sessions
    sorted by cost with `opus` / `lc_tier` / `many_turns` /
    `large_prompt` badges, and a soft "you might have used Sonnet"
    table for trivial Opus sessions (small prompt, low output, no
    LC turns) flagged by a conservative work-profile heuristic.
    Click-through to existing session detail modal.

  Six new endpoints under `/api/analysis/*`: `headline`, `trend`,
  `movers`, `top-sessions`, `routing-suggestions`. All do per-turn
  pricing (LC dispatch is per-request — aggregating tokens at SQL
  before pricing would false-trip the LC tier).

- **Settings dashboard tab.** Last in nav, two-column shell with a
  rail and 10 sections:
  - **Pricing** — table-form editor (rows = active overrides, cols =
    Input/Output/CacheRead/CacheCreation 5m/CacheCreation 1h, with a
    chevron toggling 6 long-context fields per row). Filter input,
    Add Override prompt (auto-fills from baked-in defaults when the
    model id matches), per-row Reset + Delete buttons. Defaults
    reference list at the bottom (collapsed `<details>`, all 95
    baked-in models with an "Override" shortcut). Saves hot-reload
    via `cost.Engine.Reload` — no restart needed.
  - **Backfill** — table of all 14 documented `observer backfill`
    flags with candidate counts (SQL-checkable) or "scan needed"
    (file-walking). Per-row Run button + Run All. Output panel
    surfaces captured stdout incrementally as the subprocess runs;
    multiple modes can run concurrently.
  - **Observer / Watcher / Freshness / Retention / Hooks / Proxy /
    Compression / Intelligence** — per-field forms driven by
    `SECTION_FIELDS` field specs (kind / path / label / help /
    placeholder / select options). Compression keeps a 4-card layout
    matching its sub-struct shape; the others are flat. Save → file
    is rewritten via the same `.bak`-fallback + atomic-rename path
    used for pricing; a "Restart daemon" banner appears at the top
    of the tab with a confirmation dialog.

  Endpoints: `GET /api/config`, `PUT /api/config/pricing`,
  `GET /api/config/pricing/defaults`, `PUT /api/config/section/<name>`,
  `POST /api/admin/restart`, `GET /api/backfill/status`,
  `POST /api/backfill/run`, `GET /api/backfill/jobs/<id>`.

- **API error capture — both JSONL and proxy paths.** Pre-v1.4.20
  upstream API failures (content-policy blocks, rate limits,
  overloaded responses, invalid-request errors) were silently
  dropped on both surfaces: the proxy returned early on non-2xx
  responses, the claudecode adapter skipped `type: "system"` records
  before any system-record handling. v1.4.20 closes both gaps.

  *JSONL adapter:* new `ActionAPIError` action type. The
  claudecode adapter now decodes `type: "system"` +
  `subtype: "api_error"` records. Captured rows carry the upstream
  request_id (joinable to `api_turns.request_id` when both proxy +
  JSONL saw the same call), the specific error class
  (`overloaded_error` / `rate_limit_error` /
  `invalid_request_error`), and the human message. Recursive
  envelope walker handles the 1- and 2-level-nested shapes that
  appear in real logs. Companion `--claudecode-api-errors` backfill
  flag (umbrella'd by `--all`) recovers historical errors.
  Smoke-tested against 344 live JSONL files: 54 errors recovered,
  52 with the specific class attributed correctly.

  *Proxy:* new `api_turns.{http_status, error_class, error_message}`
  columns (migration 013). The proxy now records a zero-token
  api_turn row when the upstream returns 4xx/5xx — both
  non-streaming and streaming paths. `parseErrorBody` handles both
  Anthropic (`{type: "error", error: {…}}`) and OpenAI
  (`{error: {type, message, code}}`) envelope shapes;
  `extractStreamErrorBody` pulls error JSON out of an SSE
  `event: error` data line when the upstream errored mid-stream;
  `extractRequestID` reads `x-request-id` from the response with
  `cf-ray` fallback. `store.InsertAPITurn` validation relaxed for
  error turns (Model may be empty when HTTPStatus != 0 — some
  upstreams reject malformed requests before any model field is
  parsed).

  Sister Settings → Backfill UI gets a Run button for the new
  `--claudecode-api-errors` mode.

- **Long-context Pricing struct + ModelPricing config fields**: 6
  new TOML keys per model (`long_context_threshold`,
  `long_context_input`, `long_context_output`,
  `long_context_cache_read`, `long_context_cache_creation`,
  `long_context_cache_creation_1h`). Threading from
  `IntelligenceConfig.MonthlyBudgetUSD` via `cost.Engine.Reload`.

- **`config.ResolveGlobalPath(override)`**: mirrors the path
  resolution used by `config.Load` so callers can locate the file
  for save-back operations without reimplementing the rule. Threaded
  into `dashboard.Options.ConfigPath` from
  `cmd/observer/{start.go,dashboard.go}`.

- **`cost.BakedInDefaults()`**: returns a fresh copy of
  `defaultPricing` for the dashboard's pricing-defaults reference
  list. Mutating the returned map has no effect on engine state.

- **Watcher recovery path.** `observer scan --force` (new flag) and
  `Watcher.Rescan()` re-walk every JSONL the registered adapters
  claim from offset 0, ignoring `parse_cursors`. The
  `(source_file, source_event_id)` UNIQUE index keeps the pass
  idempotent — rows already in the DB are no-ops, anything the live
  watcher dropped silently gets ingested. `backfill --all` runs the
  rescan first (before the surgical column-update backfills) so a
  single click of "Run all" on the Settings → Backfill tab recovers
  missing data and patches new columns in one shot. Diagnostic for
  the well-known watcher-falls-behind failure mode (fsnotify event
  drops on busy sessions, daemon restart with stale cursors).

- **Watcher-health endpoint + sticky banner.**
  `GET /api/health/watcher` lists every JSONL the watcher knows
  about with its saved `byte_offset` vs the live `file_size` on
  disk, plus how stale the cursor is. The dashboard polls this on
  load and every 60 s; when any file is behind by more than 10 KB,
  a top-of-page banner appears (`Watcher is behind on N file(s)…
  click to recover via Settings → Backfill → Run all`) so the
  silent-data-loss case the v1.4.20 recovery path was built for
  can't sneak past the user again.

- **Toast feedback for Backfill Run buttons.** Generic
  `showToast(id, status, title, detail, autoDismissMs)` helper.
  Click Run → sticky toast top-right with spinner + label.
  The poll loop tails captured stdout and updates the toast detail
  line live (so Run All shows phase transitions:
  `rescan complete: files_processed=346` →
  `is_sidechain backfill complete…` → `✓ recovered N rows`).
  Done auto-dismisses after 8 s; failed stays sticky with the error.

- **Compression events table — tokens / message_id / linked
  session.** `/api/compression/events` rows now carry
  `original_tokens_est` / `compressed_tokens_est` /
  `saved_tokens_est` (computed server-side as `bytes ÷ 4`, matching
  the cost engine's `CompressionStats` heuristic), plus
  `message_id` (sourced from the joined `api_turns.request_id`).
  The dashboard table replaces the verbose
  Original/Compressed/Saved-bytes triplet with a compact
  `Saved (B)` cell (with original→compressed in the tooltip), adds
  a `Saved (tok)` column, and surfaces `Session` (clickable link
  opening the existing detail modal) and `Msg ID` columns.

- **Actions table — message_id column + linked session.** The
  `/api/actions` response now exposes `message_id` (the upstream
  Anthropic `msg_xxx` populated by the claudecode adapter + the
  `--message-id` backfill). The Actions tab adds a `Msg ID` column
  (truncated, full id in tooltip) and converts the `Session` cell
  from a static `<code>` to an accent-coloured link that opens the
  session detail modal — same affordance the Compression events
  table got.

- **`docs/compression-audit.md`** — verifies which Compression
  toggles are wired to a live consumer (Shell ✓, Indexing
  excerpts ✓, Conversation pipeline ✓) versus stubs the v1.4.20
  audit found (`compression.code_graph.*` was duplicate dead
  config — Intelligence's CodeGraph is the real toggle;
  `compression.indexing.embeddings` is an experimental hook with
  no runtime consumer yet). The Compression form removes the
  CodeGraph card and labels Embeddings explicitly as "experimental,
  not yet wired."

- **Per-section purpose blurbs in Settings.** `SECTION_BLURBS` map
  covers all 10 Settings sections — each renders a one-paragraph
  explainer above the form fields describing what the section
  controls and when you'd touch it. Prevents the
  "what is this for?" foot-gun the audit-screenshots flagged on
  Intelligence + others.

### Changed

- **Cost engine made hot-reload-safe.** `Engine.Table *Table` →
  `Engine.table atomic.Pointer[Table]` (private). New
  `Engine.Lookup` / `LookupWithSource` / `Reload(cfg)` wrappers; the
  Settings → Pricing save calls `Reload`, swapping the active table
  via `atomic.Pointer.Store`. In-flight Lookup callers see either
  the old or new table — never a torn state. External `engine.Table`
  access migrated through the wrapper methods (only call sites were
  in `analysis.go` and tests).

- **Dashboard session-detail per-model breakdown.** Previously
  aggregated tokens at SQL level then called `Compute` once on the
  sum — that would false-trip the LC tier whenever a session's
  summed prompt cleared the threshold even if no individual turn
  did. Rewrote to pull individual rows and bucket per-model in Go
  after `Compute`. Same pattern applied to the headline and
  top-sessions endpoints from the start.

- **Backfill subprocess output streams incrementally.**
  `realExecBackfill` switched from `cmd.CombinedOutput()` (all-at-
  once buffer) to `StdoutPipe`/`StderrPipe` with concurrent drain
  goroutines firing an `onChunk` callback per 4 KiB read. The
  registry appends chunks under a mutex; the existing 2 s poll
  surfaces partial output as it accumulates. Output capped at 1 MiB
  with truncation marker.

- **Pricing reference doc.** `docs/pricing-reference.md` Anthropic /
  OpenAI / Gemini sections now describe the LC modeling instead of
  flagging it as a future enhancement; only Gemini Flash long-context
  remains in the out-of-scope list.

- **Backfill table responsive layout.** Scoped `.backfill-table` CSS
  with `table-layout: fixed`, wrapped descriptions, sticky action
  column, drops the Flag column at <900px viewports. Closes the
  horizontal-scroll-on-narrow-screens issue surfaced in the v1.4.20
  audit screenshots.

- **Loading-spinner drift fixed (CSS).** The chart-panel loading
  overlay's spinner used `transform: translate(-50%, -50%)` for
  centering AND `animation: obs-spin` rotating the same property —
  every animation frame replaced the centering translate with a pure
  rotation, snapping the spinner to top-left then back, producing the
  visible drift on the Analysis tab's Daily-spend chart. Centering
  switched to `top/left: calc(50% - 14px)` so the only `transform`
  on the element is the rotation.

- **Help drawer suppressed on tab clicks.** The body-level click
  handler that opened the help drawer for any `data-help`-tagged
  element previously fired on tab-nav clicks too — every tab switch
  popped the drawer open. Now skips clicks where
  `el.closest('nav')` matches AND the element is a `<button>` with
  a `data-tab` attribute. Tab navigation is silent; hover tooltips
  on tabs still show a one-line definition; the drawer is still
  reachable via the `?` button or any non-tab `data-help` click.

- **Link colour for Session-id affordances.** New `.session-link`
  CSS rule pins the new Session link in Compression events + Actions
  tables (and the existing `.row-clickable` cells across Sessions /
  Top expensive sessions / etc.) to `var(--accent)` instead of the
  default browser blue, keeping the dark theme readable.

### Tests

- 30+ new tests across `cost/`, `dashboard/`, and adjacent packages.
  Coverage highlights:
  - LC dispatch (below/above threshold, zero-fallback, threshold-
    disable, prompt-includes-cache-read, defaults round-trip,
    config-override round-trip, Reload swap-while-reading,
    concurrent-safe under `-race`).
  - Analysis endpoints (period vs prior, LC surcharge attribution,
    cache efficacy, budget echo, trend dimensions, movers diff
    math, top-sessions ranking + badges, routing-suggestions
    heuristic correctness).
  - Settings endpoints (GET full struct, no-file-yet defaults,
    pricing save+reload cycle, no-config-path 409, retention
    section save, intelligence save preserves pricing,
    unknown-section 400, backfill 14-mode coverage, pricing
    defaults shape).
  - Backfill run (allowlist rejection, happy path, non-zero exit,
    config-path arg propagation, partial output streamed before
    exit, 1 MiB output cap with truncation marker, unknown job 404).

  Full suite passes under `-race`.

### Migration notes

- New optional config fields:
  - `intelligence.monthly_budget_usd` (float, USD): hides the
    Analysis budget tile when zero/unset.
  - `intelligence.pricing.models.<id>.long_context_threshold`
    (int) + 5 paired LC rate fields. Ignored when zero.

- No new database migrations.

- `cost.Engine.Table` is now a method (`Table()`) rather than an
  exported field. External Go callers using `engine.Table` directly
  must switch to `engine.Lookup` (recommended) or `engine.Table()`.
  Internal callers in this repo were already migrated.

- The dashboard's Settings → Pricing form preserves the `.bak`
  fallback for the prior `config.toml` on every save. Comments are
  lost when the engine re-marshals the file (Option A from the
  planning doc) — the `.bak` is the recovery path for users who
  hand-comment their configs.

## [1.2.1] — 2026-04-23

### Fixed

- **Pidbridge attribution for recent Claude Code versions.** The
  `SessionStart` hook now registers the whole non-shell ancestor chain
  (hook parent → grandparent → ... up to the shell) instead of just
  `os.Getppid()`. Recent Claude Code routes hooks through a short-lived
  Node worker; registering only the worker's PID caused every
  `api_turns` row to land with `session_id = NULL` once the worker
  exited, because the proxy's `/proc/<pid>/status:PPid` walk could not
  cross the dead worker to reach the still-live main process. Walking
  multi-step at registration time guarantees at least one still-live
  PID is in the bridge by the time the proxy looks up traffic.

## [1.2.0] — 2026-04-23

### Added

- **`observer uninstall`** — clean reversal of `observer init`. Removes
  hook entries from `~/.claude/settings.json` and `~/.cursor/hooks.json`
  and the `observer` MCP server entry from `~/.claude.json`,
  `~/.cursor/mcp.json`, and `~/.codex/config.toml`. User-authored hooks
  and other MCP servers are preserved.
- Checksum-based drift detection: uninstall refuses to touch a config
  file that has been modified since install unless `--force` is passed.
- `--purge` flag to additionally delete `~/.observer/` (observer.db,
  config.toml, hook_checksums.json).

### Changed

- Config env-var prefix finished the superbased→observer rename:
  `SUPERBASED_*` overrides now read as `OBSERVER_*` in
  `internal/config/config.go`. Doc comments and test fixtures updated.

## [1.0.0] — 2026-04-17

First stable release. Captures, normalizes, and analyzes tool call activity
from AI coding assistants (Claude Code, Codex, Cursor, Cline, Copilot).

### Core

- **Multi-adapter ingestion** — Claude Code (JSONL), Codex (rollout),
  Cline/Roo Code (JSON), Cursor (hook-based), Copilot (experimental)
- **SQLite storage** — WAL mode, migrations 001–006, pure-Go via
  `modernc.org/sqlite` (no CGO)
- **Freshness engine** — content hashing, stat fast-path, 5-state
  classification (fresh/stale/new/unchanged/unknown)
- **FTS5 excerpt indexing** — searchable tool output excerpts (2KB cap)
- **Secrets scrubbing** — regex-based pipeline (Bearer, AWS, API keys,
  connection strings, env vars)

### Proxy

- **API reverse proxy** — Anthropic + OpenAI, streaming + non-streaming,
  per-turn token/cost logging in `api_turns`
- **Session attribution** — `X-Session-Id` header + Linux `/proc` pid
  bridge via SessionStart hook (migration 004)
- **Conversation compression** — content-type pipeline: per-type
  compressors, importance scoring, budget enforcer, Anthropic cache
  alignment with `cache_control` breakpoint injection
- **OpenAI compression** — Chat Completions MVP (Responses API deferred)

### MCP Server

- **12 tools** — `check_file_freshness`, `get_file_history`,
  `get_session_summary`, `search_past_outputs`, `check_command_freshness`,
  `get_session_recovery_context`, `get_project_patterns`,
  `get_last_test_result`, `get_failure_context`, `get_action_details`,
  `get_cost_summary`, `get_redundancy_report`
- **Codegraph enrichment** — `check_file_freshness` and `get_file_history`
  include `structure: {functions, callers, imports}` when graph DB is available
- **Registration** — auto-configures Claude Code, Cursor, Codex MCP configs

### Intelligence

- **Pattern derivation** — hot files, co-changes, common commands,
  edit-test pairs, onboarding sequences, cross-tool files, knowledge
  snippets (from preceding reasoning)
- **Session quality scoring** — error rate, redundancy ratio, onboarding
  cost, retry cost
- **Cost estimation** — per-model pricing table, proxy + JSONL source
  merging, compression savings tracking
- **Failure correlation** — error categorization, retry detection,
  `observer learn` correction rules
- **`observer suggest`** — generates CLAUDE.md / AGENTS.md / .cursorrules
- **AI session summaries** — Claude Haiku generates 2–4 sentence summaries,
  scrubbed before storage (migration 005)
- **Dashboard** — embedded HTML + `/api/*` JSON endpoints

### Compression

- **Shell output** — TOML-driven filter engine with 6 embedded defaults
  (git, go test, docker, kubectl, cargo, pytest), `observer run <command>`
- **Conversation layer** — message importance scoring, budget enforcement,
  prefix-stable cache alignment, savings metrics in `observer cost`

### Observability

- **Prometheus `/metrics`** — 29 gauge families from `diag.Snapshot`,
  cost engine, and pid bridge; `observer metrics` serves text format 0.0.4
- **`observer doctor`** — DB integrity, hook checksums, MCP registration
  drift, pid bridge health
- **`observer status`** / `observer tail`** — live activity monitoring

### Semantic Search

- **Feature-hashed TF-IDF vectors** — 256-dim, FNV-1a hash trick, stored
  in `action_embeddings` (migration 006)
- **Cosine similarity search** — brute-force scan, gated behind
  `compression.indexing.embeddings = true`

### Codegraph

- **Auto-install** — downloads latest release from GitHub
  (DeusData/codebase-memory-mcp), verifies SHA-256 against `checksums.txt`,
  extracts platform binary
- **Graph queries** — `FunctionsInFile`, `ImportsInFile`, `CallersOf`
  against confirmed `nodes`/`edges` schema

### CLI

`observer scan|watch|init|doctor|status|tail|prune|cost|score|
discover|patterns|learn|suggest|dashboard|metrics|summarize|export`
plus `observer proxy start`, `observer start`, `observer run <cmd>`,
`observer hook`.

## [0.6.0-alpha] — 2026-04-17

Phase 6 Strand A items 1–4: pid bridge, dashboard savings, cache_control
injection, OpenAI conversation compression.

## [0.5.0-alpha] — 2026-04-17

Phase 5: full compression layer (shell + conversation).

## [0.4.0-alpha] — 2026-04-16

Phase 4: intelligence layer (patterns, scoring, cost, dashboard, suggest).

## [0.3.0-alpha] — 2026-04-16

Phase 3: API proxy, MCP server (12 tools), codegraph skeleton, Cursor +
Copilot adapters, doctor/status/tail/prune commands.

## [0.2.0-alpha] — 2026-04-16

Phase 2: freshness engine, failure context, FTS5 indexing, Codex + Cline
adapters, init command with hook registration.

## [0.1.0-alpha] — 2026-04-16

Phase 1: foundation — config, SQLite, migrations, Claude Code adapter,
storage layer, scan + watch commands.
