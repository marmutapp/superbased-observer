-- 051_codeintel_edges_sites.sql — the code-intelligence relationship
-- graph: IMPORTS + name-matched CALLS edges, and the raw call/import
-- SITE behind each edge (internal/codeintel, docs/codeintel/schema.md +
-- resolution.md). Phase 3 of the codegraph replacement.
--
-- NODE-LOCAL, same posture as 050 (codeintel_files/nodes): paths, symbol
-- names, and bounded call/import excerpts — never file bodies. Pinned in
-- tests/invariant/privacy_test.go and excluded from orgpush by
-- construction.
--
-- file_id on codeintel_edges is the OWNING (source) file, so an
-- incremental re-index of one file deletes exactly that file's edges +
-- sites without touching the rest of the graph.

CREATE TABLE IF NOT EXISTS codeintel_edges (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    project          TEXT NOT NULL,
    file_id          INTEGER NOT NULL,           -- owning (src) file
    src_id           INTEGER NOT NULL DEFAULT 0, -- caller/importing node (0 = file-level, e.g. imports)
    dst_id           INTEGER NOT NULL DEFAULT 0, -- callee/target node (0 = unresolved / external)
    kind             TEXT NOT NULL DEFAULT '',   -- CALLS | IMPORTS (vocabulary grows additively)
    confidence       REAL NOT NULL DEFAULT 1.0,
    resolver_backend TEXT NOT NULL DEFAULT '',   -- goast | treesitter:* | heuristic | name-matched
    FOREIGN KEY(file_id) REFERENCES codeintel_files(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_codeintel_edges_dst  ON codeintel_edges(project, dst_id, kind);
CREATE INDEX IF NOT EXISTS idx_codeintel_edges_src  ON codeintel_edges(project, src_id, kind);
CREATE INDEX IF NOT EXISTS idx_codeintel_edges_file ON codeintel_edges(project, file_id);
CREATE INDEX IF NOT EXISTS idx_codeintel_edges_kind ON codeintel_edges(project, kind);

CREATE TABLE IF NOT EXISTS codeintel_sites (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    project          TEXT NOT NULL,
    edge_id          INTEGER NOT NULL,           -- -> codeintel_edges.id
    file_id          INTEGER NOT NULL,           -- file the site occurs in
    start_line       INTEGER NOT NULL DEFAULT 0,
    start_byte       INTEGER NOT NULL DEFAULT 0,
    raw_text         TEXT NOT NULL DEFAULT '',   -- bounded excerpt: call expr / import specifier
    target_name      TEXT NOT NULL DEFAULT '',   -- callee name / imported path as written
    resolver_backend TEXT NOT NULL DEFAULT '',
    confidence       REAL NOT NULL DEFAULT 1.0,
    FOREIGN KEY(edge_id) REFERENCES codeintel_edges(id) ON DELETE CASCADE,
    FOREIGN KEY(file_id) REFERENCES codeintel_files(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_codeintel_sites_file   ON codeintel_sites(project, file_id);
CREATE INDEX IF NOT EXISTS idx_codeintel_sites_edge   ON codeintel_sites(edge_id);
CREATE INDEX IF NOT EXISTS idx_codeintel_sites_target ON codeintel_sites(project, target_name);
