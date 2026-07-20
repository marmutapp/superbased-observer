-- 046_process_runs_parent_index.sql — index process_runs.parent_process_key.
--
-- The deferred cross-OS correlation pass (store.CorrelateCrossOS, §5.5) needs to
-- walk a process subtree DOWN from a candidate anchor via parent_process_key.
-- Before this, the candidate load scanned EVERY unattributed row in the table
-- (WHERE session_id IS NULL) on every Processes-drawer poll — a cost that grew
-- with the unattributed backlog and showed up as UI slowdown. The load is now a
-- recursive CTE that walks children by parent_process_key from window-bounded
-- seeds; this index makes that child-lookup an indexed seek instead of a full
-- table scan per recursion level.
--
-- NODE-LOCAL, like the rest of process_runs (pinned in
-- tests/invariant/privacy_test.go); an index adds no content/org surface.

CREATE INDEX IF NOT EXISTS idx_process_runs_parent
    ON process_runs(parent_process_key);
