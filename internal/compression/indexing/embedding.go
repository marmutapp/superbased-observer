package indexing

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

const vectorDim = 256

// Embedder generates and stores feature-hashed TF-IDF vectors for excerpt
// semantic search. Vectors are 256-dimensional float32 arrays stored as
// packed BLOBs. Search is brute-force cosine similarity.
type Embedder struct {
	db *sql.DB
}

// NewEmbedder returns an Embedder backed by db.
func NewEmbedder(db *sql.DB) *Embedder {
	return &Embedder{db: db}
}

// Embed computes a feature-hashed vector for text and stores it in
// action_embeddings. Idempotent — re-embedding overwrites the prior vector.
func (e *Embedder) Embed(ctx context.Context, actionID int64, text string) error {
	vec := vectorize(text)
	blob := packVector(vec)
	_, err := e.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO action_embeddings (action_id, embedding) VALUES (?, ?)`,
		actionID, blob)
	if err != nil {
		return fmt.Errorf("indexing.Embed: %w", err)
	}
	return nil
}

// EmbedBatch embeds multiple (actionID, text) pairs in a single transaction.
func (e *Embedder) EmbedBatch(ctx context.Context, items []EmbedItem) error {
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("indexing.EmbedBatch: begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO action_embeddings (action_id, embedding) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("indexing.EmbedBatch: prepare: %w", err)
	}
	defer stmt.Close()
	for _, item := range items {
		vec := vectorize(item.Text)
		blob := packVector(vec)
		if _, err := stmt.ExecContext(ctx, item.ActionID, blob); err != nil {
			return fmt.Errorf("indexing.EmbedBatch: action %d: %w", item.ActionID, err)
		}
	}
	return tx.Commit()
}

// EmbedItem is one (action, text) pair for batch embedding.
type EmbedItem struct {
	ActionID int64
	Text     string
}

// SemanticResult is one result from SemanticSearch.
type SemanticResult struct {
	ActionID   int64
	Similarity float64
}

// SemanticSearch finds the top-N excerpts most similar to query by cosine
// similarity against stored embedding vectors. Results are sorted descending
// by similarity (1.0 = identical, 0.0 = orthogonal).
func (e *Embedder) SemanticSearch(ctx context.Context, query string, limit int) ([]SemanticResult, error) {
	if limit <= 0 {
		limit = 10
	}
	qvec := vectorize(query)

	rows, err := e.db.QueryContext(ctx,
		`SELECT action_id, embedding FROM action_embeddings`)
	if err != nil {
		return nil, fmt.Errorf("indexing.SemanticSearch: query: %w", err)
	}
	defer rows.Close()

	type scored struct {
		id  int64
		sim float64
	}
	var results []scored
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("indexing.SemanticSearch: scan: %w", err)
		}
		dvec := unpackVector(blob)
		if dvec == nil {
			continue
		}
		sim := cosine(qvec, dvec)
		if sim > 0 {
			results = append(results, scored{id, sim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("indexing.SemanticSearch: rows: %w", err)
	}

	// Sort by similarity descending (insertion sort — result sets are small).
	for i := 1; i < len(results); i++ {
		key := results[i]
		j := i - 1
		for j >= 0 && results[j].sim < key.sim {
			results[j+1] = results[j]
			j--
		}
		results[j+1] = key
	}
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]SemanticResult, len(results))
	for i, r := range results {
		out[i] = SemanticResult{ActionID: r.id, Similarity: r.sim}
	}
	return out, nil
}

// vectorize produces a 256-dimensional feature-hashed TF vector from text.
func vectorize(text string) []float32 {
	vec := make([]float32, vectorDim)
	words := tokenize(text)
	if len(words) == 0 {
		return vec
	}
	for _, w := range words {
		idx := hashWord(w) % uint32(vectorDim)
		vec[idx]++
	}
	// L2-normalize so cosine similarity works correctly.
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range vec {
			vec[i] = float32(float64(vec[i]) / norm)
		}
	}
	return vec
}

func tokenize(text string) []string {
	lower := strings.ToLower(text)
	return strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

func hashWord(w string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(w))
	return h.Sum32()
}

func packVector(vec []float32) []byte {
	buf := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

func unpackVector(blob []byte) []float32 {
	if len(blob) != vectorDim*4 {
		return nil
	}
	vec := make([]float32, vectorDim)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
	}
	return vec
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(na) * math.Sqrt(nb)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
