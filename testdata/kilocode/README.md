# `testdata/kilocode/` — Kilo Code adapter fixtures

Live-capture reference for the `kilo-code` + `kilo-code-cli` adapters.

## Captures (reference, NOT unit-test fixtures)

| File | Source | Purpose |
|---|---|---|
| `_capture-windows.json` | `~/.local/share/kilo/kilo.db` on Windows (captured 2026-06-06) | Reference dump of a real session driven from PowerShell. One session, 18 messages, 54 parts. Tools exercised: `read`, `write`, `bash`. Project root `D:\programsx\superbased-observer` (a git repo). |
| `_capture-wsl.json` | `~/.local/share/kilo/kilo.db` on WSL Ubuntu (captured 2026-06-06) | Reference dump of a real session driven from a WSL bash shell. Tools exercised: `bash`, `write`, `websearch`. cwd `/home/marmu` (no git repo — `path.root = "/"`). |

These dumps are **reference data**, not committed test fixtures. The unit tests
in `adapter_test.go` build a synthetic `kilo.db` in code (mirroring the
OpenCode adapter's test pattern). Use these JSON dumps to understand the
real-world schema variations the synthetic fixture must cover.

Both captures use Kilo CLI version `@kilocode/plugin@7.3.40` with the bundled
Kilo Gateway (`providerID=kilo`, modelID=`kilo-auto/free`).

## Phase-0 reality-check finds (per the research note)

1. **CLI persistence = SQLite** (`kilo.db` + WAL), NOT JSON files like upstream
   OpenCode.
2. **Data dir = `~/.local/share/kilo/`** on every OS — Kilo doesn't follow
   Windows' `%APPDATA%`/`%LOCALAPPDATA%` convention.
3. **Schema = OpenCode-shaped + Kilo extensions** — `session`/`message`/`part`/
   `todo` are OpenCode-compatible; `project`/`workspace`/`event`/`session_message`/
   `account`/`permission`/`session_share` are Kilo-specific.
4. **`message.path.{cwd,root}`** carries the working directory on every assistant
   message; `path.root` may be the FS root `/` when the cwd isn't in a git tree.
5. **`message.parentID`** links assistants back to the prompting user message —
   useful for stitching turn pairs (not present in OpenCode).
6. **Model id form = `kilo-auto/<tier>`** when routed through Kilo Gateway
   (auto-routing); direct provider use exposes `<provider>/<model>` form.
7. **Tool parts carry `metadata.openrouter.reasoning_details[]`** when routed
   through OpenRouter — verbatim chain-of-thought per tool call.

## Unit-test fixtures (Phase 15 deliverables)

Built in code by `adapter_test.go`. Each test seeds a temp `kilo.db` with the
schema captured above and asserts the adapter emits the expected `ToolEvent` +
`TokenEvent` rows. Covers:

- simple session (one prompt, one assistant reply, no tools)
- multi-tool turn (assistant with N tool parts: read/write/bash/websearch)
- malformed JSON in `part.data` (parser advances past it without crashing)
- incremental parse — call `ParseSessionFile` twice with an intermediate
  watermark; second call returns only new rows
- idempotent re-parse from `fromOffset=0` — second result identical
- scrubbing — fixture contains `API_KEY=sk-…`; output has `[REDACTED]`
- action mapping — every confirmed tool name maps to the right normalized type
- project root resolution — fixture has both git-tree and non-git cwds
- cross-mount foreign-path translation (Windows-style cwd read by a Linux
  observer resolves to `/mnt/…`)
