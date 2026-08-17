# testdata/junie — JetBrains Junie fixtures

**Captured**: 2026-08-16 from a live `~/.junie/` install on the operator's
own WSL2 Ubuntu box (Junie runs as a TUI embedded in a JetBrains IDE).
**Operator**: Santosh, running the IDE's own "hello world" starter
prompt against a scratch project at `/home/marmutapp/parking-game`.
**Anonymisation**: none — these are the operator's own throwaway
hello-world test runs, not a client/production capture (HARD RULE #4 of
the adapter build task explicitly waived anonymisation for this reason).
No credential file (`secure_credentials.json`, `trust/`, or
`settings.json`'s credential fields) was ever read to produce these
fixtures.

## File inventory

| File | Purpose | Use in tests |
|------|---------|---------------|
| `index.jsonl` | Verbatim copy of the sibling `~/.junie/sessions/index.jsonl` — one line per session (`sessionId`/`createdAt`/`updatedAt`/`projectDir`/`taskName`) | `TestIndexFallback` — the project-root fallback path when a session's own `events.jsonl` never states a `CurrentDirectoryUpdatedEvent` |
| `session-260816-220304-lrfz/events.jsonl` | Verbatim copy of a real session log, 219 lines | The main fixture — every other adapter_test.go test parses this file |

## The two observed sessions

`~/.junie/sessions/` held exactly two session directories at capture
time:

- **`session-260816-220304-lrfz`** (the fixture above) — a real,
  completed hello-world task ("Python Hello World Program Creation and
  File Management" per `index.jsonl`'s `taskName`). 219 lines, 5
  top-level `kind` values, 13 distinct `agentEvent.kind` values nested
  under `SessionA2uxEvent`. This is the only session with an
  `events.jsonl` at all.
- **`session-260816-220208-d62c`** — an aborted/near-instant session:
  its directory holds only a 21-byte `transcript.md` containing the
  literal text `# Session transcript` and nothing else. No
  `events.jsonl`, no `state.json`. This is evidence (not itself
  fixtured, since there's nothing to fixture) that a session the
  operator closed before Junie ever got as far as emitting a
  `TaskStartedEvent` produces no `events.jsonl` file at all — the
  adapter's `IsSessionFile`/`ParseSessionFile` never has to special-case
  an empty-but-present log, only a missing one (which the watcher
  already skips at the file-existence layer).

## Reality-check findings baked into the fixture

Counts and structural findings below are all reproduced by
`adapter_test.go`'s `TestParseFixtureCounts` and friends; see
`internal/adapter/junie/doc.go`'s package doc for the full narrative.

- **Top-level `kind` counts**: `UserPromptEvent:1`,
  `TaskStartedEvent:1`, `SessionA2uxEvent:201`,
  `UserMessagesCommittedToHistory:15`, `TaskState:1`.
- **`SessionA2uxEvent.event.agentEvent.kind` counts** (13 distinct
  values): `AgentCurrentStatusUpdatedEvent:112`,
  `CurrentDirectoryUpdatedEvent:17`,
  `EnvironmentVariablesUpdatedEvent:17`, `LlmResponseMetadataEvent:22`,
  `TipSuggestionCreatedEvent:2`, `AgentTaskNameUpdatedEvent:1`,
  `AgentThoughtBlockUpdatedEvent:2`, `TerminalBlockUpdatedEvent:12`,
  `ContextWindowReportEvent:6`, `FileChangesBlockUpdatedEvent:6`,
  `AgentPatchCreatedEvent:1`, `ResultBlockUpdatedEvent:2`,
  `NextPromptSuggestionEvent:1`. Only 6 of the 13 have a
  normalized-action counterpart; the rest are scheduler/UI/diagnostic
  bookkeeping and are skipped silently.
- **Block collapse by `stepId`**: Terminal, FileChanges, and Result
  blocks each recur 2-4 times under the same `stepId` (an `IN_PROGRESS`
  occurrence, a terminal-status occurrence, and — once the enclosing
  task completes — a byte-identical rebroadcast). The fixture yields
  exactly 3 collapsed Terminal rows, 2 collapsed FileChanges rows, and
  1 collapsed Result row.
- **Rebroadcast-after-completion**: once `event.state` reaches
  `COMPLETED`, every block belonging to that task is re-emitted once
  more, unchanged except for timestamp jitter. Example: the Terminal
  block keyed `62c01dad-eff2-4fcc-bde3-332dde2a43c5` appears at lines
  41 (`IN_PROGRESS`), 45 (`IN_PROGRESS`), 69 (`FAILED`, its real
  terminal status), and 208 (the post-completion rebroadcast of the
  same `FAILED` occurrence).
- **`completion` is a sibling of `event`, not nested inside it**, and
  appears ONLY on a `ResultBlockUpdatedEvent` envelope:
  `{"event":{...},"completion":{"startedAtMs":...,"endedAtMs":...,"taskCostUsd":...}}`.
- **Tokens are NET, cost is provider-stated**: all 22
  `LlmResponseMetadataEvent` lines carry a non-zero `modelUsage[]`
  entry (22 `TokenEvent`s total). The first (line 8):
  `{"model":"gpt-4.1-2025-04-14","cost":0.002332,"inputTokens":1138,"cacheInputTokens":0,"cacheCreateTokens":0,"outputTokens":7,"time":0}`.
  `inputTokens` is already net of `cacheInputTokens`
  (both observed as 0 in this capture); `cost` is a genuine per-call
  dollar figure, carried straight through rather than priced by the
  cost engine.
- **Project root**: `CurrentDirectoryUpdatedEvent.currentDirectory`
  first appears around line 50 (roughly a quarter into the file), not
  on line 1 — the adapter's header pre-scan (bounded to 2000 lines,
  always re-run from byte 0 on every `ParseSessionFile` call) is what
  lets even the FIRST emitted `ToolEvent` (from line 1) carry a
  correct, non-empty `ProjectRoot`.

## Reproducing the copy

```bash
cp ~/.junie/sessions/session-260816-220304-lrfz/events.jsonl \
   testdata/junie/session-260816-220304-lrfz/events.jsonl
cp ~/.junie/sessions/index.jsonl testdata/junie/index.jsonl
```

No anonymisation script is needed or applied — see the note at the top.
