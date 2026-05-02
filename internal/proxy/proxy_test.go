package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// fakeSink records inserted APITurns in memory for test assertions.
type fakeSink struct {
	mu    sync.Mutex
	turns []models.APITurn
}

func (f *fakeSink) InsertAPITurn(_ context.Context, t models.APITurn) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, t)
	return int64(len(f.turns)), nil
}

func (f *fakeSink) all() []models.APITurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.APITurn, len(f.turns))
	copy(out, f.turns)
	return out
}

// newTestProxy wires a Proxy against two test upstream servers. The returned
// closer shuts both down.
func newTestProxy(t *testing.T, anthropicHandler, openaiHandler http.Handler) (*Proxy, *fakeSink, func()) {
	t.Helper()
	anthUp := httptest.NewServer(anthropicHandler)
	oaiUp := httptest.NewServer(openaiHandler)
	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		Sink:              sink,
	})
	if err != nil {
		anthUp.Close()
		oaiUp.Close()
		t.Fatalf("proxy.New: %v", err)
	}
	return p, sink, func() {
		anthUp.Close()
		oaiUp.Close()
	}
}

func TestProxy_AnthropicNonStreaming(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"system":"You are helpful.","tools":[{"name":"tool1"}]}`
	const responseBody = `{"id":"msg_abc","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":17,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`

	var seenBody string
	var seenAuth string
	var seenXSession string
	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		seenAuth = r.Header.Get("X-Api-Key")
		seenXSession = r.Header.Get("X-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_xyz")
		_, _ = w.Write([]byte(responseBody))
	})
	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("openai upstream unexpectedly hit: %s", r.URL.Path)
	})

	p, sink, cleanup := newTestProxy(t, anth, oai)
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("X-Api-Key", "sk-ant-test")
	req.Header.Set("X-Session-Id", "sess-123")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	gotBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if string(gotBody) != responseBody {
		t.Errorf("response body: got %q want %q", gotBody, responseBody)
	}
	if seenBody != requestBody {
		t.Errorf("upstream saw body %q want %q", seenBody, requestBody)
	}
	if seenAuth != "sk-ant-test" {
		t.Errorf("upstream saw X-Api-Key %q want %q", seenAuth, "sk-ant-test")
	}
	if seenXSession != "" {
		t.Errorf("X-Session-Id leaked to upstream: %q", seenXSession)
	}

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	tr := turns[0]
	if tr.Provider != models.ProviderAnthropic {
		t.Errorf("provider: %s", tr.Provider)
	}
	if tr.Model != "claude-sonnet-4" {
		t.Errorf("model: %s", tr.Model)
	}
	if tr.SessionID != "sess-123" {
		t.Errorf("session id: %q", tr.SessionID)
	}
	if tr.RequestID != "msg_abc" {
		t.Errorf("request id: %q", tr.RequestID)
	}
	if tr.InputTokens != 42 || tr.OutputTokens != 17 {
		t.Errorf("tokens: in=%d out=%d", tr.InputTokens, tr.OutputTokens)
	}
	if tr.CacheReadTokens != 100 {
		t.Errorf("cache read: %d", tr.CacheReadTokens)
	}
	if tr.MessageCount != 1 {
		t.Errorf("message count: %d", tr.MessageCount)
	}
	if tr.ToolUseCount != 1 {
		t.Errorf("tool use count: %d", tr.ToolUseCount)
	}
	if tr.SystemPromptHash == "" {
		t.Error("expected non-empty system prompt hash")
	}
	if tr.StopReason != "end_turn" {
		t.Errorf("stop reason: %s", tr.StopReason)
	}
}

func TestProxy_OpenAINonStreaming(t *testing.T) {
	const requestBody = `{"model":"gpt-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}],"stream":false}`
	const responseBody = `{"id":"chatcmpl-1","model":"gpt-5","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":5}}}`

	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("anthropic upstream unexpectedly hit: %s", r.URL.Path)
	})
	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})

	p, sink, cleanup := newTestProxy(t, anth, oai)
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	tr := turns[0]
	if tr.Provider != models.ProviderOpenAI {
		t.Errorf("provider: %s", tr.Provider)
	}
	if tr.Model != "gpt-5" {
		t.Errorf("model: %s", tr.Model)
	}
	if tr.InputTokens != 11 || tr.OutputTokens != 3 {
		t.Errorf("tokens: in=%d out=%d", tr.InputTokens, tr.OutputTokens)
	}
	if tr.CacheReadTokens != 5 {
		t.Errorf("cache read: %d", tr.CacheReadTokens)
	}
	if tr.StopReason != "stop" {
		t.Errorf("stop reason: %s", tr.StopReason)
	}
	if tr.MessageCount != 2 {
		t.Errorf("message count: %d", tr.MessageCount)
	}
}

