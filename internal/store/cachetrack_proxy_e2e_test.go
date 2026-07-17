package store

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/cachetrack"
	"github.com/marmutapp/superbased-observer/internal/proxy"
)

// TestProxyAnthropic_WritesCacheRows is the C8 end-to-end
// acceptance test. Wires:
//
//   - proxy.New with the real *Store as Sink AND CacheSink,
//     plus a fresh cachetrack.Engine.
//   - A real-shape Anthropic streaming request whose body
//     carries the R1(a) marker pattern: 2 system breakpoints
//     (system[1], system[2]) + 1 rolling marker on the last
//     message block, all 1h tier.
//   - An SSE upstream that returns message_start (with msg_xxx
//     and cache_creation_input_tokens) + message_delta.
//
// Asserts the full wire:
//   - api_turns row is written with request_id = msg_xxx.
//   - cache_segments rows are written linked to that api_turn,
//     with at least 3 IsBreakpoint=1 rows (the R1(a) shape).
//   - cache_events row(s) are written with kind=reanchor on
//     the first turn.
//   - The dedup gate fires for the message-id (the C7 carry-
//     forward operator concern: Tier-2 emitting for the same
//     msg_xxx must skip).
func TestProxyAnthropic_WritesCacheRows(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	// SSE upstream returning a real-shape message_start carrying
	// msg_e2e_test + cache_creation_input_tokens.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_e2e_test","model":"claude-opus-4-8","usage":{"input_tokens":200,"cache_read_input_tokens":0,"cache_creation_input_tokens":50000,"output_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":50000}}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":100}}`,
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

	engine := cachetrack.NewEngine(64)
	p, err := proxy.New(proxy.Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		Sink:              s,
		CacheEngine:       engine,
		CacheSink:         s, // *Store implements PersistCacheObservation
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)

	// R1(a)-shape request body: 2 system markers + 1 rolling
	// message marker, all 1h tier.
	body := `{
		"model":"claude-opus-4-8",
		"stream":true,
		"tools":[
			{"name":"Read","input_schema":{"type":"object"}},
			{"name":"Write","input_schema":{"type":"object"}}
		],
		"system":[
			{"type":"text","text":"You are Claude Code."},
			{"type":"text","text":"system block 1","cache_control":{"type":"ephemeral","ttl":"1h"}},
			{"type":"text","text":"system block 2","cache_control":{"type":"ephemeral","ttl":"1h"}}
		],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"hi"},
				{"type":"text","text":"do something","cache_control":{"type":"ephemeral","ttl":"1h"}}
			]}
		]
	}`
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "sE2E")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Poll for the api_turn write to complete.
	var apiTurnID int64
	var capturedReqID string
	for i := 0; i < 100; i++ {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(MAX(request_id), ''), COALESCE(MAX(id), 0) FROM api_turns`).
			Scan(&n, &capturedReqID, &apiTurnID); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if capturedReqID != "msg_e2e_test" {
		t.Fatalf("api_turns.request_id = %q, want msg_e2e_test", capturedReqID)
	}

	// Poll for cache rows. The proxy persists them on the same
	// detached context as the api_turn but the engine + store
	// translation takes a few extra ms.
	var segments, breakpointSegs, events, entries int
	for i := 0; i < 100; i++ {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_segments WHERE api_turn_id = ?`, apiTurnID).Scan(&segments); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_segments WHERE api_turn_id = ? AND is_breakpoint = 1`, apiTurnID).Scan(&breakpointSegs); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_events WHERE api_turn_id = ?`, apiTurnID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cache_entries WHERE session_id = ? AND model = ?`, "sE2E", "claude-opus-4-8").Scan(&entries); err != nil {
			t.Fatal(err)
		}
		if segments > 0 && events > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if segments == 0 {
		t.Error("cache_segments empty — proxy did not feed engine, or PersistCacheObservation skipped writes")
	}
	if breakpointSegs < 3 {
		t.Errorf("cache_segments WHERE is_breakpoint=1 count = %d, want >= 3 (R1(a) shape: 2 system + 1 rolling message)", breakpointSegs)
	}
	if events == 0 {
		t.Error("cache_events empty — engine did not produce events")
	}
	if entries == 0 {
		t.Error("cache_entries empty — engine did not produce write entries despite CacheCreationTokens > 0")
	}

	// First-turn outcome should be reanchor.
	var firstCause string
	if err := s.db.QueryRowContext(ctx,
		`SELECT cause FROM cache_events WHERE api_turn_id = ? ORDER BY id ASC LIMIT 1`, apiTurnID).Scan(&firstCause); err != nil {
		t.Fatal(err)
	}
	if firstCause != "reanchor" {
		t.Errorf("first cache_event cause = %q, want reanchor", firstCause)
	}

	// Dedup gate fires for the captured msg_xxx — the cause Tier-2
	// callers consult before emitting (operator's C7 carry-forward).
	fires, err := s.CacheEventExistsForMessage(ctx, "sE2E", "msg_e2e_test")
	if err != nil {
		t.Fatal(err)
	}
	if !fires {
		t.Error("dedup gate did not fire for the proxy-written event; Tier-2 would double-write")
	}
}
