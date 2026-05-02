package summary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

func seedDB(t *testing.T) (*Summarizer, *httptest.Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	base := time.Date(2026, 4, 17, 10, 0, 0, 0, time.UTC)
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{SourceFile: "f", SourceEventID: "e1", SessionID: "sA",
			ProjectRoot: root, Timestamp: base, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "a.go", Success: true},
		{SourceFile: "f", SourceEventID: "e2", SessionID: "sA",
			ProjectRoot: root, Timestamp: base.Add(time.Second), Tool: models.ToolClaudeCode,
			ActionType: models.ActionWriteFile, Target: "b.go", Success: true},
	}, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		resp := claudeResponse{Content: []struct {
			Text string `json:"text"`
		}{{Text: "The session read a.go and wrote b.go. No failures."}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	s, err := New(Options{
		DB:         database,
		APIKey:     "test-key",
		Model:      "claude-haiku-4-5-20251001",
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, srv
}

func TestSummarizeAll_GeneratesAndStores(t *testing.T) {
	s, _ := seedDB(t)
	res, err := s.SummarizeAll(context.Background())
	if err != nil {
		t.Fatalf("SummarizeAll: %v", err)
	}
	if res.SessionsProcessed != 1 {
		t.Errorf("processed: %d want 1", res.SessionsProcessed)
	}
	if res.Errors != 0 {
		t.Errorf("errors: %d want 0", res.Errors)
	}
	var md *string
	if err := s.opts.DB.QueryRow(
		`SELECT summary_md FROM sessions WHERE id = 'sA'`).Scan(&md); err != nil {
		t.Fatalf("select: %v", err)
	}
	if md == nil || *md == "" {
		t.Error("summary_md not populated")
	}
	if *md != "The session read a.go and wrote b.go. No failures." {
		t.Errorf("summary_md: %q", *md)
	}
}

func TestSummarizeAll_SkipsAlreadySummarized(t *testing.T) {
	s, _ := seedDB(t)
	// First pass
	if _, err := s.SummarizeAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second pass — should skip sA
	res, err := s.SummarizeAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionsProcessed != 0 {
		t.Errorf("processed on re-run: %d want 0", res.SessionsProcessed)
	}
}

func TestSummarizeAll_APIError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	base := time.Now().UTC()
	st.Ingest(context.Background(), []models.ToolEvent{
		{SourceFile: "f", SourceEventID: "e1", SessionID: "sB",
			ProjectRoot: root, Timestamp: base, Tool: models.ToolClaudeCode,
			ActionType: models.ActionReadFile, Target: "c.go", Success: true},
	}, nil, store.IngestOptions{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	s, _ := New(Options{
		DB: database, APIKey: "test-key",
		BaseURL: srv.URL, HTTPClient: srv.Client(),
	})
	res, err := s.SummarizeAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != 1 {
		t.Errorf("errors: %d want 1", res.Errors)
	}
}

func TestSummarizeAll_NoAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.db")
	database, _ := db.Open(context.Background(), db.Options{Path: path})
	defer database.Close()
	s, _ := New(Options{DB: database, APIKey: ""})
	// Unset the env var for this test
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := s.SummarizeAll(context.Background())
	if err == nil || err.Error() != "summary: no API key (set ANTHROPIC_API_KEY or configure intelligence.api_key_env)" {
		t.Errorf("expected no-key error, got: %v", err)
	}
}

func TestBuildDigest_Shape(t *testing.T) {
	s, _ := seedDB(t)
	digest, err := s.buildDigest(context.Background(), "sA", "claude-code", "2026-04-17T10:00:00Z", "/proj", 2)
	if err != nil {
		t.Fatal(err)
	}
	if digest == "" {
		t.Fatal("empty digest")
	}
	for _, want := range []string{"Session sA", "read_file", "write_file", "a.go", "b.go"} {
		if !contains(digest, want) {
			t.Errorf("digest missing %q:\n%s", want, digest)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsSubstring(s, sub))
}
func containsSubstring(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
