-- 052_codeintel_search_semantic.sql — Phase 6 (Tier C) of the code-
-- intelligence module: full-text search + semantic vectors + near-clone
-- LSH buckets (internal/codeintel, docs/codeintel/{search,semantic}.md).
--
-- NODE-LOCAL, same posture as 050/051: these tables hold symbol names,
-- fqns, bounded signature excerpts, packed feature-hashed vectors, and
-- MinHash band hashes — never file bodies or command outputs. Pinned in
-- tests/invariant/privacy_test.go and excluded from orgpush by
-- construction.
--
-- All three are DERIVED from codeintel_nodes and rebuilt per project by
-- the index-time sweep (store.CodeIntelBuildDerived), so they are simply
-- DELETEd-by-project and re-inserted on each pass — no incremental
-- bookkeeping, and they self-heal when nodes change.

-- Full-text search over a camelCase/snake_case-split token stream. modernc
-- SQLite has no custom-tokenizer hook (that needs CGO), so we pre-split
-- identifiers into the `tokens` column ourselves (semantic.SplitIdentifier)
-- and use the built-in unicode61 tokenizer over that — the tokenizer is
-- effectively our pre-split. Metadata columns are UNINDEXED (stored for
-- display + cheap `project` filtering, not part of the FTS index).
CREATE VIRTUAL TABLE IF NOT EXISTS codeintel_fts USING fts5(
    tokens,
    node_id    UNINDEXED,
    project    UNINDEXED,
    name       UNINDEXED,
    fqn        UNINDEXED,
    kind       UNINDEXED,
    lang       UNINDEXED,
    file       UNINDEXED,
    start_line UNINDEXED,
    end_line   UNINDEXED,
    tokenize = 'unicode61'
);

-- Feature-hashed TF vectors (the existing internal/compression/indexing
-- embedder's scheme, reused via internal/codeintel/semantic). Packed
-- little-endian []float32; dim recorded so a future neural embedder can
-- coexist.
CREATE TABLE IF NOT EXISTS codeintel_embeddings (
    node_id INTEGER PRIMARY KEY,
    project TEXT NOT NULL DEFAULT '',
    dim     INTEGER NOT NULL DEFAULT 0,
    vec     BLOB NOT NULL,
    FOREIGN KEY(node_id) REFERENCES codeintel_nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_codeintel_embeddings_project ON codeintel_embeddings(project);

-- MinHash + LSH bands for near-clone (SIMILAR_TO). One row per (node,
-- band): rows sharing a (project, band, hash) bucket are clone candidates,
-- confirmed by an estimated Jaccard over their full signatures.
CREATE TABLE IF NOT EXISTS codeintel_minhash (
    node_id INTEGER NOT NULL,
    project TEXT NOT NULL DEFAULT '',
    band    INTEGER NOT NULL,
    hash    INTEGER NOT NULL,
    FOREIGN KEY(node_id) REFERENCES codeintel_nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_codeintel_minhash_bucket ON codeintel_minhash(project, band, hash);
CREATE INDEX IF NOT EXISTS idx_codeintel_minhash_node   ON codeintel_minhash(node_id);