func TestProxy_AnthropicStreaming(t *testing.T) {
	// Real-shape Anthropic SSE: message_start carries input_tokens +
	// cache_*_input_tokens on message.usage; message_delta carries
	// output_tokens on its top-level usage and stop_reason on delta.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":120,"cache_read_input_tokens":500,"cache_creation_input_tokens":0,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		// Write in two halves to exercise chunk boundaries.
		mid := len(sse) / 2
		_, _ = w.Write([]byte(sse[:mid]))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(sse[mid:]))
		if flusher != nil {
			flusher.Flush()
		}
	})
	p, sink, cleanup := newTestProxy(t, anth, http.NotFoundHandler())
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if string(body) != sse {
		t.Errorf("body mismatch after tee")
	}

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	tr := turns[0]
	if tr.Model != "claude-sonnet-4" {
		t.Errorf("model: %q", tr.Model)
	}
	if tr.RequestID != "msg_1" {
		t.Errorf("request id: %q", tr.RequestID)
	}
	if tr.InputTokens != 120 {
		t.Errorf("input_tokens: %d", tr.InputTokens)
	}
	if tr.OutputTokens != 42 {
		t.Errorf("output_tokens: %d (expected delta to override message_start placeholder)", tr.OutputTokens)
	}
	if tr.CacheReadTokens != 500 {
		t.Errorf("cache_read: %d", tr.CacheReadTokens)
	}
	if tr.StopReason != "end_turn" {
		t.Errorf("stop_reason: %q", tr.StopReason)
	}
}

// TestProxy_AnthropicStreamingZeroUsageDropped guards against audit item B2:
// upstream Anthropic SSE that delivers a model in message_start but never
// produces a usage-bearing message_delta (cancelled request, mid-flight
// error, or a malformed envelope) used to land as a zero-everything row in
// api_turns. 14 of 21 live install rows had this shape. The isEmptyUsage
// gate now drops them.
func TestProxy_AnthropicStreamingZeroUsageDropped(t *testing.T) {
	// Stream has model + id in message_start but no usage block — and no
	// message_delta to fill in tokens.
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_zero","model":"claude-haiku-4-5"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	p, sink, cleanup := newTestProxy(t, anth, http.NotFoundHandler())
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-haiku-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if n := len(sink.all()); n != 0 {
		t.Errorf("want 0 turns when stream carries model but zero usage, got %d (see audit B2)", n)
	}
}

func TestProxy_OpenAIStreamingWithUsage(t *testing.T) {
	// OpenAI include_usage=true emits a final non-delta chunk with top-level
	// usage right before [DONE].
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-stream","model":"gpt-5","choices":[{"delta":{"content":"hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-stream","model":"gpt-5","choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"id":"chatcmpl-stream","model":"gpt-5","choices":[],"usage":{"prompt_tokens":33,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":10}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(sse))
		if flusher != nil {
			flusher.Flush()
		}
	})
	p, sink, cleanup := newTestProxy(t, http.NotFoundHandler(), oai)
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	tr := turns[0]
	if tr.Model != "gpt-5" {
		t.Errorf("model: %q", tr.Model)
	}
	if tr.InputTokens != 33 || tr.OutputTokens != 7 {
		t.Errorf("tokens: in=%d out=%d", tr.InputTokens, tr.OutputTokens)
	}
	if tr.CacheReadTokens != 10 {
		t.Errorf("cache_read: %d", tr.CacheReadTokens)
	}
	if tr.StopReason != "stop" {
		t.Errorf("stop_reason: %q", tr.StopReason)
	}
}

