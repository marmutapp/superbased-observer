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

// TestOpenAISummarizer_HappyPath pins the Responses API request shape +
// response extraction against an httptest.Server stand-in for
// /v1/responses. Mirrors TestAnthropicSummarizer_HappyPath.
func TestOpenAISummarizer_HappyPath(t *testing.T) {
	var sawAuth, sawCT, sawPath string
	var sawBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawCT = r.Header.Get("Content-Type")
		sawPath = r.URL.Path
		sawBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "output": [{"type":"message","content":[{"type":"output_text","text":"  Dense factual summary line. "}]}],
            "usage": {"input_tokens": 1234, "output_tokens": 56, "input_tokens_details": {"cached_tokens": 800}}
        }`))
	}))
	t.Cleanup(srv.Close)

	s := NewOpenAISummarizer(
		AuthCredentials{Authorization: "Bearer sk-test"},
		OpenAISummarizerOptions{BaseURL: srv.URL},
	)
	out, err := s.Summarize(context.Background(), []conversation.Message{
		{Role: "user", Text: "long earlier message"},
		{Role: "assistant", Text: "earlier reply"},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if out != "Dense factual summary line." {
		t.Errorf("summary trim: got %q, want %q", out, "Dense factual summary line.")
	}
	if sawAuth != "Bearer sk-test" {
		t.Errorf("Authorization: got %q", sawAuth)
	}
	if sawCT != "application/json" {
		t.Errorf("Content-Type: %q", sawCT)
	}
	if sawPath != "/v1/responses" {
		t.Errorf("path: got %q, want /v1/responses", sawPath)
	}

	var req map[string]any
	if err := json.Unmarshal(sawBody, &req); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if req["model"] != DefaultOpenAIModel {
		t.Errorf("model: got %v, want %q", req["model"], DefaultOpenAIModel)
	}
	if !strings.Contains(req["instructions"].(string), "summarising") {
		t.Errorf("instructions missing summarisation instruction: %v", req["instructions"])
	}
	if req["stream"] != false {
		t.Errorf("expected stream=false, got %v", req["stream"])
	}
}

// TestOpenAISummarizer_NoAuthError pins the "no Authorization" guard.
func TestOpenAISummarizer_NoAuthError(t *testing.T) {
	s := NewOpenAISummarizer(AuthCredentials{}, OpenAISummarizerOptions{})
	_, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}})
	if err == nil {
		t.Fatal("expected error for missing Authorization, got nil")
	}
	if !strings.Contains(err.Error(), "no Authorization") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestOpenAISummarizer_UpstreamError surfaces upstream failures so the
// pipeline can fall back gracefully.
func TestOpenAISummarizer_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	t.Cleanup(srv.Close)

	s := NewOpenAISummarizer(
		AuthCredentials{Authorization: "Bearer sk-bad"},
		OpenAISummarizerOptions{BaseURL: srv.URL},
	)
	_, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}})
	if err == nil {
		t.Fatal("expected upstream error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got %v", err)
	}
}

// TestOpenAISummarizer_RecorderCapturesUsage pins the SummaryCall
// recording: the Responses API's input_tokens_details.cached_tokens
// maps onto SummaryCall.CacheReadTokens.
func TestOpenAISummarizer_RecorderCapturesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
            "output": [{"type":"message","content":[{"type":"output_text","text":"x"}]}],
            "usage": {"input_tokens": 1000, "output_tokens": 200, "input_tokens_details": {"cached_tokens": 750}}
        }`))
	}))
	t.Cleanup(srv.Close)

	rec := &fakeRecorder{}
	s := NewOpenAISummarizer(
		AuthCredentials{Authorization: "Bearer sk-test"},
		OpenAISummarizerOptions{BaseURL: srv.URL, Recorder: rec, SessionID: "s-1"},
	)
	if _, err := s.Summarize(context.Background(), []conversation.Message{{Role: "u", Text: "x"}}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("recorder calls: got %d, want 1", len(rec.calls))
	}
	got := rec.calls[0]
	if got.SessionID != "s-1" || got.Model != DefaultOpenAIModel {
		t.Errorf("session/model: got %+v", got)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 200 || got.CacheReadTokens != 750 {
		t.Errorf("token counts: got %+v", got)
	}
	if got.CacheCreationTokens != 0 {
		t.Errorf("CacheCreationTokens should be 0 on OpenAI (no separate write tier); got %d", got.CacheCreationTokens)
	}
}

// TestOpenAIFactory_ChatGPTJWTReturnsNil pins the JWT-degrade path:
// when the cached Authorization is a Bearer eyJ... JWT (ChatGPT-Plus
// subscription), the factory returns nil so D20 no-ops gracefully.
// Routing JWTs to api.openai.com would 401 on every summary call.
func TestOpenAIFactory_ChatGPTJWTReturnsNil(t *testing.T) {
	cache := NewAuthCache(8)
	cache.Set("session-jwt", AuthCredentials{Authorization: "Bearer eyJabc.def.ghi"})
	f := NewOpenAIFactory(cache, OpenAISummarizerOptions{})
	if got := f.For("session-jwt"); got != nil {
		t.Errorf("ChatGPT JWT must return nil summariser; got %T", got)
	}
}

// TestOpenAIFactory_APIKeyReturnsSummarizer pins the happy path: a
// regular sk-... API key produces a real summariser.
func TestOpenAIFactory_APIKeyReturnsSummarizer(t *testing.T) {
	cache := NewAuthCache(8)
	cache.Set("session-api", AuthCredentials{Authorization: "Bearer sk-test"})
	f := NewOpenAIFactory(cache, OpenAISummarizerOptions{})
	got := f.For("session-api")
	if got == nil {
		t.Fatal("expected summariser for sk- API key, got nil")
	}
}

// TestOpenAIFactory_NoCredsReturnsNil pins the missing-creds path.
func TestOpenAIFactory_NoCredsReturnsNil(t *testing.T) {
	cache := NewAuthCache(8)
	f := NewOpenAIFactory(cache, OpenAISummarizerOptions{})
	if got := f.For("nope"); got != nil {
		t.Errorf("expected nil for missing session, got %T", got)
	}
}

// fakeRecorder collects SummaryCall rows for assertions. Tests in this
// package share the type; redefine here to keep the file self-contained.
type fakeRecorder struct {
	calls []SummaryCall
}

func (f *fakeRecorder) Record(_ context.Context, c SummaryCall) error {
	f.calls = append(f.calls, c)
	return nil
}
