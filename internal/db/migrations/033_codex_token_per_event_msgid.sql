-- 033_codex_token_per_event_msgid.sql — v1.7.24 codex backfill.
--
-- Pre-v1.7.24 the codex adapter set token_usage.message_id = TurnID,
-- so all per-inference token_count events of one user-turn collapsed
-- onto the same message_id. Migration 032 added the turn_id column
-- for grouping, but the dashboard's per-inference detail view
-- (?detail=inference, msg_key = COALESCE(message_id, source_event_id))
-- still sees one msg_key per turn because message_id remained the
-- old TurnID value on legacy rows.
--
-- The v1.7.24 adapter sets message_id = source_event_id at emit
-- time, but store.InsertTokenEvents's ON CONFLICT clause preserves
-- the existing message_id on re-emits (intentional for claudecode
-- and other Anthropic adapters where the upstream msg_xxx is
-- authoritative and shouldn't be clobbered by a re-parse). So
-- legacy codex rows never auto-upgrade — they get their turn_id
-- backfilled to the same TurnID value, leaving message_id ==
-- turn_id and the per-inference grouping degenerate.
--
-- This migration upgrades codex token rows in place. For each
-- legacy row:
--   - If turn_id is still NULL/empty, move the old message_id
--     value (which IS the TurnID) into turn_id.
--   - Replace message_id with source_event_id, the per-event ID
--     that matches the v1.7.24 contract.
--
-- The WHERE clause is idempotent: post-migration message_id ==
-- source_event_id so a re-run finds zero matching rows. Only codex
-- token rows whose source_event_id matches the per-event format
-- (`tk:<file>:L<n>`) are touched — other adapters' rows are left
-- alone.

UPDATE token_usage
   SET turn_id    = COALESCE(NULLIF(turn_id, ''), message_id),
       message_id = source_event_id
 WHERE tool = 'codex'
   AND source_event_id LIKE 'tk:%'
   AND message_id != source_event_id;
