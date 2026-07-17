//go:build !no_obs

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// TestJudgeEgressScrubsSecretBeforeSend is the admission spec §5/§12 boundary
// guarantee: a credential-bearing prompt sent to a REMOTE judge leaves the
// node redacted, and the request body stays valid JSON (the ScrubForward
// regression class).
func TestJudgeEgressScrubsSecretBeforeSend(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	// A canonical AWS access-key id (built by concat so the literal is inert).
	leaked := "AKIA" + "IOSFODNN7EXAMPLE"
	prompt := "Evaluate this request. The user pasted a key: " + leaked

	judge := newChatCompletionsJudge(srv.URL, "test-cred").
		withEgressScrub(scrub.New().String, judgeMaxPromptBytes)
	if _, err := judge.complete(context.Background(), "test-model", prompt); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if strings.Contains(string(gotBody), leaked) {
		t.Errorf("raw credential egressed to the judge: %s", gotBody)
	}
	if !strings.Contains(string(gotBody), scrub.Redacted) {
		t.Errorf("scrubbed body missing redaction marker: %s", gotBody)
	}
	if !json.Valid(gotBody) {
		t.Errorf("egress body is not valid JSON: %s", gotBody)
	}
}

// TestSecureJudgeEgressGating pins that the egress scrub is applied for a
// remote judge and skipped for a loopback/off one (no overhead, no risk).
func TestSecureJudgeEgressGating(t *testing.T) {
	base := newChatCompletionsJudge("https://openrouter.ai/api/v1", "cred")
	for _, tc := range []struct {
		hosting  string
		wantScr  bool
		wantCapd bool
	}{
		{"local", false, false},
		{"off", false, false},
		{"provider", true, true},
		{"aggregator", true, true},
		{"private", true, true},
	} {
		got := secureJudgeEgress(base, tc.hosting)
		if (got.scrubPrompt != nil) != tc.wantScr {
			t.Errorf("hosting %q: scrubPrompt set = %v, want %v", tc.hosting, got.scrubPrompt != nil, tc.wantScr)
		}
		if (got.maxPromptBytes > 0) != tc.wantCapd {
			t.Errorf("hosting %q: capped = %v, want %v", tc.hosting, got.maxPromptBytes > 0, tc.wantCapd)
		}
	}
}

// decodeJudgeBody unmarshals a captured request body into a generic map.
func decodeJudgeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("request body not JSON: %v (%s)", err, raw)
	}
	return m
}

// TestJudgeSendsMaxTokens pins that the default reply cap is present in the
// request and that withTuning overrides it (gap-audit §5.3 max_tokens).
func TestJudgeSendsMaxTokens(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	// Default: newChatCompletionsJudge bakes in defaultJudgeMaxTokens.
	j := newChatCompletionsJudge(srv.URL, "cred")
	if _, err := j.complete(context.Background(), "m", "hi"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := decodeJudgeBody(t, gotBody)["max_tokens"]; got != float64(defaultJudgeMaxTokens) {
		t.Errorf("default max_tokens = %v, want %d", got, defaultJudgeMaxTokens)
	}

	// Override via withTuning.
	j2 := newChatCompletionsJudge(srv.URL, "cred").withTuning(64, 0)
	if _, err := j2.complete(context.Background(), "m", "hi"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := decodeJudgeBody(t, gotBody)["max_tokens"]; got != float64(64) {
		t.Errorf("tuned max_tokens = %v, want 64", got)
	}
}

// TestJudgeAllowsEmptyKeyForLocal is gap-audit §5.3: a loopback (Ollama-style)
// judge needs no credential, and no Authorization header is sent when the key
// is empty. httptest serves on 127.0.0.1, so the judge is local.
func TestJudgeAllowsEmptyKeyForLocal(t *testing.T) {
	var hadAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hadAuth = r.Header.Get("Authorization") != ""
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	j := newChatCompletionsJudge(srv.URL, "") // empty credential
	if !j.local {
		t.Fatalf("loopback URL %q not classified local", srv.URL)
	}
	out, err := j.complete(context.Background(), "m", "hi")
	if err != nil {
		t.Fatalf("local keyless judge errored: %v", err)
	}
	if out != "ok" {
		t.Errorf("content = %q, want ok", out)
	}
	if hadAuth {
		t.Error("Authorization header sent for a keyless local judge")
	}
}

// TestJudgeRemoteStillRequiresKey pins that a non-loopback judge with no
// credential still errors before any network call.
func TestJudgeRemoteStillRequiresKey(t *testing.T) {
	j := newChatCompletionsJudge("https://openrouter.ai/api/v1", "")
	if _, err := j.complete(context.Background(), "m", "hi"); err == nil {
		t.Fatal("remote keyless judge should error on the empty credential")
	}
}

// TestJudgeNumCtxGating pins that num_ctx is passed through ONLY for a local
// judge (gap-audit §5.3 optional passthrough) — a remote judge never receives
// the Ollama-specific field.
func TestJudgeNumCtxGating(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	// Local (loopback httptest) + num_ctx set → present.
	j := newChatCompletionsJudge(srv.URL, "").withTuning(0, 4096)
	if _, err := j.complete(context.Background(), "m", "hi"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if got := decodeJudgeBody(t, gotBody)["num_ctx"]; got != float64(4096) {
		t.Errorf("local num_ctx = %v, want 4096", got)
	}

	// Same server but forced non-local → num_ctx omitted (never sent remotely).
	j2 := newChatCompletionsJudge(srv.URL, "cred").withTuning(0, 4096)
	j2.local = false
	if _, err := j2.complete(context.Background(), "m", "hi"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, present := decodeJudgeBody(t, gotBody)["num_ctx"]; present {
		t.Error("num_ctx sent to a non-local judge")
	}
}

// TestJudgeRetriesOn5xx is gap-audit §5.3: a transient 5xx is retried once and
// then succeeds.
func TestJudgeRetriesOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream blip"}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	j := newChatCompletionsJudge(srv.URL, "cred")
	out, err := j.complete(context.Background(), "m", "hi")
	if err != nil {
		t.Fatalf("complete after retry: %v", err)
	}
	if out != "ok" {
		t.Errorf("content = %q, want ok", out)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("server saw %d calls, want 2 (one retry)", n)
	}
}

// TestJudgeDoesNotRetry4xx pins that a 4xx is returned immediately — a bad
// request will not fix itself on retry.
func TestJudgeDoesNotRetry4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad model"}`))
	}))
	defer srv.Close()

	j := newChatCompletionsJudge(srv.URL, "cred")
	if _, err := j.complete(context.Background(), "m", "hi"); err == nil {
		t.Fatal("expected an error on 4xx")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("server saw %d calls, want 1 (no retry on 4xx)", n)
	}
}

// TestIsLoopbackJudgeURL pins the local-hosting predicate.
func TestIsLoopbackJudgeURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:11434/v1", true},
		{"http://localhost:11434/v1", true},
		{"http://0.0.0.0:8080/v1", true},
		{"http://[::1]:11434/v1", true},
		{"https://openrouter.ai/api/v1", false},
		{"https://api.openai.com/v1", false},
		{"", false},
	} {
		if got := isLoopbackJudgeURL(tc.url); got != tc.want {
			t.Errorf("isLoopbackJudgeURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
