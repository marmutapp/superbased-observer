package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/proxy"
)

// TestStreamingTurn_RequestIDEnablesDedupGate is the C7
// operator-requested end-to-end protector for the streaming
// path. It guards the load-bearing join:
//
//   - The proxy's [proxy.Proxy.buildStreamTurn] must capture
//     raw.ID from the upstream `message_start` SSE event into
//     APITurn.RequestID (= the msg_xxx upstream id).
//   - The C5 dedup gate
//     [Store.CacheEventExistsForMessage] joins cache_events on
//     api_turns.request_id to detect when a proxy-tier event
//     already exists for a given msg_xxx.
//
// If either side drifts (a future proxy change writes msg_xxx
// to a different column, the SSE shape changes, the schema
// renames request_id, …), this test fails LOUDLY rather than
// silently degrading to double-write events from Tier-1 +
// Tier-2 for the same logical turn. Claude Code streams, so
// the failure mode would be silent in production.
//
// The path under test:
//
//  1. SSE message_start carrying msg_streaming_xyz arrives.
//  2. parseAnthropicStream + buildStreamTurn together produce
//     APITurn.RequestID = "msg_streaming_xyz".
//  3. insertTurnDetached writes the row via the Sink (which IS
//     the *Store in production); we exercise the same wire by
//     passing *Store as Sink here.
//  4. A subsequent tier='proxy' cache_event is inserted linked
//     to the new api_turn id.
//  5. CacheEventExistsForMessage("sA", "msg_streaming_xyz")
//     returns true → Tier-2 emitters know to skip.
func TestStreamingTurn_RequestIDEnablesDedupGate(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	// A minimal real-shape Anthropic SSE: message_start sets the
	// msg id; message_delta carries final stop_reason; the
	// proxy's parseAnthropicStream extracts both.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_streaming_xyz","model":"claude-opus-4-7","usage":{"input_tokens":100,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	anth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		if f, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(sse))
			f.Flush()
		}
	})
	anthUp := httptest.NewServer(anth)
	t.Cleanup(anthUp.Close)
	oaiUp := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(oaiUp.Close)

	// *Store satisfies proxy.Sink (InsertAPITurn) — the
	// production wiring. Using the real store here means the
	// api_turn lands in the same DB the gate queries.
	p, err := proxy.New(proxy.Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		Sink:              s,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-opus-4-7","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "sA")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// insertTurnDetached writes asynchronously; poll briefly.
	var apiTurnID int64
	var capturedRequestID string
	for i := 0; i < 100; i++ {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(request_id), ''), COALESCE(MAX(id), 0) FROM api_turns`).
			Scan(&n, &capturedRequestID, &apiTurnID); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if capturedRequestID != "msg_streaming_xyz" {
		t.Fatalf("api_turns.request_id = %q, want %q — buildStreamTurn did NOT capture raw.ID from message_start. The dedup gate would silently fail into double-writes (Tier-1 + Tier-2 both emit events for the same logical turn).",
			capturedRequestID, "msg_streaming_xyz")
	}

	// Insert a tier='proxy' cache_event linked to this turn —
	// simulating what C8's proxy wiring would do.
	if _, err := s.InsertCacheEvents(ctx, []CacheEventRow{{
		SessionID: "sA", APITurnID: &apiTurnID, Tier: "proxy",
		Timestamp: time.Now().UTC(), Model: "claude-opus-4-7",
		Kind: "hit", Cause: "suffix_growth",
		TokensRead: 100,
	}}); err != nil {
		t.Fatalf("InsertCacheEvents: %v", err)
	}

	// The §9 dedup gate: a Tier-2 emitter calling this with
	// the same (sessionID, msg_xxx) MUST see true so the
	// observation is skipped.
	fires, err := s.CacheEventExistsForMessage(ctx, "sA", "msg_streaming_xyz")
	if err != nil {
		t.Fatal(err)
	}
	if !fires {
		t.Fatal("dedup gate did NOT fire on streaming-captured msg_xxx; Tier-2 emitter would double-write events for this session — the gate's join key drifted out of sync with proxy.buildStreamTurn's capture")
	}

	// Sanity: a different msg_xxx must NOT trigger the gate
	// (the gate is precise per-message, not session-wide).
	if other, err := s.CacheEventExistsForMessage(ctx, "sA", "msg_unrelated"); err != nil || other {
		t.Errorf("gate over-fired: msg_unrelated returned (%v, err=%v); want (false, nil)", other, err)
	}
}
