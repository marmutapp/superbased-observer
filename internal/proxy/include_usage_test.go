package proxy

import (
	"encoding/json"
	"testing"
)

func mustJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("re-injected body is not valid JSON: %v", err)
	}
	return m
}

func TestInjectOpenAIIncludeUsage_StreamingBodyWithoutOptions(t *testing.T) {
	// The aider shape: stream:true, no stream_options — the exact case that
	// landed api_turns 166805 with 0/0 tokens.
	body := []byte(`{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	out, ok := injectOpenAIIncludeUsage(body)
	if !ok {
		t.Fatal("injectOpenAIIncludeUsage = ok:false, want injection")
	}
	m := mustJSON(t, out)
	opts, ok := m["stream_options"].(map[string]any)
	if !ok || opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want {include_usage:true}", m["stream_options"])
	}
	// Unknown fields round-trip.
	if m["model"] != "gpt-4o-mini" {
		t.Fatalf("model lost in rewrite: %#v", m["model"])
	}
	if _, has := m["messages"]; !has {
		t.Fatal("messages lost in rewrite")
	}
}

func TestInjectOpenAIIncludeUsage_ExistingOptionsUntouched(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true,"custom":1}}`)
	out, ok := injectOpenAIIncludeUsage(body)
	if ok {
		t.Fatal("must not touch a body that already carries stream_options")
	}
	if out != nil {
		t.Fatal("out must be nil when no injection happened")
	}
}

func TestInjectOpenAIIncludeUsage_NonStreamingBody(t *testing.T) {
	body := []byte(`{"model":"m","stream":false,"messages":[]}`)
	if out, ok := injectOpenAIIncludeUsage(body); ok || out != nil {
		t.Fatal("non-streaming body must pass through untouched (usage is native)")
	}
}

func TestInjectOpenAIIncludeUsage_NonJSONBody(t *testing.T) {
	if out, ok := injectOpenAIIncludeUsage([]byte("not json")); ok || out != nil {
		t.Fatal("non-JSON body must fail open (forward original)")
	}
}

func TestParseOpenAIStream_AiderShapeWithInjectedUsage(t *testing.T) {
	// End-to-end of the fix: after injection the upstream emits the final
	// usage chunk; the existing parser must pick it up.
	stream := []byte(
		"data: {\"id\":\"c1\",\"model\":\"gpt-4o-mini\",\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"model\":\"gpt-4o-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":665,\"completion_tokens\":1}}\n\n" +
			"data: [DONE]\n\n",
	)
	res := parseOpenAIStream(stream)
	if res.InputTokens != 665 || res.OutputTokens != 1 {
		t.Fatalf("usage = %d/%d, want 665/1", res.InputTokens, res.OutputTokens)
	}
}
