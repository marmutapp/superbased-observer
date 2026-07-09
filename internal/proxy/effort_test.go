package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRewriteEffortFields_Rows — one row per §R6.5 splice behavior:
// replace-only (never add), downshift-only (never raise), provider-
// aware field selection, byte preservation outside the span.
func TestRewriteEffortFields_Rows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		effort   string
		provider string
		changed  bool
		contains string
		excludes string
	}{
		{
			name:     "anthropic_thinking_lowered",
			body:     `{"model":"claude-opus-4-8","thinking":{"type":"enabled","budget_tokens":32000},"messages":[]}`,
			effort:   "low",
			provider: "anthropic",
			changed:  true,
			contains: `"thinking":{"type":"enabled","budget_tokens":4096}`,
		},
		{
			name:     "anthropic_minimal_disables",
			body:     `{"thinking":{"type":"enabled","budget_tokens":32000},"messages":[]}`,
			effort:   "minimal",
			provider: "anthropic",
			changed:  true,
			contains: `"thinking":{"type":"disabled"}`,
		},
		{
			name:     "anthropic_absent_thinking_never_added",
			body:     `{"model":"claude-opus-4-8","messages":[]}`,
			effort:   "low",
			provider: "anthropic",
			changed:  false,
		},
		{
			name:     "anthropic_already_below_target_untouched",
			body:     `{"thinking":{"type":"enabled","budget_tokens":2048}}`,
			effort:   "low",
			provider: "anthropic",
			changed:  false,
		},
		{
			name:     "anthropic_already_disabled_untouched",
			body:     `{"thinking":{"type":"disabled"}}`,
			effort:   "minimal",
			provider: "anthropic",
			changed:  false,
		},
		{
			name:     "anthropic_high_is_noop",
			body:     `{"thinking":{"type":"enabled","budget_tokens":32000}}`,
			effort:   "high",
			provider: "anthropic",
			changed:  false,
		},
		{
			name:     "openai_reasoning_effort_lowered",
			body:     `{"model":"gpt-5.4","reasoning_effort":"high","messages":[]}`,
			effort:   "low",
			provider: "openai",
			changed:  true,
			contains: `"reasoning_effort":"low"`,
			excludes: `"high"`,
		},
		{
			name:     "openai_reasoning_effort_already_lower",
			body:     `{"reasoning_effort":"minimal"}`,
			effort:   "low",
			provider: "openai",
			changed:  false,
		},
		{
			name:     "openai_reasoning_object_lowered",
			body:     `{"model":"gpt-5.4","reasoning":{"effort":"high"},"input":[]}`,
			effort:   "medium",
			provider: "openai",
			changed:  true,
			contains: `"effort":"medium"`,
		},
		{
			name:     "openai_reasoning_object_without_effort_never_added",
			body:     `{"reasoning":{"summary":"auto"}}`,
			effort:   "low",
			provider: "openai",
			changed:  false,
		},
		{
			name:     "openai_absent_fields_never_added",
			body:     `{"model":"gpt-4o","messages":[]}`,
			effort:   "low",
			provider: "openai",
			changed:  false,
		},
		{
			name:     "unknown_provider_noop",
			body:     `{"thinking":{"type":"enabled","budget_tokens":32000}}`,
			effort:   "low",
			provider: "google",
			changed:  false,
		},
		{
			name:     "unknown_effort_noop",
			body:     `{"thinking":{"type":"enabled","budget_tokens":32000}}`,
			effort:   "turbo",
			provider: "anthropic",
			changed:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, changed := rewriteEffortFields([]byte(tc.body), tc.effort, tc.provider)
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v (out=%q)", changed, tc.changed, out)
			}
			if !changed {
				if string(out) != tc.body {
					t.Errorf("unchanged body mutated: %q", out)
				}
				return
			}
			if tc.contains != "" && !strings.Contains(string(out), tc.contains) {
				t.Errorf("out = %q, want contains %q", out, tc.contains)
			}
			if tc.excludes != "" && strings.Contains(string(out), tc.excludes) {
				t.Errorf("out = %q, must not contain %q", out, tc.excludes)
			}
		})
	}
}

// TestRouter_EffortRoundTrip pins the §R6.5 round-trip: an effort-only
// enforce verdict lowers the thinking budget in the forwarded body,
// the model is untouched, and every byte outside the thinking span is
// preserved.
func TestRouter_EffortRoundTrip(t *testing.T) {
	const effortReqBody = `{"model":"claude-opus-4-8","thinking":{"type":"enabled","budget_tokens":32000},` +
		`"metadata":{"user_id":"{\"session_id\":\"sess-effort\"}"},"messages":[{"role":"user","content":"hi"}]}`

	var seenBody string
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(routerRespBody))
	}))
	defer anthUp.Close()
	fr := &fakeRouter{verdict: RouterVerdict{Apply: true, SetEffort: "low", Token: 5}}
	sink := &fakeSink{}
	p, err := New(Options{AnthropicUpstream: anthUp.URL, OpenAIUpstream: anthUp.URL, Sink: sink, ModelRouter: fr})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	ts := httptest.NewServer(p.Handler())
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(effortReqBody))
	req.Header.Set("X-Api-Key", "sk-ant-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if !strings.Contains(seenBody, `"thinking":{"type":"enabled","budget_tokens":4096}`) {
		t.Errorf("thinking not downshifted: %q", seenBody)
	}
	if !strings.Contains(seenBody, `"model":"claude-opus-4-8"`) {
		t.Errorf("model mutated on an effort-only decision: %q", seenBody)
	}
	if !strings.Contains(seenBody, `"metadata":{"user_id":"{\"session_id\":\"sess-effort\"}"}`) {
		t.Errorf("metadata not preserved: %q", seenBody)
	}
}
