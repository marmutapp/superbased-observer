-- 007_dedup_token_usage_history.sql — heuristic backfill for the
-- analytics-audit A1 finding (forward fix landed in commit cb16006).
--
-- Until that commit, the Claude Code adapter emitted one token_usage row
-- per JSONL line that carried a `usage` block. Anthropic API calls
-- produce N JSONL records (one per content block: text + tool_use × N)
-- with the same message.id and an *echoing cumulative* usage envelope
-- (output_tokens grows from streaming placeholder to final count;
-- input_tokens, cache_read_tokens, cache_creation_tokens stay
-- byte-constant). The store dedup UNIQUE(source_file, source_event_id)
-- keyed on per-record line.UUID so each block landed as a separate row,
-- multiplying counts 2-4× per real API call.
--
-- The forward fix uses message.id as the dedup key. This migration
-- collapses pre-fix rows by *fingerprint group*: same source_file +
-- session + model + input + cache_read + cache_creation → one row,
-- keeping the maximum output_tokens (the final cumulative). The id is
-- a tiebreaker so we keep the latest row when output_tokens ties.
--
-- Heuristic edge case: two genuinely distinct API calls in the same
-- session whose prompt-side tokens (input + both cache columns) are
-- byte-identical collapse into one row. This is rare in practice — each
-- conversation turn typically extends the cache, so cache_read advances
-- monotonically — and consolidating two rows into one is far
-- preferable to leaving 2-4× duplication. The historical token_usage
-- stream is already flagged reliability='unreliable' for this kind of
-- imprecision.

DELETE FROM token_usage
WHERE tool = 'claude-code'
  AND id IN (
    SELECT id FROM (
      SELECT id,
        ROW_NUMBER() OVER (
          PARTITION BY source_file,
                       COALESCE(session_id, ''),
                       COALESCE(model, ''),
                       COALESCE(input_tokens, 0),
                       COALESCE(cache_read_tokens, 0),
                       COALESCE(cache_creation_tokens, 0)
          ORDER BY output_tokens DESC, id DESC
        ) AS rn
      FROM token_usage
      WHERE tool = 'claude-code'
    )
    WHERE rn > 1
  );
