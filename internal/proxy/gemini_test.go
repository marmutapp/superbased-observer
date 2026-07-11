package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestProviderForPath_Gemini(t *testing.T) {
	cases := map[string]string{
		"/v1beta/models/gemini-2.5-flash:generateContent":     models.ProviderGoogle,
		"/v1beta/models/gemini-2.5-pro:streamGenerateContent": models.ProviderGoogle,
		"/v1beta/models/gemini-2.5-flash:countTokens":         models.ProviderGoogle,
		"/v1/messages":         models.ProviderAnthropic,
		"/v1/chat/completions": models.ProviderOpenAI,
		"/v1/models":           models.ProviderOpenAI, // OpenAI listing, not Gemini
	}
	for path, want := range cases {
		if got := providerForPath(path); got != want {
			t.Errorf("providerForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestParseGeminiResponse_Object(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"cachedContentTokenCount":4,"thoughtsTokenCount":2},` +
		`"modelVersion":"gemini-2.5-flash","responseId":"resp-1"}`)
	got := parseGeminiResponse(body)
	if got.InputTokens != 6 { // 10 - 4 cached
		t.Errorf("InputTokens = %d, want 6 (net of cached)", got.InputTokens)
	}
	if got.OutputTokens != 5 { // 3 candidates + 2 thoughts
		t.Errorf("OutputTokens = %d, want 5 (candidates+thoughts)", got.OutputTokens)
	}
	if got.CacheReadTokens != 4 {
		t.Errorf("CacheReadTokens = %d, want 4", got.CacheReadTokens)
	}
	if got.Model != "gemini-2.5-flash" || got.RequestID != "resp-1" || got.StopReason != "STOP" {
		t.Errorf("meta = %+v", got)
	}
}

func TestParseGeminiResponse_StreamingArrayTakesLastChunk(t *testing.T) {
	// Non-SSE streaming returns a JSON array of chunks; usage is cumulative
	// in the terminal chunk.
	body := []byte(`[` +
		`{"candidates":[{"content":{"parts":[{"text":"h"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1}},` +
		`{"candidates":[{"content":{"parts":[{"text":"i"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4},"modelVersion":"gemini-2.5-flash"}` +
		`]`)
	got := parseGeminiResponse(body)
	if got.InputTokens != 10 || got.OutputTokens != 4 || got.StopReason != "STOP" || got.Model != "gemini-2.5-flash" {
		t.Errorf("array parse = %+v, want last-chunk totals", got)
	}
}

func TestParseGeminiStream_SSE(t *testing.T) {
	body := []byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"h\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":1}}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"i\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":3,\"cachedContentTokenCount\":2},\"modelVersion\":\"gemini-2.5-pro\"}\n\n")
	got := parseGeminiStream(body)
	if got.InputTokens != 6 { // 8 - 2 cached
		t.Errorf("InputTokens = %d, want 6", got.InputTokens)
	}
	if got.OutputTokens != 3 {
		t.Errorf("OutputTokens = %d, want 3", got.OutputTokens)
	}
	if got.CacheReadTokens != 2 || got.StopReason != "STOP" || got.Model != "gemini-2.5-pro" {
		t.Errorf("stream meta = %+v", got)
	}
}

// TestProxy_GeminiRoutesAndCapturesUsage is the end-to-end Phase E check: a
// generateContent request reaches the Gemini upstream at the canonical path,
// the query string (?key=) is preserved, and usageMetadata lands in api_turns
// as provider=google.
func TestProxy_GeminiRoutesAndCapturesUsage(t *testing.T) {
	const responseBody = `{"candidates":[{"content":{"parts":[{"text":"hi there"}],"role":"model"},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":5,"cachedContentTokenCount":2},"modelVersion":"gemini-2.5-flash"}`

	var gotPath, gotQuery string
	gemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer gemini.Close()

	anth := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("anthropic upstream unexpectedly hit: %s", r.URL.Path)
	})
	oai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("openai upstream unexpectedly hit: %s", r.URL.Path)
	})
	anthUp := httptest.NewServer(anth)
	defer anthUp.Close()
	oaiUp := httptest.NewServer(oai)
	defer oaiUp.Close()
	sink := &fakeSink{}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL,
		OpenAIUpstream:    oaiUp.URL,
		GeminiUpstream:    gemini.URL,
		Sink:              sink,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(
		ts.URL+"/v1beta/models/gemini-2.5-flash:generateContent?key=AIzaTEST",
		"application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotPath != "/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotQuery != "key=AIzaTEST" {
		t.Errorf("upstream query = %q, want key=AIzaTEST preserved", gotQuery)
	}
	turns := sink.all()
	if len(turns) != 1 {
		t.Fatalf("want 1 turn, got %d", len(turns))
	}
	tr := turns[0]
	if tr.Provider != models.ProviderGoogle {
		t.Errorf("provider = %s, want google", tr.Provider)
	}
	if tr.InputTokens != 10 || tr.OutputTokens != 5 || tr.CacheReadTokens != 2 {
		t.Errorf("tokens in=%d out=%d cacheRead=%d (want 10/5/2)", tr.InputTokens, tr.OutputTokens, tr.CacheReadTokens)
	}
	if tr.Model != "gemini-2.5-flash" {
		t.Errorf("model = %q", tr.Model)
	}
}
