-- 010_actions_is_sidechain.sql — flag actions emitted by sub-agent
-- runtimes (Claude Code's `Agent` tool spawn).
--
-- Claude Code's session JSONL marks every line emitted inside a
-- sub-agent runtime with `isSidechain: true`. The sub-agent's activity
-- shares the parent's session_id (NOT a separate session — a common
-- mismodelling), so the only structural marker distinguishing parent-
-- thread work from sub-agent work is this flag. Persisting it onto
-- the actions table lets the dashboard segment "you wrote this code"
-- from "your sub-agent wrote this code", and lets discover.staleReads
-- split out cross-thread (parent ↔ sub-agent) redundancy from main-
-- thread redundancy with different actionable advice for each.
--
-- Backfill semantics: existing rows default to 0 (main-thread). This
-- is a safer assumption than 1 because pre-migration data may have
-- some sub-agent rows but mostly main-thread rows; any segmentation
-- numbers from old rows simply count zero in the sidechain bucket
-- (treated as "we didn't track this back then"). New rows get the
-- real value from the JSONL adapter.

ALTER TABLE actions ADD COLUMN is_sidechain INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_actions_sidechain ON actions(is_sidechain);
