# testdata/muse — Meta Muse Code fixture inventory

Fixtures for `internal/adapter/muse`. Phase-0 grounding ran 2026-08-06
against a live **`Muse Code 0.1.0 (0.1.0-R708.1)`** install on WSL2 Ubuntu
(Linux x86_64): one real interactive session, its 15 child-agent logs, the
three config files, and the shipped binary's own string table.

---

## Inventory

| File | Derived from | What it exercises |
|---|---|---|
| `simple-session.jsonl` | the real session log, anonymized + record-selected | The whole happy path: header metadata, `session.opened.observed`, a `retained_marker` tombstone, `session.workspace_branch.observed`, `run.model.configured`, a user prompt, 9 `model_completed` usage envelopes, 7 tool calls across all 4 grounded tools, their `tool_batch.effect.terminal` verdicts and `tool_result_batch_committed` bodies, the assistant message, and `session.end`. |
| `subagent-session.jsonl` | one real `subagent/<child-uuid>/session.jsonl`, anonymized in full (all 29 records) | The child-log contract: tokens that appear NOWHERE in the parent, no metadata record of its own (project root must come from the parent), `IsSidechain`, and the parent-id roll-up. |
| `malformed.jsonl` | hand-built | A truncated JSON line **and** a blank line mid-stream; the cursor must step past both and still parse the records after them. |
| `secrets-session.jsonl` | hand-built | Credential-shaped strings in the user prompt, the `bash` command arg and the tool result body, so the scrub pin covers `Target` / `RawToolInput` / `ToolOutput`. |

`simple-session.jsonl` keeps the real records' `sequence` / `recorded_at` /
`id` / usage numbers **verbatim** — that is the point of it. The token test
re-reads the file's own `model_completed.usage` envelopes and compares the
adapter's output against them, so the netting assertions are pinned to real
provider arithmetic rather than to numbers someone typed.

## Anonymization

Every string field of every retained record was rewritten before the fixture
was written:

| Real | Fixture |
|---|---|
| the operator's real workspace path | `/home/dev/demoproj` |
| the operator's real home directory | `/home/dev` |
| the real OS username (incl. `ls -l` owner columns) | `dev` |
| the real project/package names (three casings) | `demoproj` / `demopkg` / `DemoPkg` |
| the real session uuid | `11111111-2222-3333-4444-555555555555` |
| the real child-session uuid | `aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee` |
| `reasoning_committed.encrypted_content` (provider-sealed blobs, ~1.5 KB each) | `<redacted-opaque-blob>` |
| any remaining string > 900 bytes | truncated with a `…[fixture-truncated]` marker |

No credential material was copied from anywhere: `~/.config/muse/auth.json`
was never opened (only its top-level KEY NAMES were listed, to confirm what
it holds), and `tui-history.jsonl` — the cross-project raw-keystroke log —
was never read at all. The credential-shaped strings in
`secrets-session.jsonl` are synthetic, assembled from fragments so no literal
credential pattern exists in the generator either.

Verification (all four fixtures, expected to print nothing): grep the
fixtures for the real username, workspace/project names, and both original
session uuids (the values live only in the operator's environment, not in
this file), plus the standard credential shapes:

```bash
grep -ainE 'sk-[A-Za-z0-9]{16}|ghp_|AKIA[0-9A-Z]{16}|access_token|refresh_token' \
  testdata/muse/*.jsonl
grep -aoE '/home/[a-z]+' testdata/muse/*.jsonl | sort -u   # → /home/dev only
```

---

## Phase-0 findings (the seven that shaped the parser)

**1. It is an EVENT-SOURCED log, not a chat transcript.** Every line is a
record envelope `{schema_version, id, stream, sequence, recorded_at,
record_type, durability, causation_id, payload_type,
payload_schema_version, payload}`. The real discriminator is two levels
down: `payload_type` (18 distinct values in the capture), and for the
dominant `runtime.session` type, `payload.event.kind` (30 distinct kinds
across 408 of the 448 lines). Only 5 kinds carry data worth a row; the rest
is scheduler / reminder / diagnostic bookkeeping that must be skipped
SILENTLY.

**2. `recorded_at` is MICROSECONDS.** Sixteen digits
(`1785962540739784`). Read as millis it lands in 58,563 CE; read as seconds
it lands in 1970. `parseTimestamp` uses a magnitude ladder so a future unit
change degrades rather than silently corrupting.

