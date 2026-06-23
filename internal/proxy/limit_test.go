package proxy

import (
	"net/http"
	"testing"
	"time"
)

func TestParseLimitHeaders_AnthropicUnified(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-5h-utilization", "42")   // percent form
	h.Set("anthropic-ratelimit-unified-5h-reset", "6600")       // 110m from now (relative)
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.39") // fraction form
	h.Set("anthropic-ratelimit-unified-status", "allowed")

	snap, ok := parseLimitHeaders(h, "anthropic", now)
	if !ok {
		t.Fatal("expected a snapshot")
	}
	if snap.Window5hUtil == nil || *snap.Window5hUtil != 0.42 {
		t.Errorf("5h util = %v, want 0.42 (normalized from 42)", snap.Window5hUtil)
	}
	if snap.Window7dUtil == nil || *snap.Window7dUtil != 0.39 {
		t.Errorf("7d util = %v, want 0.39", snap.Window7dUtil)
	}
	if snap.Window5hReset == nil || *snap.Window5hReset != now.Add(6600*time.Second).Unix() {
		t.Errorf("5h reset = %v, want now+6600s", snap.Window5hReset)
	}
	if snap.Status != "allowed" {
		t.Errorf("status = %q", snap.Status)
	}
	if snap.Raw == "" {
		t.Error("raw should carry the allow-listed headers")
	}
}

func TestParseLimitHeaders_AnthropicClassicRFC3339(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "987")
	h.Set("anthropic-ratelimit-requests-reset", "2026-06-19T12:05:00Z")

	snap, ok := parseLimitHeaders(h, "anthropic", now)
	if !ok {
		t.Fatal("classic-only headers should still produce a snapshot")
	}
	if snap.Window5hUtil != nil {
		t.Error("no unified header → 5h util should be nil")
	}
	if snap.ReqRemaining == nil || *snap.ReqRemaining != 987 {
		t.Errorf("req remaining = %v, want 987", snap.ReqRemaining)
	}
	wantReset := time.Date(2026, 6, 19, 12, 5, 0, 0, time.UTC).Unix()
	if snap.ReqReset == nil || *snap.ReqReset != wantReset {
		t.Errorf("req reset = %v, want %d (RFC3339 parsed)", snap.ReqReset, wantReset)
	}
}

func TestParseLimitHeaders_OpenAIDuration(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("x-ratelimit-remaining-tokens", "120000")
	h.Set("x-ratelimit-reset-tokens", "6m0s") // Go duration form

	snap, ok := parseLimitHeaders(h, "openai", now)
	if !ok {
		t.Fatal("expected a snapshot")
	}
	if snap.TokRemaining == nil || *snap.TokRemaining != 120000 {
		t.Errorf("tok remaining = %v", snap.TokRemaining)
	}
	if snap.TokReset == nil || *snap.TokReset != now.Add(6*time.Minute).Unix() {
		t.Errorf("tok reset = %v, want now+6m", snap.TokReset)
	}
	// OpenAI carries no subscription window in v1.
	if snap.Window5hUtil != nil || snap.Window7dUtil != nil {
		t.Error("openai should expose no 5h/weekly window")
	}
}

func TestParseLimitHeaders_NoneReturnsFalse(t *testing.T) {
	snap, ok := parseLimitHeaders(http.Header{}, "anthropic", time.Now())
	if ok {
		t.Errorf("empty headers should return ok=false, got %+v", snap)
	}
}
