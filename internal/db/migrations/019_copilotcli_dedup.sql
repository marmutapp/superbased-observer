-- 019_copilotcli_dedup.sql — backfill the copilot-cli Tier 1/3 dedup
-- introduced as runtime logic in InsertTokenEvents (v1.6.3).
--
-- copilot-cli emits two TokenEvents per Request-ID when --log-level
-- debug is enabled:
--   • Tier 1 (source='otel'): parsed from ~/.copilot/logs/process-*.log
--     debug `[DEBUG] response (Request-ID …):` blocks; carries the full
--     usage breakdown (input, output, cached, reasoning).
--   • Tier 3 (source='jsonl'): parsed from
--     ~/.copilot/session-state/<sid>/events.jsonl assistant.message
--     events; carries only outputTokens.
--
-- They land in token_usage under DIFFERENT (source_file, source_event_id)
-- keys, so the ON CONFLICT clause in InsertTokenEvents can't dedup them.
-- output_tokens were double-counted in cost rollups for every Request-ID
-- where both tiers fired. Runtime fix shipped in v1.6.3; this migration
-- cleans up rows captured before the runtime fix landed.
--
-- The DELETE is conservative: only Tier-3 (jsonl) rows are removed, and
-- only when a Tier-1 (otel) row exists for the same
-- (session_id, message_id). Tier-3-only sessions (debug logging never
-- enabled) keep their rows so OutputTokens isn't lost.
--
-- Idempotent: re-running this migration on a clean DB is a no-op.

DELETE FROM token_usage
WHERE tool = 'copilot-cli'
  AND source = 'jsonl'
  AND COALESCE(message_id, '') != ''
  AND EXISTS (
    SELECT 1 FROM token_usage h
    WHERE h.tool = 'copilot-cli'
      AND h.source = 'otel'
      AND h.session_id = token_usage.session_id
      AND h.message_id = token_usage.message_id
      AND h.id != token_usage.id
  );
