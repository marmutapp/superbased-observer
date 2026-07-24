package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// TestAPISessionMessages_Tail pins the additive ?tail=N behaviour: an absent
// param is byte-identical to the current output, a present param returns only
// the LAST N message rows while keeping total the full count, and the clamp
// (1..200; <1 / non-numeric ignored, >200 saturated) holds.
func TestAPISessionMessages_Tail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	base := time.Date(2026, 4, 28, 6, 50, 0, 0, time.UTC)

	// Five distinct user-prompt messages → five chronological message rows.
	const n = 5
	evts := make([]models.ToolEvent, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		mid := "user_" + string(rune('A'+i))
		ids = append(ids, mid)
		evts = append(evts, models.ToolEvent{
			SourceFile: "f", SourceEventID: mid, SessionID: "sTail",
			ProjectRoot: root, Timestamp: base.Add(time.Duration(i) * time.Second),
			Tool: models.ToolClaudeCode, ActionType: models.ActionUserPrompt,
			Target: "prompt " + mid, Success: true, MessageID: mid,
		})
	}
	if _, err := st.Ingest(context.Background(), evts, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}

	type resp struct {
		Messages []struct {
			MessageID string `json:"message_id"`
		} `json:"messages"`
		Total  int `json:"total"`
		Offset int `json:"offset"`
	}
	call := func(query string) resp {
		t.Helper()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/session/sTail/messages"+query, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("query %q: status %d body=%s", query, rr.Code, rr.Body.String())
		}
		var out resp
		if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
			t.Fatalf("query %q: decode: %v", query, err)
		}
		return out
	}
	msgIDs := func(r resp) []string {
		got := make([]string, 0, len(r.Messages))
		for _, m := range r.Messages {
			got = append(got, m.MessageID)
		}
		return got
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Baseline: no tail → all five, chronological.
	full := call("")
	if len(full.Messages) != n || full.Total != n {
		t.Fatalf("baseline: got %d messages / total %d, want %d/%d", len(full.Messages), full.Total, n, n)
	}
	if !eq(msgIDs(full), ids) {
		t.Fatalf("baseline order = %v, want %v", msgIDs(full), ids)
	}

	tests := []struct {
		name       string
		query      string
		wantIDs    []string
		wantOffset int
	}{
		{"tail 2 returns last 2", "?tail=2", ids[n-2:], n - 2},
		{"tail 1 returns last 1", "?tail=1", ids[n-1:], n - 1},
		{"tail >= count returns all", "?tail=5", ids, 0},
		{"tail over count returns all", "?tail=99", ids, 0},
		{"tail 0 ignored as absent", "?tail=0", ids, 0},
		{"tail negative ignored as absent", "?tail=-3", ids, 0},
		{"tail non-numeric ignored as absent", "?tail=abc", ids, 0},
		{"tail over cap saturates at 200", "?tail=100000", ids, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := call(tc.query)
			if got := msgIDs(r); !eq(got, tc.wantIDs) {
				t.Fatalf("messages = %v, want %v", got, tc.wantIDs)
			}
			// total is ALWAYS the full count, regardless of tail.
			if r.Total != n {
				t.Fatalf("total = %d, want %d (tail must not change the reported count)", r.Total, n)
			}
			if r.Offset != tc.wantOffset {
				t.Fatalf("offset = %d, want %d", r.Offset, tc.wantOffset)
			}
		})
	}

	// FROZEN contract: tail combined with any pagination param is a hard 400 —
	// the two windowing schemes are mutually exclusive.
	for _, q := range []string{
		"?locate=" + ids[0] + "&tail=2",
		"?tail=2&limit=3",
		"?tail=2&offset=1",
		"?tail=abc&limit=3", // present-but-garbage tail still rejects the combo
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/session/sTail/messages"+q, nil)
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status %d, want 400 (tail+pagination must be rejected)", q, rr.Code)
		}
	}
}

// TestAPISessionMessages_TailFullTimeline pins the load-bearing fix: with more
// than one default page of messages (>100), ?tail=N must return the last N of
// the FULL ordered timeline — not the tail of page 0 (the pre-fix bug returned
// rows 95..100 for ?tail=6 against 150 messages). It also pins the offset
// arithmetic (total−N) and that total stays the full count.
func TestAPISessionMessages_TailFullTimeline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	database, err := db.Open(context.Background(), db.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	st := store.New(database)
	root := t.TempDir()
	base := time.Date(2026, 4, 28, 6, 50, 0, 0, time.UTC)

	// 150 messages — well past the default limit=100 first page.
	const n = 150
	evts := make([]models.ToolEvent, 0, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		mid := fmt.Sprintf("user_%03d", i)
		ids = append(ids, mid)
		evts = append(evts, models.ToolEvent{
			SourceFile: "f", SourceEventID: mid, SessionID: "sBig",
			ProjectRoot: root, Timestamp: base.Add(time.Duration(i) * time.Second),
			Tool: models.ToolClaudeCode, ActionType: models.ActionUserPrompt,
			Target: "prompt " + mid, Success: true, MessageID: mid,
		})
	}
	if _, err := st.Ingest(context.Background(), evts, nil, store.IngestOptions{}); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Options{DB: database, DBPath: path})
	if err != nil {
		t.Fatal(err)
	}

	var out struct {
		Messages []struct {
			MessageID string `json:"message_id"`
		} `json:"messages"`
		Total  int `json:"total"`
		Offset int `json:"offset"`
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session/sBig/messages?tail=6", nil)
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rr.Code, rr.Body.String())
	}
	if err := json.NewDecoder(rr.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != n {
		t.Fatalf("total = %d, want %d", out.Total, n)
	}
	if out.Offset != n-6 {
		t.Fatalf("offset = %d, want %d (total−N)", out.Offset, n-6)
	}
	if len(out.Messages) != 6 {
		t.Fatalf("got %d messages, want 6", len(out.Messages))
	}
	want := ids[n-6:] // the TRUE last 6, not rows 95..100 of page 0
	for i, m := range out.Messages {
		if m.MessageID != want[i] {
			t.Fatalf("row %d = %s, want %s (tail must be last-N of the FULL timeline)", i, m.MessageID, want[i])
		}
	}
}
