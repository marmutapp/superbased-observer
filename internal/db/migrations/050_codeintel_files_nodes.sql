-- 050_codeintel_files_nodes.sql — the code-intelligence index: indexed
-- source files + the symbols defined in them (internal/codeintel,
-- docs/codeintel/schema.md). Phase 1 of the codegraph replacement —
-- exact spans only. Edges/sites land in a later migration (Phase 3),
-- FTS/embeddings/minhash later still (Phase 6).
--
-- NODE-LOCAL. These tables hold a project's source paths, symbol names,
-- and bounded signature excerpts — a structural map of private code that
-- MUST NOT leave the machine. Pinned in tests/invariant/privacy_test.go
-- (forbidden-table sentinel, pre-registered) and excluded from
-- internal/store/orgpush.go by construction (it selects an explicit
-- allow-list; these tables are never in it). Same posture as the
-- cachetrack / limit_snapshots tables.
--
-- We store ONLY paths, symbol names, and the declaration-line excerpt
-- (signature). NEVER file bodies — collapsed bodies go to the stash
-- (internal/stash). Honors CLAUDE.md "Don't store file contents... only
-- paths, commands, excerpts."

CREATE TABLE IF NOT EXISTS codeintel_files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project      TEXT NOT NULL,            -- git-root identity (internal/git)
    path         TEXT NOT NULL,            -- absolute file path
    lang         TEXT NOT NULL DEFAULT '', -- resolved codeintel.Language
    content_hash TEXT NOT NULL DEFAULT '', -- drives incremental re-index
    mtime        INTEGER NOT NULL DEFAULT 0, -- file mtime (unix seconds)
    indexed_at   INTEGER NOT NULL DEFAULT 0, -- last successful index pass (unix)
    parser       TEXT NOT NULL DEFAULT '', -- backend: goast | treesitter:* | heuristic
    status       TEXT NOT NULL DEFAULT 'pending', -- pending|indexing|indexed|stale|failed|needs_consent
    UNIQUE(project, path)
);

CREATE INDEX IF NOT EXISTS idx_codeintel_files_status
    ON codeintel_files(project, status);

CREATE TABLE IF NOT EXISTS codeintel_nodes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    project    TEXT NOT NULL,
    file_id    INTEGER NOT NULL,           -- -> codeintel_files.id
    kind       TEXT NOT NULL DEFAULT '',   -- function|method|class|interface|type|enum|...
    name       TEXT NOT NULL DEFAULT '',
    fqn        TEXT NOT NULL DEFAULT '',
    lang       TEXT NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line   INTEGER NOT NULL DEFAULT 0, -- 0 = approximate span (heuristic); non-collapsible
    start_byte INTEGER NOT NULL DEFAULT 0,
    end_byte   INTEGER NOT NULL DEFAULT 0,
    signature  TEXT NOT NULL DEFAULT '',   -- bounded declaration-line excerpt, never a body
    sig_hash   TEXT NOT NULL DEFAULT '',
    FOREIGN KEY(file_id) REFERENCES codeintel_files(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_codeintel_nodes_file
    ON codeintel_nodes(project, file_id, start_line);

CREATE INDEX IF NOT EXISTS idx_codeintel_nodes_name
    ON codeintel_nodes(project, name);
