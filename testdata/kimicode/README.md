# kimi-code test fixtures

Anonymized live-capture fixtures for the kimi-code adapter
(`internal/adapter/kimicode`). Captured from a real `@moonshot-ai/kimi-code`
install on 2026-07-09, then anonymized: usernames → `auzy`, project paths →
`/home/auzy/demo-project`, hostnames/emails removed. The wire-event
structure, field names, and token magnitudes are preserved faithfully.

Layout mirrors the real store
(`~/.kimi-code/sessions/wd_<slug>_<hash>/session_<uuid>/`):

```
sessions/
  wd_demo-project_ab12cd34ef56/
    session_11111111-…/
      state.json                     ← metadata (workDir = project-root seam)
      agents/main/wire.jsonl         ← primary agent wire-protocol trace
  wd_winproj_ffee00112233/
    session_22222222-…/
      state.json                     ← workDir uses FORWARD-slash C:/ Windows path
      agents/main/wire.jsonl
  wd_malformed_445566778899/
    session_33333333-…/
      agents/main/wire.jsonl         ← contains one non-JSON line (no state.json)
  wd_subagent_aabbccddeeff/
    session_44444444-…/
      state.json
      agents/main/wire.jsonl         ← spawns an Agent (sub-agent)
      agents/researcher/wire.jsonl   ← the sub-agent's own trace (IsSidechain)
```

## Fixture inventory

| Fixture | Exercises |
|---|---|
| `wd_demo-project_…/…/agents/main/wire.jsonl` | The full happy path: metadata → session-start marker; `turn.prompt` (user) + a duplicate `context.append_message` + an injected system-reminder (skipped); `llm.request` (clean `gpt-4o` model); `Write` (→ write_file, `ContentBytes`=15) + `Bash` (secret in `export API_KEY=…`, scrubbed) + a **failing** `Grep` (→ search_text, `isError`); paired `tool.result`s; `content.part` assistant text; two `step.end`/`usage.record` pairs proving the NET-input arithmetic (step2 `inputOther`=55 with `inputCacheRead`=18816). |
| `state.json` (demo) | `workDir` project-root resolution; metadata-only (no credentials). |
| `wd_winproj_…/agents/main/wire.jsonl` + `state.json` | Windows capture with **forward-slash** `C:/Users/auzy_/winproj` paths — asserts `crossmount.TranslateForeignPath` maps them to `/mnt/c/...` with no adapter-side fix. |
| `wd_malformed_…/agents/main/wire.jsonl` | A malformed (non-JSON) line mid-stream: parser warns, advances the cursor, and keeps the surrounding prompt/assistant/token events. No `state.json` (empty-project-root fallback path). |
| `wd_subagent_…/agents/main/wire.jsonl` | `Agent` tool call → `spawn_subagent`; NOT sidechain. |
| `wd_subagent_…/agents/researcher/wire.jsonl` | A sub-agent trace — every event marked `IsSidechain`. |

## Token arithmetic (gross-vs-net evidence)

`usage.record` / `step.end` carry `{inputOther, output, inputCacheRead,
inputCacheCreation}`. In the demo fixture step 2 reports `inputOther`=55
alongside `inputCacheRead`=18816. Since 55 < 18816, `inputOther` cannot
include the cached portion — it is the **NET** non-cached input. The adapter
maps it straight onto `TokenEvent.InputTokens` with no subtraction. No
reasoning/thoughts field exists in the wire format (honest gap).

## Off-limits (never fixtured, never read)

The real `~/.kimi-code/config.toml` holds a plaintext provider API key
(world-readable `0644` observed 2026-07-09); `credentials/` and `oauth/`
hold auth material. None are captured here and the adapter never reads them.
