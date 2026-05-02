-- 011_actions_session_target_idx.sql — composite index that lets the
-- discover.staleReads correlated EXISTS subquery scale.
--
-- Discover's stale_count + cross_thread_count queries do, per row:
--   EXISTS (
--     SELECT 1 FROM actions a2
--     WHERE a2.session_id = a.session_id
--       AND a2.project_id = a.project_id
--       AND a2.target     = a.target
--       AND a2.action_type = 'read_file'
--       AND a2.timestamp  < a.timestamp
--       [AND a2.is_sidechain != a.is_sidechain]   -- cross-thread only
--   )
--
-- Pre-fix: only single-column indexes on session_id, target_hash,
-- action_type, timestamp existed. The correlated subquery falls back to
-- one of those (typically session_id) and then scans within the matching
-- session for the rest of the predicate. With 31k actions and 8,825
-- sidechain rows, the 30-day discover endpoint timed out > 30s.
--
-- Fix: composite index sorted to match the subquery's predicate prefix.
-- (session_id, target, action_type, timestamp, is_sidechain) lets SQLite
-- seek directly to the prior-read range for any given a-row, which
-- collapses the inner cost to O(log N) per outer row.

CREATE INDEX IF NOT EXISTS idx_actions_session_target_action_ts
    ON actions(session_id, target, action_type, timestamp, is_sidechain);

-- The single-column idx_actions_sidechain is now redundant for the
-- common discover queries. Keep it — it still helps the
-- COUNT(WHERE is_sidechain=1) query on /api/sessions row hydration.