func TestProxy_OpenAIStreamingNoUsageDropped(t *testing.T) {
	// Without include_usage, we see no usage block. The tee must pass the
	// body through but drop the turn (zero tokens, empty model on some
	// chunks). Spec §24: unreliable tokens aren't worth recording as
	// "accurate"; leave them for the JSONL adapter to capture.
	//
	// Here the stream has no usage block AND no model on any chunk — the
	// serve() check on turn.Model drops it. The B2 isEmptyUsage gate
	// (added later) drops it again belt-and-suspenders if a model surfaces
	// elsewhere; the regression test for that case is
	// TestProxy_AnthropicStreamingZeroUsageDropped below.
	sse := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	})
	p, sink, cleanup := newTestProxy(t, http.NotFoundHandler(), oai)
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if n := len(sink.all()); n != 0 {
		t.Errorf("want 0 turns when neither stream nor request carries a model, got %d", n)
	}
}

// TestProxy_UpstreamError pins the v1.4.20 capture: non-2xx upstream
// responses are forwarded unchanged AND recorded as zero-token api_turn
// rows with HTTPStatus / ErrorClass / ErrorMessage populated. Pre-fix
// these were dropped silently (`if resp.StatusCode < 200 || >= 300 { return }`),
// matching the JSONL-side gap the same release closed.
func TestProxy_UpstreamError(t *testing.T) {
	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_011err400")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages: array too short"}}`))
	})
	p, sink, cleanup := newTestProxy(t, anth, http.NotFoundHandler())
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status: %d want 400", resp.StatusCode)
	}

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 captured error turn, got %d", len(turns))
	}
	turn := turns[0]
	if turn.HTTPStatus != 400 {
		t.Errorf("http_status: %d want 400", turn.HTTPStatus)
	}
	if turn.ErrorClass != "invalid_request_error" {
		t.Errorf("error_class: %q want invalid_request_error", turn.ErrorClass)
	}
	if !strings.Contains(turn.ErrorMessage, "array too short") {
		t.Errorf("error_message: %q", turn.ErrorMessage)
	}
	if turn.RequestID != "req_011err400" {
		t.Errorf("request_id: %q want req_011err400", turn.RequestID)
	}
	if turn.Model != "claude-sonnet-4-6" {
		t.Errorf("model: %q want claude-sonnet-4-6 (from request body)", turn.Model)
	}
	if turn.Provider != models.ProviderAnthropic {
		t.Errorf("provider: %q", turn.Provider)
	}
	// Errors don't consume billing tokens — the row should be zero-token.
	if turn.InputTokens != 0 || turn.OutputTokens != 0 {
		t.Errorf("expected zero tokens on error: in=%d out=%d", turn.InputTokens, turn.OutputTokens)
	}
}

// TestProxy_UpstreamRateLimit pins 429 + the rate_limit_error class.
func TestProxy_UpstreamRateLimit(t *testing.T) {
	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_011rate")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"}}`))
	})
	p, sink, cleanup := newTestProxy(t, anth, http.NotFoundHandler())
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-7"}`))
	resp.Body.Close()

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].HTTPStatus != 429 || turns[0].ErrorClass != "rate_limit_error" {
		t.Errorf("turn = %+v", turns[0])
	}
}

// TestProxy_OpenAIErrorEnvelope pins capture against OpenAI's slightly
// different error shape (no top-level `type: "error"` wrapper, just
// `{error: {type, message, code}}`). parseErrorBody handles both.
func TestProxy_OpenAIErrorEnvelope(t *testing.T) {
	openai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_oaerr")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error","code":"invalid_api_key"}}`))
	})
	p, sink, cleanup := newTestProxy(t, http.NotFoundHandler(), openai)
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-5"}`))
	resp.Body.Close()

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].HTTPStatus != 401 || turns[0].ErrorClass != "invalid_request_error" {
		t.Errorf("turn = %+v", turns[0])
	}
	if !strings.Contains(turns[0].ErrorMessage, "Invalid API key") {
		t.Errorf("message: %q", turns[0].ErrorMessage)
	}
}

