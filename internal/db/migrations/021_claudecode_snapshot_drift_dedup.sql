-- 021_claudecode_snapshot_drift_dedup.sql — backfill the v1.6.10
-- snapshot-drift dedup landed as runtime logic in InsertTokenEvents
-- (audit B2, docs/audits/claude-code-audit-2026-05-18.md).
--
-- The v1.6.5 byte-identical tuple dedup (migration 020) collapses
-- claude-code rows that share (tool, session_id, message_id) AND
-- have byte-identical token tuples. It correctly handles Anthropic's
-- per-content-block re-emissions within ONE file.
--
-- But Claude Code's auto-compaction (`agent-acompact-*.jsonl` subagent
-- files) snapshots in-flight API turns from the parent file at a
-- LATER cumulative state than the parent's earliest matching row.
-- After both files ingest, the same msg.id has TWO rows in DB — one
-- from parent (early, low output_tokens) and one from acompact
-- (later, higher output_tokens). Tuples differ on output_tokens, so
-- migration 020's byte-identical pass doesn't catch them; the cost
-- engine then sums both, double-counting the API call.
--
-- The maintainer corpus audit (2026-05-18) found 96 msgids across
-- 2 sessions (853e0bc0, c242775f) inflated by 2,401 output tokens
-- and 122 input tokens. Small in dollars (~$0.06 at Opus rates) but
-- a real billing inflation that fails the operator's "absolute
-- accuracy" gate.
--
-- This DELETE drops, per (tool='claude-code', session_id, message_id,
-- source), every row that has a sibling with higher output_tokens
-- (ties broken by higher id — matches the runtime pass's
-- determinism). Net effect: keep only the canonical latest cumulative
-- snapshot per API call. Idempotent — re-running on a clean DB
-- is a no-op.
--
-- Scoping mirrors the runtime pass at store.go:679-705:
--   • tool = 'claude-code' only (codex emits LEGITIMATE per-turn
--     delta token_count rows that must NOT be collapsed; see
--     TestInsertTokenEvents_TupleDedupPreservesDistinctRows).
--   • source must match (jsonl vs proxy are legitimate complementary
--     captures; see TestInsertTokenEvents_DedupScopedToCopilotCLI
--     and TestInsertTokenEvents_ClaudecodeCrossSourceProxyJsonlSurvive).
--
-- Relies on idx_token_usage_session_message (migration 012) to keep
-- the EXISTS scan cheap on large tables.

DELETE FROM token_usage
WHERE tool = 'claude-code'
  AND COALESCE(message_id, '') != ''
  AND EXISTS (
    SELECT 1 FROM token_usage h
    WHERE h.tool = token_usage.tool
      AND h.session_id = token_usage.session_id
      AND h.message_id = token_usage.message_id
      AND h.source = token_usage.source
      AND h.id != token_usage.id
      AND (h.output_tokens > token_usage.output_tokens
           OR (h.output_tokens = token_usage.output_tokens AND h.id > token_usage.id))
  );
