package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestLoadCacheObservations_ImplicitRoutingOverlay drives the §15.3
// boundary at the opencode emitter: a message whose providerID is
// implicit-cache-shape (openai, deepseek, openrouter) must emit
// CacheTurnObservation.ImplicitCache=true with cleared BlockHashes;
// the existing Anthropic-routed path stays unchanged.
//
// Built against a hand-rolled in-memory schema (mirrors the
// existing opencode tests' pattern at cachetrack_test.go::seedOpenCodeDB).
func TestLoadCacheObservations_ImplicitRoutingOverlay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		providerID    string
		modelID       string
		wantImplicit  bool
		wantHasBlocks bool // Anthropic emits blocks; implicit clears them
	}{
		{"anthropic routed", "anthropic", "claude-opus-4-8", false, true},
		{"openai routed", "openai", "gpt-5", true, false},
		{"openrouter routed", "openrouter", "openai/gpt-5", true, false},
		{"deepseek routed", "deepseek", "deepseek-chat", true, false},
		{"unknown provider stays anthropic-shape", "vertex-ai", "any", false, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openInMemoryDB(t)
			defer db.Close()
			seedOpencodeMessageWithProvider(t, db, "msg-1", "sess-1", tc.providerID, tc.modelID)

			a := New()
			obs, err := a.loadCacheObservations(context.Background(), db, "/tmp/fake.db", 0)
			if err != nil {
				t.Fatalf("loadCacheObservations: %v", err)
			}
			if len(obs) != 1 {
				t.Fatalf("got %d observations, want 1", len(obs))
			}
			if obs[0].ImplicitCache != tc.wantImplicit {
				t.Errorf("obs[0].ImplicitCache = %v, want %v (provider=%q model=%q)",
					obs[0].ImplicitCache, tc.wantImplicit, tc.providerID, tc.modelID)
			}
			hasBlocks := len(obs[0].BlockHashes) > 0
			if hasBlocks != tc.wantHasBlocks {
				t.Errorf("BlockHashes len > 0 = %v, want %v (implicit MUST clear; anthropic MUST keep)",
					hasBlocks, tc.wantHasBlocks)
			}
		})
	}
}

func openInMemoryDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	// Minimal opencode schema — message + part. Matches the loader's
	// SELECT shapes (loadCacheObservations queries message; loadOrderedParts
	// queries part).
	schema := `
		CREATE TABLE message (
			id            TEXT PRIMARY KEY,
			session_id    TEXT NOT NULL,
			time_created  INTEGER NOT NULL,
			time_updated  INTEGER NOT NULL,
			data          TEXT NOT NULL
		);
		CREATE TABLE part (
			id            TEXT PRIMARY KEY,
			message_id    TEXT NOT NULL,
			time_created  INTEGER NOT NULL,
			data          TEXT NOT NULL
		);
	`
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func seedOpencodeMessageWithProvider(t *testing.T, db *sql.DB, msgID, sessID, providerID, modelID string) {
	t.Helper()
	meta := messageData{
		Role: "assistant",
	}
	meta.Model.ProviderID = providerID
	meta.Model.ModelID = modelID
	meta.ProviderID = providerID
	meta.ModelID = modelID
	meta.Tokens.Input = 100
	meta.Tokens.Output = 50
	meta.Tokens.Cache.Read = 10
	meta.Tokens.Cache.Write = 20
	meta.Time.Completed = 1000

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		msgID, sessID, 1000, 1000, string(data)); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	// Add one text part so the chain has something to hash.
	part := struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "text", Text: "hello from " + providerID}
	pd, err := json.Marshal(part)
	if err != nil {
		t.Fatalf("marshal part: %v", err)
	}
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO part (id, message_id, time_created, data) VALUES (?, ?, ?, ?)`,
		"part-1", msgID, 1000, string(pd)); err != nil {
		t.Fatalf("insert part: %v", err)
	}
}

// pin: the routing helper isn't in this package's API; use the
// public symbol via the import in cachetrack.go.
var _ = models.CacheTurnObservation{}