// TestProxy_StreamUpstreamError covers the streaming path: a non-2xx
// response code on a streaming endpoint records an error turn the same
// way the non-streaming path does. The captured body might be a plain
// JSON error (when the upstream rejects before any SSE events) or an
// SSE `event: error` envelope; extractStreamErrorBody handles both.
func TestProxy_StreamUpstreamError(t *testing.T) {
	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req_011stream_err")
		w.WriteHeader(http.StatusServiceUnavailable)
		// Anthropic's overloaded path emits the same JSON body shape
		// regardless of stream/non-stream when it rejects pre-flight.
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	})
	p, sink, cleanup := newTestProxy(t, anth, http.NotFoundHandler())
	defer cleanup()
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, _ := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true}`))
	resp.Body.Close()

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].HTTPStatus != 503 || turns[0].ErrorClass != "overloaded_error" {
		t.Errorf("turn = %+v", turns[0])
	}
}

func TestProxy_UpstreamUnreachable(t *testing.T) {
	// An unparseable upstream URL is rejected at New(). An unreachable one
	// surfaces as 502 at request time.
	p, err := New(Options{
		AnthropicUpstream: "http://127.0.0.1:1", // port 1 is unreachable
		OpenAIUpstream:    "http://127.0.0.1:1",
		Sink:              &fakeSink{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", resp.StatusCode)
	}
}

// TestProxy_RetriesTransientConnectionReset pins the v1.4.24 retry
// behaviour: the proxy should swallow ONE "connection reset by peer"
// from the upstream transport and re-issue the request on a fresh
// connection. WSL2 / corporate-firewall NATs frequently close idle
// keep-alive entries before our IdleConnTimeout expires; without
// retry the user sees a 502 on what should be a successful call.
//
// Uses a custom RoundTripper since httptest can't simulate TCP-level
// connection resets cleanly.
func TestProxy_RetriesTransientConnectionReset(t *testing.T) {
	const responseBody = `{"id":"msg_R","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	var attempts int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			// Mid-write transient — exactly the shape Claude Code reports.
			return nil, errors.New(`Post "https://api.anthropic.com/v1/messages": write tcp 127.0.0.1:54321->1.2.3.4:443: write: connection reset by peer`)
		}
		body := strings.NewReader(responseBody)
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(body),
			Header:     http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_R"}},
		}, nil
	})

	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		OpenAIUpstream:    "https://api.openai.com",
		Sink:              sink,
		Client:            &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status: got %d want 200 (retry should have succeeded)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("attempts: got %d want 2 (one fail + one retry)", got)
	}
	if len(sink.all()) != 1 {
		t.Errorf("turns recorded: got %d want 1 (only successful retry should land)", len(sink.all()))
	}
}

// TestProxy_DoesNotRetryNonTransient confirms that we do NOT retry on
// non-transient errors — TLS handshake failures, dial timeouts, etc.
// The retry budget exists for stale-keep-alive recovery, not for
// masking real upstream outages.
func TestProxy_DoesNotRetryNonTransient(t *testing.T) {
	var attempts int32
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&attempts, 1)
		return nil, errors.New(`Post "...": tls: handshake failure`)
	})

	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: "https://api.anthropic.com",
		OpenAIUpstream:    "https://api.openai.com",
		Sink:              sink,
		Client:            &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status: got %d want 502", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts: got %d want 1 (TLS handshake should not retry)", got)
	}
}

// roundTripFunc is a tiny adapter so tests can pass a func as
// http.RoundTripper without spinning up a real httptest server (we
// can't simulate TCP-level resets through a real server).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProxy_NewValidation(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("missing sink should error")
	}
	if _, err := New(Options{Sink: &fakeSink{}, AnthropicUpstream: "not a url"}); err == nil {
		t.Error("bad upstream url should error")
	}
}

func TestProviderForPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/messages", models.ProviderAnthropic},
		{"/v1/messages/batches", models.ProviderAnthropic},
		{"/v1/chat/completions", models.ProviderOpenAI},
		{"/v1/responses", models.ProviderOpenAI},
		{"/v1/embeddings", models.ProviderOpenAI},
		{"/", models.ProviderAnthropic},
		{"/health", models.ProviderAnthropic},
	}
	for _, tc := range cases {
		if got := providerForPath(tc.path); got != tc.want {
			t.Errorf("providerForPath(%q) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

// fakeCompressor is a test double for the Compressor interface. It
// returns a fixed CompressionResult and records the body it received so
// assertions can verify the proxy fed the correct input.
type fakeCompressor struct {
	mu        sync.Mutex
	received  []byte
	provider  string
	result    CompressionResult
	callCount int
}

func (f *fakeCompressor) Compress(_ context.Context, provider string, body []byte) CompressionResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append([]byte{}, body...)
	f.provider = provider
	f.callCount++
	return f.result
}

// fakeObsLog captures observer_log writes from the proxy.
type fakeObsLog struct {
	mu      sync.Mutex
	entries []obsEntry
}

type obsEntry struct {
	level, component, message, details string
}

func (f *fakeObsLog) InsertObserverLog(_ context.Context, level, component, message, details string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, obsEntry{level, component, message, details})
	return nil
}

func TestProxy_CompressorRunsBeforeForward(t *testing.T) {
	const clientBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"original input that will not reach upstream"}]}`
	const compressedBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"compressed"}]}`
	const responseBody = `{"id":"msg_abc","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`

	var seenBody string
	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})
	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("openai unexpected") })

	anthUp := httptest.NewServer(anth)
	oaiUp := httptest.NewServer(oai)
	defer anthUp.Close()
	defer oaiUp.Close()

	sink := &fakeSink{}
	comp := &fakeCompressor{
		result: CompressionResult{
			Body:              []byte(compressedBody),
			Skipped:           false,
			MessagePrefixHash: "deadbeef",
			OriginalBytes:     len(clientBody),
			CompressedBytes:   len(compressedBody),
			CompressedCount:   1,
		},
	}
	obs := &fakeObsLog{}

	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		Sink:              sink,
		Compressor:        comp,
		ObserverLog:       obs,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client POST: %v", err)
	}
	_ = resp.Body.Close()

	if comp.callCount != 1 {
		t.Errorf("compressor call count = %d, want 1", comp.callCount)
	}
	if string(comp.received) != clientBody {
		t.Errorf("compressor received %q, want client body", comp.received)
	}
	if seenBody != compressedBody {
		t.Errorf("upstream saw %q, want compressed body %q", seenBody, compressedBody)
	}
	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if turns[0].MessagePrefixHash != "deadbeef" {
		t.Errorf("turn.MessagePrefixHash = %q, want deadbeef", turns[0].MessagePrefixHash)
	}
	obs.mu.Lock()
	entries := obs.entries
	obs.mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("observer_log entries = %d, want 1", len(entries))
	}
	if entries[0].component != "compress" {
		t.Errorf("observer_log component = %q, want compress", entries[0].component)
	}
	if !strings.Contains(entries[0].details, `"original_bytes":`+strconv.Itoa(len(clientBody))) {
		t.Errorf("observer_log details missing original_bytes: %q", entries[0].details)
	}
}

