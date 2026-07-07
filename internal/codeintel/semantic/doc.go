// Package semantic provides the Embedder interface (TF-IDF default,
// neural deferred — ADR-0003/0004), cosine similarity for
// SEMANTICALLY_RELATED, and MinHash+LSH near-clone detection for
// SIMILAR_TO. It is PURE — no database/sql, net/http, or fsnotify;
// vectors and shingles are computed in-memory and persisted by the
// store seam. See docs/codeintel/semantic.md.
package semantic
