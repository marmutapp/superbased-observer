# Command Code fixtures

Phase-0 research fixtures for a possible `command-code` adapter
(`command-code` npm package, `commandcode.ai`). See
`docs/plans/commandcode-adapter-plan-2026-07-29.md` for the full
reality check and implementation plan.

All fixtures below are **hand-anonymized reconstructions** of real
capture from a live WSL install (`command-code@1.4.5`) and a live
Windows-side mirror install, with real paths replaced by
`/home/user/project` (Linux) / `C:\Users\user\example-project`
(Windows), real session/message UUIDs replaced with zero-padded or
letter-padded placeholders, and all prose/tool-output text replaced
with short benign placeholder text. Structure (keys, nesting, field
types, numeric magnitudes for token counts) is preserved verbatim from
the live capture — only *content* was synthesized. No fixture here was
produced by a raw `cp`/`cat` of a live file.

## Reproduction

Any of these fixtures can be regenerated against a live
`command-code` install:

```
commandcode -p "hi"                          # interactive/-p session under ~/.commandcode/projects/<dash-encoded-cwd>/<uuid>.jsonl
commandcode --output-format json -p "hi"      # NDJSON headless mode (not captured here — see open questions)
```

## Inventory

| File | Source shape | Notes |
|---|---|---|
| `session-sample.jsonl` | `~/.commandcode/projects/<dash-encoded-cwd>/<uuid>.jsonl` (Linux/WSL) | 1 `session` header line + 6 `message` lines (3 user, 3 assistant). Demonstrates the `parentId` linked-list chain, inline `cwd`, per-assistant-message `usage`+`model`, and a `tool_use`/`tool_result` pair (`read_file`). |
| `session-sample.meta.json` | `~/.commandcode/projects/<dash-encoded-cwd>/<uuid>.meta.json` | Per-session metadata sidecar: `traceIds` (OTel trace correlation), `model`, `title` (auto-generated session title). |
| `session-sample.checkpoints.jsonl` | `~/.commandcode/projects/<dash-encoded-cwd>/<uuid>.checkpoints.jsonl` | One line per user turn (`/rewind` restore points). Carries the **raw prompt text** again (`prompt` field) — redundant with the main transcript; NOT a distinct capture source, just corroborating evidence of the per-turn checkpoint model. |
| `project-config-sample.json` | `~/.commandcode/projects/<dash-encoded-cwd>/config.json` | Per-project settings. Live sample carried a `tasteOnboarding.skippedSessions` map keyed by OTHER tool names (`claude-code`, `codex`) with THEIR session UUIDs — evidence of the `/import` / `/learn-taste` cross-tool taste-learning feature reading sibling tools' session stores. Not itself session/token data; no capture action needed, documented for awareness only. |
| `history-sample.jsonl` | `~/.commandcode/history.jsonl` (top-level, NOT per-project) | One line per submitted prompt across ALL projects: `{"p": <prompt text>, "t": <unix-ms>}`. No project/session correlation field. Off-limits for capture — mirrors the `cline-cli` `user_input_history.jsonl` precedent (cross-session raw-prompt aggregate, redundant with per-session transcripts, sensitive). |
| `toplevel-config-sample.json` | `~/.commandcode/config.json` | Global config: `installed`, `firstMessageSent`, `provider` (constant `"command-code"`), `model` (last-used model, used as the CLI's own default-model memory). |
| `windows-session-sample.jsonl` | `/mnt/c/Users/<user>/.commandcode/projects/<dash-encoded-cwd>/<uuid>.jsonl` (Windows mirror, read over DrvFs) | Same schema as the Linux fixture. Demonstrates the Windows drive-letter dash-encoding (`C:\Users\user\example-project` → `c-users-user-example-project`) and two more tool names: `read_directory` (vs `read_file`). |
| `windows-session-sample.checkpoints.jsonl` / `.meta.json` | same Windows session | Paired sidecars, same shape as the Linux ones. |

## NOT captured / off-limits (documented, not copied)

- `auth.json` (top-level, mode 0600) — OAuth/session credential for the
  operator's Command Code account. Existence confirmed
  (`~/.commandcode/auth.json`, 274 bytes); contents never read or
  quoted, per hard rule.
- `ide/` (top-level, mode 0700) — IDE-connection socket
  (`code-<hash>.sock`) + a small JSON descriptor. Live connection
  plumbing, not session data.
- `skills/` (present on the Windows install only in this environment;
  absent on the WSL side) — symlinks to skill packages
  (`find-skills -> ...\.agents\skills\find-skills\`). User-installed
  extensions, not session/token data.
- `telemetry-install-id`, `updates.json` — anonymous install
  telemetry / auto-update bookkeeping. Irrelevant to capture.

## Live-install facts NOT reproducible in a committed fixture

- **Encoding rule, empirically confirmed by three live probes** (not
  guessed from these fixtures alone):
  - `/home/marmutapp/needlehaystack` → `home-marmutapp-needlehaystack`
    (existing real session, pre-dated this research).
  - `/tmp/cc-probe-scratch` → `tmp-cc-probe-scratch` (fresh probe:
    confirms **leading `/` is simply dropped — NO leading dash**,
    unlike Claude Code's `-tmp-cc-probe-scratch`).
  - `/tmp/cc_probe_two/sub_dir` → `tmp-cc-probe-two-sub-dir` (fresh
    probe: confirms **underscores are ALSO folded into `-`**, then
    the resulting double-dash from `_` immediately followed by the
    path-separator `-` is collapsed to one).
  - Windows: `C:\Users\auzy_\copilot-smoke` →
    `c-users-auzy-copilot-smoke` (live install: confirms **drive
    letter lowercased, colon dropped, backslashes → `-`, and the
    trailing underscore in the username folder is folded the same
    way** — `auzy_` + `-` collapses to `auzy-`, not `auzy_-`).
- **Token gross/net arithmetic** — see the plan doc §"Token capture"
  for the full argument; summarized here because it's the load-bearing
  fact for Phase 8/9: across 3 independent live sessions, a turn's
  `cacheReadTokens` is consistently *comparable to or nearly equal to*
  `inputTokens` while `inputTokens` itself grows only slightly
  turn-over-turn — the signature of `cacheReadTokens ⊆ inputTokens`
  (GROSS), not `total == input + cacheRead` (NET). Combined with the
  `chatcmpl-tool-*` tool-call ID prefix (an OpenAI-Chat-Completions-
  shaped backend under `@ai-sdk/openai-compatible`), this is
  high-confidence but NOT 100% proxy-confirmed (no network capture was
  taken — see plan §Open questions).
