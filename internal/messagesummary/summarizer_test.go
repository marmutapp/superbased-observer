package messagesummary

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation"
)

// TestAnthropicSummarizer_HappyPath pins the request shape + response
// extraction against an httptest.Server stand-in for /v1/messages.
func TestAnthropicSummarizer_HappyPath(t *testing.T) {
	var sawAuth, sawAPIKey, sawVersion, sawCT string
	var sawBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawAPIKey = r.Header.Get("x-api-key")
		sawVersion = r.Header.Get("anthropic-version")
		sawCT = r.Header.Get("Content-Type")
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"  Tight summary line. "}]}`))
	}))
	t.Cleanup(srv.Close)

	s := NewAnthropicSummarizer(
		AuthCredentials{Authorization: "Bearer test-token"},
		SummarizerOptions{BaseURL: srv.URL},
	)
	out, err := s.Summarize(context.Background(), []conversation.Message{
		{Role: "user", Text: "long earlier message"},
		{Role: "assistant", Text: "earlier reply"},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "Tight summary line." {
		t.Errorf("summary trim: got %q, want %q", out, "Tight summary line.")
	}
	if sawAuth != "Bearer test-token" {
		t.Errorf("Authorization: got %q", sawAuth)
	}
	if sawAPIKey != "" {
		t.Errorf("x-api-key should be empty: %q", sawAPIKey)
	}
	if sawVersion != DefaultAnthropicVersion {
		t.Errorf("anthropic-version: got %q, want %q", sawVersion, DefaultAnthropicVersion)
	}
	if sawCT != "application/json" {
		t.Errorf("Content-Type: %q", sawCT)
	}

	var req map[string]any
	if err := json.Unmarshal(sawBody, &req); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if req["model"] != DefaultModel {
		t.Errorf("model: got %v, want %q", req["model"], DefaultModel)
	}
	if !strings.Contains(req["system"].(string), "summarising") {
		t.Errorf("system prompt missing summarisation instruction: %v", req["system"])
	}
}

// TestAnthropicSummarizer_APIKeyAuth pins the api-key code path:
// only x-api-key is set, no Authorization header.
func TestAnthropicSummarizer_APIKeyAuth(t *testing.T) {
	var sawAuth, sawAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawAPIKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"x"}]}`))
	}))
	t.Cleanup(srv.Close)

	s := NewAnthropicSummarizer(
		AuthCredentials{APIKey: "sk-ant-test"},
		SummarizerOptions{BaseURL: srv.URL},
	)
	if _, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if sawAuth != "" {
		t.Errorf("Authorization unexpectedly set: %q", sawAuth)
	}
	if sawAPIKey != "sk-ant-test" {
		t.Errorf("x-api-key: %q", sawAPIKey)
	}
}

// TestAnthropicSummarizer_NoCredsError pins the safety contract: an
// empty AuthCredentials returns an error rather than sending an
// unauthenticated request.
func TestAnthropicSummarizer_NoCredsError(t *testing.T) {
	s := NewAnthropicSummarizer(AuthCredentials{}, SummarizerOptions{})
	_, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}})
	if err == nil {
		t.Errorf("expected error on empty creds")
	}
}

// TestAnthropicSummarizer_UpstreamError pins that a non-2xx status
// surfaces the response body in the error message (truncated).
func TestAnthropicSummarizer_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	t.Cleanup(srv.Close)

	s := NewAnthropicSummarizer(AuthCredentials{Authorization: "x"}, SummarizerOptions{BaseURL: srv.URL})
	_, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention status: %v", err)
	}
	if !strings.Contains(err.Error(), "rate_limit_error") {
		t.Errorf("error should include body: %v", err)
	}
}

// TestFactory_ForReturnsNilWhenSessionMissing pins that a session
// without cached creds gets nil from For() — the pipeline then no-
// ops the rolling-summ pass.
func TestFactory_ForReturnsNilWhenSessionMissing(t *testing.T) {
	cache := NewAuthCache(0)
	f := NewFactory(cache, SummarizerOptions{})
	if got := f.For("missing"); got != nil {
		t.Errorf("expected nil for missing session, got %T", got)
	}
}

// TestFactory_ForReturnsSummarizerWhenSessionPresent pins that a
// session with cached creds gets a working summariser.
func TestFactory_ForReturnsSummarizerWhenSessionPresent(t *testing.T) {
	cache := NewAuthCache(0)
	cache.Set("sess-X", AuthCredentials{Authorization: "Bearer y"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(srv.Close)

	f := NewFactory(cache, SummarizerOptions{BaseURL: srv.URL})
	s := f.For("sess-X")
	if s == nil {
		t.Fatal("expected summariser for present session")
	}
	out, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "ok" {
		t.Errorf("summary: %q", out)
	}
}

// TestFactory_NilCacheNilFor pins that a nil-cache Factory returns
// nil from For() rather than panicking.
func TestFactory_NilCacheNilFor(t *testing.T) {
	var f *Factory
	if got := f.For("any"); got != nil {
		t.Errorf("nil factory should return nil")
	}
	f = NewFactory(nil, SummarizerOptions{})
	if got := f.For("any"); got != nil {
		t.Errorf("nil-cache factory should return nil")
	}
}
