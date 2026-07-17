-- 069_codex_session_lineage.sql — adds codex fork/subagent lineage
-- columns to sessions.
--
-- Codex 0.144+ physically replays a parent rollout's event stream into
-- a fork or subagent's new rollout JSONL. The owning session_meta of
-- such a child stamps lineage markers the adapter now captures:
--   forked_from_id   — parent session a user-fork/subagent forked from
--   parent_thread_id — spawning parent thread for a subagent
--   thread_source    — "user" (normal + user-fork) or "subagent"
--
-- All three are NODE-LOCAL: they are NEVER selected by the org-push
-- seam (internal/store/orgpush.go::SelectUnpushedSince) and are pinned
-- out of the wire by tests/invariant/privacy_test.go. They are written
-- ONLY through Store.SetSessionLineage (not UpsertSession), so a
-- re-parse never clobbers a captured value (COALESCE-preserving).
--
-- Nullable, no backfill: existing rows keep NULL until the owning
-- rollout is re-scanned. Normal (non-fork) codex sessions carry
-- thread_source = "user" with the two id columns NULL.

ALTER TABLE sessions ADD COLUMN forked_from_id TEXT;
ALTER TABLE sessions ADD COLUMN parent_thread_id TEXT;
ALTER TABLE sessions ADD COLUMN thread_source TEXT;

CREATE INDEX IF NOT EXISTS idx_sessions_forked_from ON sessions(forked_from_id);
