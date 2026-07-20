-- Migration 006: Embedding vectors for semantic search across excerpts.
-- See spec §25 (Strand B — semantic search across excerpts).
-- Each row stores a feature-hashed TF-IDF vector as a packed float32 BLOB.
CREATE TABLE IF NOT EXISTS action_embeddings (
    action_id INTEGER PRIMARY KEY,
    embedding BLOB NOT NULL
);

INSERT OR REPLACE INTO schema_meta (key, value) VALUES ('version', '6');
