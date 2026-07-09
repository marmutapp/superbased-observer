package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/compression/indexing"
)

// TestHandleSearch pins the P6.2 surface: FTS5 hits enriched with the
// owning action's session/timestamp, sentinel-marked snippets, rank
// ordering, the bad-query 400, and the method guard.
func TestHandleSearch(t *testing.T) {
	server, _ := wizardTestServer(t)
	now := time.Now().UTC()
	seedLiveSession(t, server, "search-s", "claude-code", now.Add(-time.Hour), now.Add(-30*time.Minute), 2)

	var actionID int64
	if err := server.opts.DB.QueryRow(
		`SELECT id FROM actions WHERE session_id = 'search-s' ORDER BY id LIMIT 1`,
	).Scan(&actionID); err != nil {
		t.Fatal(err)
	}
	idx := indexing.New(server.opts.DB, 0)
	if err := idx.Index(t.Context(), actionID, "Bash", "npm test",
		"FAIL src/auth.test.ts: deadline exceeded after 30s", "exit 1"); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search?q=deadline", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Query string `json:"query"`
		Count int    `json:"count"`
		Hits  []struct {
			ActionID  int64  `json:"action_id"`
			SessionID string `json:"session_id"`
			Timestamp string `json:"timestamp"`
			Tool      string `json:"tool"`
			ToolName  string `json:"tool_name"`
			Target    string `json:"target"`
			Snippet   string `json:"snippet"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 1 || len(got.Hits) != 1 {
		t.Fatalf("hits: %+v", got)
	}
	h := got.Hits[0]
	if h.ActionID != actionID || h.SessionID != "search-s" || h.Tool != "claude-code" {
		t.Errorf("enrichment: %+v", h)
	}
	if h.Timestamp == "" {
		t.Error("timestamp not enriched")
	}
	if h.ToolName != "Bash" || h.Target != "npm test" {
		t.Errorf("identity: %+v", h)
	}
	if want := "\x01deadline\x02"; !strings.Contains(h.Snippet, want) {
		t.Errorf("snippet missing sentinel-marked match: %q", h.Snippet)
	}

	// Empty query → 400.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/search", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty q: got %d want 400", rr.Code)
	}

	// FTS5 syntax error → 400, not 500 (user input, not server fault).
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, `/api/search?q=%22unclosed`, nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad fts query: got %d want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	// Method guard.
	rr = httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/search?q=x", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: got %d want 405", rr.Code)
	}
}
