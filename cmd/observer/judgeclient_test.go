//go:build !no_obs

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
