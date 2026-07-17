-- 058_codex_reasoning_output_net.sql — one-shot data fix for HISTORICAL
-- codex reasoning double-billing.
--
-- The OpenAI/Codex wire reports GROSS output_tokens: reasoning ⊂ output
-- (input + output == total on every event, verified on live gpt-5.6
-- traffic). The codex adapter emitted that gross output through the
-- 2026-07-10 fix that nets reasoning back out at both emit sites
-- (netOutput = output_tokens - reasoning_tokens, clamped ≥ 0). So every
-- codex token_usage row written BEFORE that fixed binary first started
-- carries reasoning twice: once inside output_tokens (billed at the
-- output rate) and once again as reasoning_tokens (also billed at the
-- output rate — see the reasoning-billed-at-output-rate cost rule). The
-- cost engine therefore renders codex history at a double-billed
-- reasoning-token cost.
--
-- A rescan cannot lower the stored counts: token_usage upserts use an
-- ON CONFLICT MAX-upgrade that keeps every column monotonically
-- non-decreasing, so a re-parse of an old session with the fixed
-- (smaller, net) output can never write it back down. This one-shot
-- UPDATE is the only path that corrects the stored rows.
--
-- Idempotent-by-construction via migration ordering. Migrations run
-- exactly once, recorded in schema_meta. At the first startup of the
-- fixed binary EVERY existing codex row is still gross, so this runs
-- against a fully-gross corpus and subtracts reasoning exactly once.
-- Every codex ingest AFTER that startup writes NET output, so those
-- rows are never seen by this migration. And a corrected row is stable
-- against later re-parse: the fixed adapter re-emits the same net
-- output, so the MAX-upgrade sees equal values and leaves it be. There
-- is no ordering in which reasoning is subtracted twice.
--
-- Scoping — tool = 'codex' ONLY. This gross-output shape is a
-- codex/OpenAI-wire property, arithmetic-verified there. The other
-- reasoning-carrying wires are NOT the same shape and must be left
-- alone: gemini/antigravity are arithmetic-verified DISJOINT (reasoning
-- is already excluded from output as stored — correct); qwencode is
-- unverified (all live thoughts counts are zero, nothing to correct);
-- the remaining reasoning-carrying tools are unaudited. Touching any of
-- them here would corrupt correct data.
--
-- MAX(..., 0) clamp guards the pathological row where a captured
-- reasoning_tokens somehow exceeds output_tokens — the result floors at
-- 0 rather than going negative.

UPDATE token_usage
SET output_tokens = MAX(output_tokens - reasoning_tokens, 0)
WHERE tool = 'codex'
  AND reasoning_tokens > 0;