**3. `input_tokens` is GROSS.** Turn 2 reports `input_tokens=15924` with
`cache_read_tokens=15665`, immediately after turn 1's `input_tokens=15719 /
output_tokens=101`. Read as NET, turn 2's prompt would be 31,589 tokens — an
impossible 2× jump from a 101-token reply plus a one-line user message. Read
as GROSS it is a textbook prompt-cache replay, and every later turn repeats
it (`cache_read` of turn N ≈ `input` of turn N−1). Not netting bills the
cached prefix at both the input and the cache-read rate.

**4. `output_tokens` is ALSO gross — it contains `reasoning_tokens`.** The
backend is visibly OpenAI-Responses-shaped (`resp_…` response ids,
`rs_…`/`fc_…`/`msg_…` item ids, an `encrypted_content` reasoning item), the
same convention `internal/adapter/codex` nets against. Corroborated here:
turn 1 reports `output=101, reasoning=84` for the visible reply
"Hi — how can I help?" (~7 tokens of prose). `cost.ComputeBreakdown` bills
Reasoning ADDITIVELY at the output rate, so `Output` must carry the
non-reasoning remainder.

**5. `cached_tokens` duplicates `cache_read_tokens`.** Identical in all 9
observed rows. It is a fallback, never additive — adding them would double
the cached count.

**6. Child-agent logs carry tokens that exist nowhere else.** All 15
`subagent/<uuid>/session.jsonl` files have their own `model_completed` rows
(1.7 k – 7.5 k input tokens each) and their `goal_usage_attribution` owner is
a distinct run; none of it appears in the parent's 9 `model_completed`
records. Skipping them silently undercounts the session. They have NO
`runtime.session.metadata` record, so their project root has to come from the
parent log.

**7. The verdict and the body are separate records.** One tool call produces
four: `assistant_tool_calls_committed` (name + `call_id` + `args` as a JSON
STRING), `tool_batch.effect.started`, `tool_batch.effect.terminal`
(`outcome.kind` — the only explicit success signal), and
`tool_result_batch_committed` (`results[].text`, joined on
`tool_call_id == call_id`). Only `completed` was ever observed, so the
adapter's failure polarity is inverted on purpose: an UNRECOGNISED outcome
kind leaves the call successful rather than inventing a failure.

## Two findings the fixtures could not have produced

Checklist §21 ("fixtures encode the shapes you UNDERSTAND; live captures
carry the variants that BITE") earned its keep here. Re-parsing the whole
live tree — all 16 session files — with the finished adapter surfaced two
real defects that every fixture test was green through:

1. **`submit_reminder_decision` is a real 15th native tool**, the reminder
   observer's verdict submission (`{decision, reason, confidence, priority,
   advisory_text, next_step}`). It occurs **only in child logs** — 13 calls
   across the 15 — and **never in the parent**, so reading the tool surface
   off the main session alone misses it completely. It writes back into the
   harness and touches no workspace state → `harness_call`.
2. **A child run's `started` prompt is machine-authored**, not typed: *"You
   are a reminder observer for the main agent. Do not answer the user…"*.
   Typing it as `user_prompt` turned the session's 3 real prompts into 18 —
   and would have corrupted every surface that counts user-message
   BOUNDARIES, `internal/predict`'s turns-per-message ladder most of all.
   The adapter now emits `subagent_start` for a child run's seed and keeps
   the text queryable.

The post-fix live re-parse over the real tree:

```
claimed 16 session files · 1 distinct session id · 0 warnings · 0 unknown actions
  assistant_message 2   edit_file 1   harness_call 13   read_file 1
  run_command 4         session_end 1 session_start 1   subagent_start 15
  turn_aborted 3        user_prompt 3 write_file 1
TOKENS net_in=57735 net_out=3302 cache_read=202192 reasoning=6464
```

All four token totals match an independent Python sum over the same 22
`model_completed` envelopes exactly.

## Grounded tool surface

Observed in the capture: `bash` ×4, `read_file`, `write_file`, `edit_file`
(parent log) and `submit_reminder_decision` ×13 (child logs only),
with argument keys `bash{command, workdir, description, shell, timeout_ms,
max_output_tokens, tty, login, yield_time_ms}`, `read_file{path, limit,
offset}`, `write_file{path, content}`, `edit_file{path, find, replace}`.

The run had **27 active tools** (`model_request_configured.toolset` reports
`active_tools#len: 27`) but the trace ELIDES the names
(`active_tools: ["<string>"]`). Four more were recovered from the shipped
binary's string table — `web_search`, `web_fetch`, `read_skill`, and `search`
(from the workflow-child guardrail string *"child tools may only include
read_file, search, bash, or web_search"*). The remaining ~19 are unknown; the
adapter's `actionMap` covers the conventional vocabulary defensively and
warns once per unmapped name.

## Off-limits on a live install

`~/.config/muse/auth.json`, `~/.config/muse/trust.json`,
`~/.local/share/muse/tui-history.jsonl`, `<session>/cron.db` and
`<session>/tool-outputs/` are never read by the adapter. See
`docs/muse-adapter.md` and the package doc for the rationale.
