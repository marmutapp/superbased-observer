-- 087_token_usage_is_sidechain.sql — flag token_usage rows emitted inside
-- a sub-agent runtime (Claude Code's Agent-tool spawns).
--
-- Companion to 010_actions_is_sidechain. Claude Code's session JSONL marks
-- EVERY line emitted inside a sub-agent runtime with `isSidechain: true` —
-- including the assistant lines whose usage block produces the token_usage
-- rows. Until now only actions carried the flag (010), so the session-detail
-- sub-agents view could attribute ACTIVITY to each sub-agent window but not
-- TOKENS or COST — the honest omission recorded in commit ad46b05b. This
-- column closes that gap: per-sub-agent input/output/cache-read tokens and
-- estimated cost roll up from the SAME parent-session rows, bucketed into
-- the same spawn/start→stop windows the action summaries already use
-- (internal/intelligence/dashboard/subagents.go).
--
-- Backfill semantics: identical posture to 010 — existing rows default to 0
-- (main-thread), which simply counts zero in the sidechain bucket. No
-- dedicated surgical pass exists or is needed: the flag is a deterministic
-- function of the source line, and re-ingesting a transcript through the
-- normal adapter path (`observer scan --force`; also the dashboard Backfill
-- tab's "Run all" rescan step) re-reads it and heals existing rows via the
-- InsertTokenEvents source_event_id upsert, whose conflict clause now takes
-- excluded.is_sidechain verbatim.
--
-- NODE-LOCAL by construction, like actions.is_sidechain (010): token_usage
-- rides the org-push wire as an EXPLICIT column list
-- (internal/store/orgpush.go SelectUnpushedSince) and this column is not
-- added to it — per-sub-agent economics stay a node-side concern. No paired
-- orgserver migration. The org-import path (internal/store/import.go)
-- likewise keeps its explicit column list, so imported rows land at the 0
-- default until the owning node re-ingests its own transcripts.

ALTER TABLE token_usage ADD COLUMN is_sidechain INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_token_usage_session_sidechain
    ON token_usage(session_id, is_sidechain);
