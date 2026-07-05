package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// InsertOTelContent persists scrubbed native-OTel content bodies (migration
// 045). Callers MUST have already scrubbed Content for secrets; this method
// computes ContentHash (sha256-hex of the stored content) when empty and
// inserts idempotently — re-delivered OTLP exports collide on the UNIQUE key
// and are ignored. Returns the number of rows newly inserted.
func (s *Store) InsertOTelContent(ctx context.Context, rows []models.OTelContent) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store.InsertOTelContent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var inserted int
	for _, r := range rows {
		if r.Kind == "" {
			return inserted, fmt.Errorf("store.InsertOTelContent: kind is required")
		}
		hash := r.ContentHash
		if hash == "" {
			sum := sha256.Sum256([]byte(r.Content))
			hash = hex.EncodeToString(sum[:])
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO otel_content
			    (request_id, session_id, tool_use_id, kind, content, content_hash, timestamp, source)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(content_hash, kind, request_id, tool_use_id) DO NOTHING`,
			// request_id + tool_use_id are part of the UNIQUE key, so they must
			// be stored as '' (not NULL) — SQLite treats NULLs as distinct and
			// idempotency would break for prompt rows (no tool_use_id).
			r.RequestID, nullableString(r.SessionID),
			r.ToolUseID, r.Kind,
			nullableString(r.Content), hash,
			timestamp(r.Timestamp), nullableString(r.Source))
		if err != nil {
			return inserted, fmt.Errorf("store.InsertOTelContent: insert: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	if err := tx.Commit(); err != nil {
		return inserted, fmt.Errorf("store.InsertOTelContent: commit: %w", err)
	}
	return inserted, nil
}
