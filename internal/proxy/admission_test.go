package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestExtractLastUserText(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		body     string
		want     string
	}{
		{
			name: "anthropic string content", provider: models.ProviderAnthropic,
			body: `{"messages":[{"role":"user","content":"hello there"}]}`, want: "hello there",
		},
		{
			name: "anthropic block content, last user wins", provider: models.ProviderAnthropic,
			body: `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"hi"},` +
				`{"role":"user","content":[{"type":"text","text":"second ask"}]}]}`,
			want: "second ask",
		},
		{
			name: "anthropic tool_result-only last user → empty", provider: models.ProviderAnthropic,
			body: `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`, want: "",
		},
		{
			name: "openai chat messages", provider: models.ProviderOpenAI,
			body: `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"chat ask"}]}`, want: "chat ask",
		},
		{
			name: "openai responses input string", provider: models.ProviderOpenAI,
			body: `{"input":"responses ask"}`, want: "responses ask",
		},
		{
			name: "openai responses input array input_text", provider: models.ProviderOpenAI,
			body: `{"input":[{"role":"user","content":[{"type":"input_text","text":"typed ask"}]}]}`, want: "typed ask",
		},
		{
			name: "unparseable → empty", provider: models.ProviderAnthropic,
			body: `not json`, want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractLastUserText(tt.provider, []byte(tt.body)); got != tt.want {
				t.Errorf("extractLastUserText = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteAdmissionRefusalShapes(t *testing.T) {
	t.Run("anthropic", func(t *testing.T) {
		w := httptest.NewRecorder()
		writeAdmissionRefusal(w, models.ProviderAnthropic, "off-scope")
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403", w.Code)
		}
		var body struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Type != "error" || body.Error.Type != "permission_error" || body.Error.Message != "off-scope" {
			t.Errorf("anthropic refusal body = %s", w.Body.String())
		}
	})
	t.Run("openai", func(t *testing.T) {
		w := httptest.NewRecorder()
		writeAdmissionRefusal(w, models.ProviderOpenAI, "off-scope")
		var body struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Error.Code != "admission_denied" || body.Error.Message != "off-scope" {
			t.Errorf("openai refusal body = %s", w.Body.String())
		}
	})
}

// fakeAdmitter is a test Admitter that records the text it saw.
type fakeAdmitter struct {
	block   bool
	reason  string
	called  bool
	gotText string
	gotUser string
}

func (f *fakeAdmitter) Admit(_ context.Context, in AdmitInput) AdmitResult {
	f.called = true
	f.gotText = in.Text
	f.gotUser = in.User
	return AdmitResult{Block: f.block, Reason: f.reason, Criterion: "AD-100"}
}

// TestProxyAdmissionThreadsUserHeader proves the configured AdmissionUserHeader
// is read off the request and threaded into AdmitInput.User (the org-hosted-app
// per-end-user identity path).
func TestProxyAdmissionThreadsUserHeader(t *testing.T) {
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer anthUp.Close()

	adm := &fakeAdmitter{block: false}
	p, err := New(Options{
		AnthropicUpstream: anthUp.URL, Sink: &fakeSink{}, Admitter: adm,
		AdmissionUserHeader: "X-Superbased-User",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Superbased-User", "alice@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if !adm.called {
		t.Fatal("admitter was not consulted")
	}
	if adm.gotUser != "alice@example.com" {
		t.Errorf("admitter saw user %q, want alice@example.com", adm.gotUser)
	}
}

// TestProxyAdmissionBlocksBeforeForward proves an enforce-mode block short-
// circuits with a provider-shaped 403 and never touches the upstream.
func TestProxyAdmissionBlocksBeforeForward(t *testing.T) {
	anthUp := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream hit despite admission block: %s", r.URL.Path)
	}))
	defer anthUp.Close()

	adm := &fakeAdmitter{block: true, reason: "off-scope request"}
	p, err := New(Options{AnthropicUpstream: anthUp.URL, Sink: &fakeSink{}, Admitter: adm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"do the forbidden thing"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if !adm.called || adm.gotText != "do the forbidden thing" {
		t.Errorf("admitter saw text %q (called=%v)", adm.gotText, adm.called)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "off-scope request") {
		t.Errorf("refusal body missing reason: %s", body)
	}
}

// TestProxyAdmissionObserveForwards proves a non-blocking verdict (observe
// mode, or an allow) forwards to the upstream unchanged.
func TestProxyAdmissionObserveForwards(t *testing.T) {
	hit := false
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],` +
			`"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	defer anthUp.Close()

	adm := &fakeAdmitter{block: false}
	p, err := New(Options{AnthropicUpstream: anthUp.URL, Sink: &fakeSink{}, Admitter: adm})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"messages":[{"role":"user","content":"a fine question"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if !hit {
		t.Error("upstream was not forwarded to on a non-blocking verdict")
	}
	if !adm.called {
		t.Error("admitter was not consulted")
	}
}
