package otel

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func attrMap(kvs []attribute.KeyValue) map[string]attribute.Value {
	m := make(map[string]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value
	}
	return m
}

func TestSpanName(t *testing.T) {
	if got := SpanName(models.APITurn{Model: "claude-opus-4-7"}); got != "chat claude-opus-4-7" {
		t.Errorf("SpanName = %q", got)
	}
	if got := SpanName(models.APITurn{}); got != "chat" {
		t.Errorf("SpanName(no model) = %q, want %q", got, "chat")
	}
}

func TestAttributes_TurnOnly(t *testing.T) {
	turn := models.APITurn{
		Provider: "anthropic", Model: "claude-opus-4-7", RequestID: "req_123",
		InputTokens: 100, OutputTokens: 50, CacheReadTokens: 40, WebSearchRequests: 2,
		CostUSD: 0.0123, StopReason: "end_turn", SessionID: "sess-1",
	}
	m := attrMap(Attributes(turn, nil, "/home/me/proj", false))

	wantStr := map[string]string{
		keyGenAIProviderName:  "anthropic",
		keyGenAIOperationName: operationChat,
		keyGenAIRequestModel:  "claude-opus-4-7",
		keyGenAIResponseModel: "claude-opus-4-7",
		keyGenAIResponseID:    "req_123",
		keySBOProjectRoot:     "/home/me/proj",
		keySBOSessionID:       "sess-1",
	}
	for k, want := range wantStr {
		if got := m[k].AsString(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if m[keyGenAIUsageInputTok].AsInt64() != 100 || m[keyGenAIUsageOutputTok].AsInt64() != 50 {
		t.Errorf("usage tokens wrong: in=%d out=%d", m[keyGenAIUsageInputTok].AsInt64(), m[keyGenAIUsageOutputTok].AsInt64())
	}
	if m[keySBOCacheReadTokens].AsInt64() != 40 {
		t.Errorf("cache read = %d, want 40", m[keySBOCacheReadTokens].AsInt64())
	}
	if m[keySBOWebSearchRequests].AsInt64() != 2 {
		t.Errorf("web search = %d, want 2", m[keySBOWebSearchRequests].AsInt64())
	}
	if got := m[keySBOCostUSD].AsFloat64(); got != 0.0123 {
		t.Errorf("cost = %v, want 0.0123", got)
	}
	if fr := m[keyGenAIFinishReasons].AsStringSlice(); len(fr) != 1 || fr[0] != "end_turn" {
		t.Errorf("finish_reasons = %v, want [end_turn]", fr)
	}
	// Not enrolled → no org/user, no action attrs.
	for _, absent := range []string{keySBOOrgID, keySBOUserEmail, keySBOToolAdapter, keySBOActionNormalized, keySBOFreshnessClass, errorTypeKey} {
		if _, ok := m[absent]; ok {
			t.Errorf("attribute %s should be absent for a solo-local turn-only span", absent)
		}
	}
}

func TestAttributes_UserEmailGating(t *testing.T) {
	turn := models.APITurn{Provider: "openai", Model: "gpt-x", OrgID: "org-1", UserEmail: "dev@acme.com"}

	// Enrolled, emit OFF: org id present, email absent.
	off := attrMap(Attributes(turn, nil, "", false))
	if off[keySBOOrgID].AsString() != "org-1" {
		t.Errorf("org id should be present when enrolled")
	}
	if _, ok := off[keySBOUserEmail]; ok {
		t.Error("user email must be absent when emitUserEmail=false")
	}

	// Enrolled, emit ON: email present.
	on := attrMap(Attributes(turn, nil, "", true))
	if on[keySBOUserEmail].AsString() != "dev@acme.com" {
		t.Errorf("user email should be present when enrolled and emitUserEmail=true")
	}

	// Not enrolled but emit ON: still no email (no org).
	solo := models.APITurn{Provider: "openai", Model: "gpt-x", UserEmail: "dev@acme.com"}
	s := attrMap(Attributes(solo, nil, "", true))
	if _, ok := s[keySBOUserEmail]; ok {
		t.Error("user email must be absent when not enrolled, even with emitUserEmail=true")
	}
	if _, ok := s[keySBOOrgID]; ok {
		t.Error("org id must be absent when not enrolled")
	}
}

func TestAttributes_ActionLevel(t *testing.T) {
	turn := models.APITurn{Provider: "anthropic", Model: "m"}
	cases := []struct {
		name      string
		freshness string
		wantStale bool
	}{
		{"stale reread", models.FreshnessStale, true},
		{"fresh read", models.FreshnessFresh, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			action := &models.Action{Tool: "claude-code", ActionType: "read_file", Freshness: c.freshness}
			m := attrMap(Attributes(turn, action, "", false))
			if m[keySBOToolAdapter].AsString() != "claude-code" {
				t.Errorf("tool adapter = %q", m[keySBOToolAdapter].AsString())
			}
			if m[keySBOActionNormalized].AsString() != "read_file" {
				t.Errorf("normalized type = %q", m[keySBOActionNormalized].AsString())
			}
			if m[keySBOFreshnessClass].AsString() != c.freshness {
				t.Errorf("freshness = %q, want %q", m[keySBOFreshnessClass].AsString(), c.freshness)
			}
			if m[keySBORedundancyStale].AsBool() != c.wantStale {
				t.Errorf("is_stale_reread = %v, want %v", m[keySBORedundancyStale].AsBool(), c.wantStale)
			}
		})
	}
}

func TestAttributes_ErrorTurn(t *testing.T) {
	turn := models.APITurn{Provider: "anthropic", HTTPStatus: 429, ErrorClass: "rate_limit_error"}
	m := attrMap(Attributes(turn, nil, "", false))
	if m[errorTypeKey].AsString() != "rate_limit_error" {
		t.Errorf("error.type = %q, want rate_limit_error", m[errorTypeKey].AsString())
	}
}
