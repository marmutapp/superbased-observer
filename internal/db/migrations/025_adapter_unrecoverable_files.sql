-- 025_adapter_unrecoverable_files.sql — v1.6.11 Issue #4 follow-up.
--
-- Persistent record of (adapter, source_file) pairs that already
-- failed every available recovery path. Lets the antigravity adapter
-- (and any future adapter with similar multi-path recovery) skip the
-- expensive decrypt + gRPC fallback dance on subsequent backfill
-- invocations when the file's underlying bytes haven't changed.
--
-- The v1.6.11 first pass used an in-memory cache on the Adapter
-- struct — that helped the `observer start` daemon's repeated
-- Rescans but did nothing for one-shot CLI `backfill --all` runs
-- (the operator's primary case). This table closes that gap: the
-- cache survives across process boundaries.
--
-- Schema notes:
--   - source_file is the absolute path on the host filesystem.
--     Composite primary key with adapter so a future adapter that
--     happens to read the same .pb file (none today) can't collide.
--   - file_size + file_mtime_unix pin the file's identity at failure
--     time. On lookup, the adapter re-stats and short-circuits ONLY
--     when both still match — any drift means the user re-encoded
--     the file (e.g. reopened the workspace in Antigravity which
--     re-derives the .pb) and we should retry.
--   - reason captures the failure cause for operator visibility on
--     the dashboard (future "Unrecoverable Files" panel) and for
--     log-search debugging.
--   - last_attempted_at is RFC3339 UTC; used to expire entries via
--     a future TTL pass (not implemented now — manual DELETE works).

CREATE TABLE IF NOT EXISTS adapter_unrecoverable_files (
    adapter           TEXT NOT NULL,
    source_file       TEXT NOT NULL,
    file_size         INTEGER NOT NULL,
    file_mtime_unix   INTEGER NOT NULL,
    reason            TEXT NOT NULL,
    last_attempted_at TEXT NOT NULL,
    PRIMARY KEY (adapter, source_file)
);

CREATE INDEX IF NOT EXISTS idx_adapter_unrecoverable_files_attempted
    ON adapter_unrecoverable_files (last_attempted_at);
