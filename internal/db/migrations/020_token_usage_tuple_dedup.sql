-- 020_token_usage_tuple_dedup.sql — backfill the tool-agnostic
-- token_usage tuple dedup introduced as runtime logic in
-- InsertTokenEvents (v1.6.5).
--
-- token_usage accumulates byte-identical re-emissions per
-- (tool, session_id, message_id) that the UNIQUE
-- (source_file, source_event_id) constraint can't catch:
--
--   • claudecode JSONL emits N content-block lines per assistant
--     message (one per text/tool_use block), and every line carries
--     the SAME cumulative usage snapshot of the parent API call.
--     Pre-cb16006 (2026-04-25) the adapter used per-line UUID as
--     source_event_id, so each line's TokenEvent landed under a
--     unique key and re-parses inserted fresh rows. ~22k historical
--     residue rows on the maintainer DB across ~18k message groups,
--     inflating per-model cost by ~30% on every dashboard surface.
--
--   • codex token_count events occasionally fire twice within
--     ~2-3s with byte-identical last_token_usage AND
--     total_token_usage (existing in-call seenModernTotal dedup
--     catches this only within one parser invocation; cross-tick
--     re-emissions slip through). ~22 residue rows.
--
--   • copilot-cli small leftover groups outside the
--     v1.6.3-T1 otel-vs-jsonl mixed-source scope (migration 019).
--
-- The DELETE collapses byte-identical tuples within each
-- (tool, session_id, message_id) group: highest id wins, all
-- lower-id rows with the same (input, output, cache_read,
-- cache_creation, cache_creation_1h, reasoning) values are dropped.
-- Rows with distinct token values (real progressions, real
-- per-emission deltas) are preserved — only true duplicates collapse.
--
-- Tool-agnostic by design: any future adapter that produces
-- byte-identical re-emissions inherits the dedup without bespoke
-- per-tool scoping.
--
-- Idempotent: re-running on a clean DB is a no-op. Relies on
-- idx_token_usage_session_message (added in 012_message_id.sql) to
-- keep the EXISTS scan cheap on large tables.

DELETE FROM token_usage
WHERE COALESCE(message_id, '') != ''
  AND EXISTS (
    SELECT 1 FROM token_usage h
    WHERE h.tool = token_usage.tool
      AND h.session_id = token_usage.session_id
      AND h.message_id = token_usage.message_id
      AND COALESCE(h.input_tokens, 0)             = COALESCE(token_usage.input_tokens, 0)
      AND COALESCE(h.output_tokens, 0)            = COALESCE(token_usage.output_tokens, 0)
      AND COALESCE(h.cache_read_tokens, 0)        = COALESCE(token_usage.cache_read_tokens, 0)
      AND COALESCE(h.cache_creation_tokens, 0)    = COALESCE(token_usage.cache_creation_tokens, 0)
      AND COALESCE(h.cache_creation_1h_tokens, 0) = COALESCE(token_usage.cache_creation_1h_tokens, 0)
      AND COALESCE(h.reasoning_tokens, 0)         = COALESCE(token_usage.reasoning_tokens, 0)
      AND h.id > token_usage.id
  );
