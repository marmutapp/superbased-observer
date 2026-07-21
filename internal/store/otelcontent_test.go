package store

import (
	"context"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestInsertOTelContent_HashesAndDedups(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	row := models.OTelContent{
		RequestID: "req_1", SessionID: "sess_1", Kind: "prompt",
		Content: "refactor the parser", Timestamp: time.Now().UTC(), Source: "cc_otel",
	}
	n, err := s.InsertOTelContent(ctx, []models.OTelContent{row})
	if err != nil || n != 1 {
		t.Fatalf("first insert: n=%d err=%v", n, err)
	}

	// Re-deliver the identical record (OTLP at-least-once): must be ignored.
	n, err = s.InsertOTelContent(ctx, []models.OTelContent{row})
	if err != nil || n != 0 {
		t.Fatalf("re-delivery should dedup: n=%d err=%v", n, err)
	}

	var count int
	var hash, content string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(content_hash),''), COALESCE(MAX(content),'') FROM otel_content WHERE request_id='req_1'`).
		Scan(&count, &hash, &content); err != nil {
		t.Fatalf("read: %v", err)
	}
	if count != 1 {
		t.Fatalf("want 1 row after re-delivery, got %d", count)
	}
	// sha256-hex of the content, computed when ContentHash was empty.
	if len(hash) != 64 {
		t.Fatalf("content_hash should be 64-char sha256-hex, got %q", hash)
	}
	if content != "refactor the parser" {
		t.Fatalf("content not stored: %q", content)
	}
}

func TestInsertOTelContent_PromptsWithEmptyKeysStayDistinctByHash(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	// Two session-level prompts (no request_id, no tool_use_id) with different
	// content must both land — dedup is by content_hash, and empty key columns
	// are stored as '' (not NULL) so the UNIQUE works.
	rows := []models.OTelContent{
		{Kind: "prompt", Content: "first"},
		{Kind: "prompt", Content: "second"},
	}
	n, err := s.InsertOTelContent(ctx, rows)
	if err != nil || n != 2 {
		t.Fatalf("two distinct prompts: n=%d err=%v", n, err)
	}
}

func TestInsertOTelContent_RequiresKind(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.InsertOTelContent(context.Background(), []models.OTelContent{{Content: "x"}}); err == nil {
		t.Fatal("expected error for missing kind")
	}
}