func TestProxy_SkippedCompressionLeavesBodyIntact(t *testing.T) {
	const clientBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`
	const responseBody = `{"id":"msg_abc","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`

	var seenBody string
	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})
	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("openai unexpected") })
	anthUp := httptest.NewServer(anth)
	oaiUp := httptest.NewServer(oai)
	defer anthUp.Close()
	defer oaiUp.Close()

	sink := &fakeSink{}
	comp := &fakeCompressor{result: CompressionResult{Skipped: true}}
	obs := &fakeObsLog{}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		Sink:              sink,
		Compressor:        comp,
		ObserverLog:       obs,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(clientBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client POST: %v", err)
	}
	_ = resp.Body.Close()

	if seenBody != clientBody {
		t.Errorf("upstream saw %q, want original body", seenBody)
	}
	obs.mu.Lock()
	entries := obs.entries
	obs.mu.Unlock()
	if len(entries) != 0 {
		t.Errorf("skipped compression should not log to observer_log, got %d entries", len(entries))
	}
	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(turns))
	}
	if turns[0].MessagePrefixHash != "" {
		t.Errorf("skipped: turn.MessagePrefixHash = %q, want empty", turns[0].MessagePrefixHash)
	}
}

// fakeResolver implements SessionResolver for tests. It returns the
// pre-seeded session_id for one remote addr and records every call.
type fakeResolver struct {
	mu        sync.Mutex
	sessionID string
	ok        bool
	err       error
	calls     []string
}

func (f *fakeResolver) Resolve(_ context.Context, remoteAddr string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, remoteAddr)
	return f.sessionID, f.ok, f.err
}

func (f *fakeResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestProxy_SessionResolverFillsMissingHeader(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`
	const responseBody = `{"id":"msg_abc","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	anth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})
	oai := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("openai upstream unexpectedly hit")
	})
	anthUp := httptest.NewServer(anth)
	oaiUp := httptest.NewServer(oai)
	defer anthUp.Close()
	defer oaiUp.Close()

	sink := &fakeSink{}
	resolver := &fakeResolver{sessionID: "sess-from-bridge", ok: true}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		Sink:              sink,
		SessionResolver:   resolver,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	// No X-Session-Id — expect resolver to fill it in.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].SessionID != "sess-from-bridge" {
		t.Errorf("session id: got %q want %q", turns[0].SessionID, "sess-from-bridge")
	}
	if resolver.callCount() != 1 {
		t.Errorf("resolver call count: got %d want 1", resolver.callCount())
	}
}

func TestProxy_SessionResolverNotCalledWhenHeaderPresent(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`
	const responseBody = `{"id":"msg_abc","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	anth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})
	anthUp := httptest.NewServer(anth)
	defer anthUp.Close()

	sink := &fakeSink{}
	resolver := &fakeResolver{sessionID: "sess-from-bridge", ok: true}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    "https://unused.example",
		Sink:              sink,
		SessionResolver:   resolver,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "header-wins")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].SessionID != "header-wins" {
		t.Errorf("session id: got %q want %q", turns[0].SessionID, "header-wins")
	}
	if resolver.callCount() != 0 {
		t.Errorf("resolver should not be called when header is present (called %d times)", resolver.callCount())
	}
}

