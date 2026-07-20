-- 059_copilotcli_reasoning_output_net.sql — one-shot data fix for
-- HISTORICAL Copilot CLI reasoning double-billing (Tier-1 debug-log path).
--
-- Copilot CLI's `[DEBUG] response` usage block is the OpenAI usage schema:
-- `completion_tokens` is GROSS and reasoning ⊂ completion. Arithmetic-
-- verified on the captured blocks — prompt_tokens + completion_tokens ==
-- total_tokens on every one (15474 + 565 == 16039; 16267 + 152 == 16419),
-- so `completion_tokens_details.reasoning_tokens` is NOT a separate summand:
-- it lives INSIDE completion_tokens. The copilotcli adapter's Tier-1 path
-- (internal/adapter/copilotcli/log.go::emitTokenEvent) emitted that gross
-- completion through OutputTokens alongside ReasoningTokens, so — like the
-- codex wire (migration 058) — every row written BEFORE the 2026-07-10 fix
-- that nets reasoning back out (netOutput = completion - reasoning, clamped
-- ≥ 0) carries reasoning twice: once inside output_tokens (billed at the
-- output rate) and again as reasoning_tokens (also billed at the output
-- rate). The cost engine therefore renders that Copilot CLI history at a
-- double-billed reasoning-token cost.
--
-- A rescan cannot lower the stored counts: token_usage upserts use an
-- ON CONFLICT MAX-upgrade that keeps every column monotonically
-- non-decreasing, so a re-parse of an old session with the fixed (smaller,
-- net) output can never write it back down. This one-shot UPDATE is the
-- only path that corrects the stored rows.
--
-- Idempotent-by-construction via migration ordering. Migrations run exactly
-- once, recorded in schema_meta. At the first startup of the fixed binary
-- every gross Tier-1 row is still gross, so this subtracts reasoning exactly
-- once; every ingest AFTER that writes NET output and is never seen here;
-- and a corrected row is stable against later re-parse (the fixed adapter
-- re-emits the same net output, so the MAX-upgrade sees equal values).
--
-- Scoping — tool = 'copilot-cli' AND the `output_tokens >= reasoning_tokens`
-- guard. Only the Tier-1 debug-log path (source = 'otel') ever stored a
-- gross completion alongside reasoning; the guard subtracts only where the
-- stored output could actually have contained the reasoning subset.
-- Crucially it EXCLUDES the Tier-0 `session.shutdown` modelMetrics rows
-- (source = 'session_summary'), which deliberately store output_tokens = 0
-- and carry reasoning on their own row (0 >= reasoning is false for any
-- reasoning > 0), so those rows are left untouched — their reasoning is a
-- separate, unaudited cross-tier question, NOT a gross-output subset. Other
-- reasoning-carrying wires are out of scope: opencode + kilo-code-cli are
-- arithmetic-verified DISJOINT (output can be < reasoning on the wire —
-- reasoning is already excluded from output as stored, so netting would be
-- wrong); grok + copilot (VS Code) have no per-request total anchor and are
-- UNVERIFIABLE (left alone); codex is handled by migration 058.
--
-- MAX(..., 0) clamp guards the pathological row where a captured
-- reasoning_tokens somehow exceeds output_tokens — floors at 0 rather than
-- going negative.

UPDATE token_usage
SET output_tokens = MAX(output_tokens - reasoning_tokens, 0)
WHERE tool = 'copilot-cli'
  AND reasoning_tokens > 0
  AND output_tokens >= reasoning_tokens;