func TestProxy_SessionResolverMissLeavesNull(t *testing.T) {
	const requestBody = `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`
	const responseBody = `{"id":"msg_abc","model":"claude-sonnet-4","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	anth := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	})
	anthUp := httptest.NewServer(anth)
	defer anthUp.Close()

	sink := &fakeSink{}
	resolver := &fakeResolver{ok: false} // clean miss
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    "https://unused.example",
		Sink:              sink,
		SessionResolver:   resolver,
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	resp.Body.Close()

	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	if turns[0].SessionID != "" {
		t.Errorf("session id: got %q want empty", turns[0].SessionID)
	}
}

// TestParseAnthropicResponse_CacheCreationTierBreakdown verifies the
// non-streaming response parser captures both
// usage.cache_creation_input_tokens (the legacy total) and the per-tier
// breakdown under usage.cache_creation. The 1h subset must land in
// CacheCreation1hTokens so the cost engine can bill it at the higher
// rate. Audit item C5.
func TestParseAnthropicResponse_CacheCreationTierBreakdown(t *testing.T) {
	body := []byte(`{
		"id": "msg_abc",
		"model": "claude-sonnet-4",
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"cache_read_input_tokens": 1000,
			"cache_creation_input_tokens": 600,
			"cache_creation": {
				"ephemeral_5m_input_tokens": 400,
				"ephemeral_1h_input_tokens": 200
			}
		}
	}`)
	got := parseAnthropicResponse(body)
	if got.CacheCreationTokens != 600 {
		t.Errorf("cache_creation total: got %d want 600", got.CacheCreationTokens)
	}
	if got.CacheCreation1hTokens != 200 {
		t.Errorf("cache_creation 1h: got %d want 200", got.CacheCreation1hTokens)
	}

	// Newer-only shape: breakdown present without the legacy total.
	bodyNew := []byte(`{
		"id": "msg_xyz",
		"model": "claude-sonnet-4",
		"usage": {
			"input_tokens": 50,
			"output_tokens": 25,
			"cache_creation": {
				"ephemeral_5m_input_tokens": 300,
				"ephemeral_1h_input_tokens": 150
			}
		}
	}`)
	got = parseAnthropicResponse(bodyNew)
	if got.CacheCreationTokens != 450 {
		t.Errorf("cache_creation total derived: got %d want 450", got.CacheCreationTokens)
	}
	if got.CacheCreation1hTokens != 150 {
		t.Errorf("cache_creation 1h derived: got %d want 150", got.CacheCreation1hTokens)
	}

	// Legacy-only shape (current data): cache_creation total is set, no
	// breakdown — 1h portion is zero.
	bodyLegacy := []byte(`{
		"id": "msg_old",
		"model": "claude-sonnet-4",
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"cache_creation_input_tokens": 800
		}
	}`)
	got = parseAnthropicResponse(bodyLegacy)
	if got.CacheCreationTokens != 800 {
		t.Errorf("cache_creation legacy: got %d want 800", got.CacheCreationTokens)
	}
	if got.CacheCreation1hTokens != 0 {
		t.Errorf("cache_creation 1h on legacy shape: got %d want 0", got.CacheCreation1hTokens)
	}
}

// TestParseAnthropicStream_CacheCreationTierBreakdown verifies the SSE
// parser captures the 1h tier subset from message_start.message.usage
// and exposes it as CacheCreation1hTokens.
func TestParseAnthropicStream_CacheCreationTierBreakdown(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4","usage":{"input_tokens":120,"cache_read_input_tokens":500,"cache_creation_input_tokens":600,"cache_creation":{"ephemeral_5m_input_tokens":400,"ephemeral_1h_input_tokens":200},"output_tokens":1}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
	}, "\n")
	got := parseSSEStream([]byte(sse), models.ProviderAnthropic)
	if got.CacheCreationTokens != 600 {
		t.Errorf("cache_creation total: got %d want 600", got.CacheCreationTokens)
	}
	if got.CacheCreation1hTokens != 200 {
		t.Errorf("cache_creation 1h: got %d want 200", got.CacheCreation1hTokens)
	}

	// 1h breakdown lands on message_delta in some Anthropic SSE variants.
	sseDelta := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_2","model":"claude-sonnet-4","usage":{"input_tokens":100,"output_tokens":1}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":200,"ephemeral_1h_input_tokens":100}}}`,
		``,
	}, "\n")
	got = parseSSEStream([]byte(sseDelta), models.ProviderAnthropic)
	if got.CacheCreationTokens != 300 {
		t.Errorf("delta cache_creation total: got %d want 300", got.CacheCreationTokens)
	}
	if got.CacheCreation1hTokens != 100 {
		t.Errorf("delta cache_creation 1h: got %d want 100", got.CacheCreation1hTokens)
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct {
		base, path, want string
	}{
		{"", "/v1/messages", "/v1/messages"},
		{"/", "/v1/messages", "/v1/messages"},
		{"/proxy", "/v1/messages", "/proxy/v1/messages"},
		{"/proxy/", "/v1/messages", "/proxy/v1/messages"},
	}
	for _, tc := range cases {
		if got := joinPath(tc.base, tc.path); got != tc.want {
			t.Errorf("joinPath(%q,%q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}
