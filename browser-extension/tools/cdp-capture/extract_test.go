package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestTruncStr(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 80, "short"},
		{"abcdef", 3, "abc …[+3 chars]"},
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := truncStr(c.in, c.n); got != c.want {
			t.Errorf("truncStr(%q,%d)=%q want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestDescribeBody(t *testing.T) {
	long := strings.Repeat("x", 200)
	body := fmt.Sprintf(`{"model":"gpt-5-6","messages":[{"role":"user","content":%q}]}`, long)
	structure, perr := describeBody(body)
	if perr != "" {
		t.Fatalf("unexpected parse error: %s", perr)
	}
	m, ok := structure.(map[string]interface{})
	if !ok {
		t.Fatalf("structure not a map: %T", structure)
	}
	if m["model"] != "gpt-5-6" {
		t.Errorf("model not preserved: %v", m["model"])
	}
	// The long content string must be replaced by a <str:N chars> marker.
	arr := m["messages"].(map[string]interface{})
	if arr["__array_len"].(int) != 1 {
		t.Errorf("array length wrong: %v", arr["__array_len"])
	}
	first := arr["__first_elem"].(map[string]interface{})
	content, _ := first["content"].(string)
	if !strings.HasPrefix(content, "<str:200 chars>") {
		t.Errorf("long string not truncated in structure: %q", content)
	}

	// Non-JSON body → marker + parse error.
	structure, perr = describeBody("f.req=%5B%5D&at=xyz")
	if perr == "" {
		t.Errorf("expected parse error for non-JSON body")
	}
	if s, _ := structure.(string); !strings.HasPrefix(s, "<non-JSON body") {
		t.Errorf("non-JSON marker missing: %v", structure)
	}
}

func TestExtractChatGPT(t *testing.T) {
	reqBody := `{"model":"gpt-5-6","conversation_id":"conv-123","messages":[` +
		`{"author":{"role":"user"},"content":{"content_type":"text","parts":["Hello world"]}}]}`
	// Response: bootstrap snapshot (assistant) then sticky deltas at
	// content/parts/0 with o+p omitted on later frames.
	resp := strings.Join([]string{
		`data: {"v":{"message":{"id":"msg-9","author":{"role":"assistant"},"content":{"parts":[""]},"metadata":{"model_slug":"gpt-5-6"}},"conversation_id":"conv-123"}}`,
		`data: {"p":"/message/content/parts/0","o":"append","v":"Hi"}`,
		`data: {"v":" there"}`,
		`data: [DONE]`,
	}, "\n")

	ex := Extract("chatgpt-web", "https://chatgpt.com/backend-api/conversation", reqBody, resp, nil)
	if ex.Model != "gpt-5-6" {
		t.Errorf("model=%q", ex.Model)
	}
	if !strings.Contains(ex.ModelPath, "model_slug") {
		t.Errorf("model path should prefer server echo: %q", ex.ModelPath)
	}
	if ex.PromptTextSample != "Hello world" {
		t.Errorf("prompt=%q", ex.PromptTextSample)
	}
	if !strings.Contains(ex.PromptTextPath, "content.parts") {
		t.Errorf("prompt path=%q", ex.PromptTextPath)
	}
	// Sticky delta " there" MUST be appended (the #1 silent-failure mode).
	if ex.ResponseTextSample != "Hi there" {
		t.Errorf("response=%q want %q", ex.ResponseTextSample, "Hi there")
	}
	if ex.ConversationID != "conv-123" {
		t.Errorf("conv id=%q", ex.ConversationID)
	}
	if ex.MessageID != "msg-9" {
		t.Errorf("msg id=%q", ex.MessageID)
	}
}

func TestExtractChatGPTSnapshotOnly(t *testing.T) {
	// Short response arriving as a snapshot only (no appends) → fall back.
	resp := `data: {"v":{"message":{"id":"m1","author":{"role":"assistant"},"content":{"parts":["Full answer"]}}}}` + "\n" + `data: [DONE]`
	ex := Extract("chatgpt-web", "https://chatgpt.com/backend-api/conversation", `{"model":"gpt-5-6"}`, resp, nil)
	if ex.ResponseTextSample != "Full answer" {
		t.Errorf("snapshot fallback failed: %q", ex.ResponseTextSample)
	}
	if !strings.Contains(ex.ResponseTextPath, "snapshot-only") {
		t.Errorf("path=%q", ex.ResponseTextPath)
	}
}

func TestExtractClaude(t *testing.T) {
	reqBody := `{"model":"claude-sonnet-4","prompt":"Ping"}`
	resp := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4-5","usage":{"input_tokens":12}}}`,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Po"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"ng"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":7}}`,
	}, "\n")
	url := "https://claude.ai/api/organizations/org-1/chat_conversations/conv-abc/completion"
	ex := Extract("claude-web", url, reqBody, resp, nil)
	if ex.Model != "claude-sonnet-4-5" {
		t.Errorf("model=%q (should be overridden by message_start)", ex.Model)
	}
	if ex.ResponseTextSample != "Pong" {
		t.Errorf("response=%q (thinking_delta must be excluded)", ex.ResponseTextSample)
	}
	if ex.ConversationID != "conv-abc" {
		t.Errorf("conv id from URL=%q", ex.ConversationID)
	}
	if ex.MessageID != "msg_01" {
		t.Errorf("msg id=%q", ex.MessageID)
	}
	// token sweep should find input_tokens + output_tokens.
	foundInput, foundOutput := false, false
	for _, tf := range ex.TokenFieldsFound {
		if strings.HasSuffix(tf.Path, "input_tokens") {
			foundInput = true
		}
		if strings.HasSuffix(tf.Path, "output_tokens") {
			foundOutput = true
		}
	}
	if !foundInput || !foundOutput {
		t.Errorf("token sweep missed usage fields: %+v", ex.TokenFieldsFound)
	}
}

func TestExtractPerplexity(t *testing.T) {
	// Triple-nested: data.text is a JSON string of steps[]; the FINAL step's
	// content is AGAIN a JSON string of {answer,...}.
	finalContent := `{"answer":"42 is the answer","chunks":[]}`
	steps := []map[string]interface{}{
		{"step_type": "SEARCH_RESULTS", "content": "irrelevant"},
		{"step_type": "FINAL", "content": finalContent},
	}
	stepsJSON, _ := json.Marshal(steps)
	frame := map[string]interface{}{
		"backend_uuid": "bk-1",
		"uuid":         "u-1",
		"text":         string(stepsJSON),
	}
	frameJSON, _ := json.Marshal(frame)
	reqBody := `{"query_str":"What is the answer","params":{"model_preference":"turbo"}}`
	resp := "event: message\ndata: " + string(frameJSON) + "\nevent: end_of_stream\ndata: {}"

	ex := Extract("perplexity-web", "https://www.perplexity.ai/rest/sse/perplexity_ask", reqBody, resp, nil)
	if ex.PromptTextSample != "What is the answer" {
		t.Errorf("prompt=%q", ex.PromptTextSample)
	}
	if ex.Model != "turbo" {
		t.Errorf("model=%q", ex.Model)
	}
	if ex.ResponseTextSample != "42 is the answer" {
		t.Errorf("triple-decode failed: %q", ex.ResponseTextSample)
	}
	if !strings.Contains(ex.ResponseTextPath, "triple decode") {
		t.Errorf("path=%q", ex.ResponseTextPath)
	}
	if ex.ConversationID != "bk-1" {
		t.Errorf("conv id=%q", ex.ConversationID)
	}
}

func TestExtractPerplexitySchematized(t *testing.T) {
	// LIVE-CONFIRMED 2026-07-10 (pplx_pro_upgraded, use_schematized_api=true):
	// FINAL.content arrives as an OBJECT and its .answer is a NESTED JSON string
	// wrapper `{"answer":"…"}`. The parser must peel the wrapper to the prose.
	innerAnswer := `{"answer":"Louis XVI was the last king","chunks":[]}`
	steps := []map[string]interface{}{
		{"step_type": "INITIAL_QUERY", "content": "irrelevant"},
		{"step_type": "FINAL", "content": map[string]interface{}{"answer": innerAnswer, "chunks": []interface{}{}}},
	}
	stepsJSON, _ := json.Marshal(steps)
	frame := map[string]interface{}{
		"backend_uuid": "bk-s",
		"uuid":         "u-s",
		"text":         string(stepsJSON),
	}
	frameJSON, _ := json.Marshal(frame)
	reqBody := `{"query_str":"Last king of france","params":{"model_preference":"pplx_pro_upgraded"}}`
	resp := "event: message\ndata: " + string(frameJSON) + "\nevent: end_of_stream\ndata: {}"

	ex := Extract("perplexity-web", "https://www.perplexity.ai/rest/sse/perplexity_ask", reqBody, resp, nil)
	if ex.Model != "pplx_pro_upgraded" {
		t.Errorf("model=%q", ex.Model)
	}
	if ex.ResponseTextSample != "Louis XVI was the last king" {
		t.Errorf("schematized unwrap failed: %q", ex.ResponseTextSample)
	}
	if !strings.Contains(ex.ResponseTextPath, "schematized") {
		t.Errorf("path=%q", ex.ResponseTextPath)
	}
}

// buildGeminiEnvelope constructs a )]}'-prefixed UTF-16-length-prefixed
// batchexecute envelope from a candidate text (to exercise the accumulator).
func buildGeminiEnvelope(text string) string {
	partJSON := []interface{}{
		nil,
		[]interface{}{"cid-1", "rid-1"},
		nil,
		nil,
		[]interface{}{ // candidates
			[]interface{}{"rcid-1", []interface{}{text}},
		},
	}
	pjBytes, _ := json.Marshal(partJSON)
	frame := []interface{}{
		[]interface{}{"wrb.fr", "abc", string(pjBytes)},
	}
	frameBytes, _ := json.Marshal(frame)
	frameStr := string(frameBytes)
	// length is UTF-16 code units.
	n := len(utf16.Encode([]rune(frameStr)))
	return ")]}'\n" + strconv.Itoa(n) + "\n" + frameStr + "\n"
}

func TestExtractGemini(t *testing.T) {
	envelope := buildGeminiEnvelope("The Gemini answer 🌟")
	headers := map[string]string{"x-goog-ext-525001261-jspb": "[\"hex-model-tag\"]"}
	ex := Extract("gemini-web", "https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate", "f.req=...", envelope, headers)
	if ex.ResponseTextSample != "The Gemini answer 🌟" {
		t.Errorf("gemini response=%q", ex.ResponseTextSample)
	}
	if ex.ConversationID != "cid-1" {
		t.Errorf("cid=%q", ex.ConversationID)
	}
	if ex.MessageID != "rid-1" {
		t.Errorf("rid=%q", ex.MessageID)
	}
	if !strings.Contains(ex.ModelPath, "x-goog-ext-525001261-jspb") {
		t.Errorf("model path=%q", ex.ModelPath)
	}
	if !strings.Contains(ex.ResponseTextPath, "candidate[1][0]") {
		t.Errorf("response path=%q", ex.ResponseTextPath)
	}
}

func TestGeminiFramesUTF16(t *testing.T) {
	// An emoji (surrogate pair) inside the frame must be sliced correctly by
	// UTF-16 code-unit length.
	env := buildGeminiEnvelope("ab🌟cd")
	frames := geminiFrames(env)
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	arr, ok := frames[0].([]interface{})
	if !ok || len(arr) != 1 {
		t.Fatalf("frame shape wrong: %v", frames[0])
	}
}

func TestStripXSSI(t *testing.T) {
	if got := stripXSSI(")]}'\n\n123"); got != "123" {
		t.Errorf("stripXSSI=%q", got)
	}
	if got := stripXSSI("no-guard"); got != "no-guard" {
		t.Errorf("stripXSSI passthrough=%q", got)
	}
}

func TestSiteMatching(t *testing.T) {
	cases := []struct {
		url  string
		site string
	}{
		{"https://chatgpt.com/c/abc", "chatgpt-web"},
		{"https://claude.ai/chat/xyz", "claude-web"},
		{"https://www.perplexity.ai/search", "perplexity-web"},
		{"https://gemini.google.com/app", "gemini-web"},
		{"https://example.com", ""},
	}
	for _, c := range cases {
		s := siteForURL(c.url)
		got := ""
		if s != nil {
			got = s.Site
		}
		if got != c.site {
			t.Errorf("siteForURL(%q)=%q want %q", c.url, got, c.site)
		}
	}
}

func TestMatchesEndpoint(t *testing.T) {
	cg := siteForURL("https://chatgpt.com/")
	if !cg.matchesEndpoint("https://chatgpt.com/backend-api/f/conversation") {
		t.Error("chatgpt f/conversation should match")
	}
	if !cg.matchesEndpoint("https://chatgpt.com/backend-api/conversation") {
		t.Error("chatgpt /backend-api/conversation should match")
	}
	if cg.matchesEndpoint("https://chatgpt.com/backend-api/me") {
		t.Error("chatgpt /me should not match")
	}
	// The /prepare + /init debounce siblings contain the completion path as a
	// substring but must be excluded.
	if cg.matchesEndpoint("https://chatgpt.com/backend-api/f/conversation/prepare") {
		t.Error("chatgpt /f/conversation/prepare must NOT match (debounce sibling)")
	}
	if cg.matchesEndpoint("https://chatgpt.com/backend-api/f/conversation/init") {
		t.Error("chatgpt /f/conversation/init must NOT match")
	}
	// Gemini: match StreamGenerate, reject activity batchexecute RPCs.
	gm := siteForURL("https://gemini.google.com/")
	if !gm.matchesEndpoint("https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?bl=boq") {
		t.Error("gemini StreamGenerate should match")
	}
	if gm.matchesEndpoint("https://gemini.google.com/_/BardChatUi/data/batchexecute?rpcids=ESY5D") {
		t.Error("gemini activity batchexecute (ESY5D) must NOT match")
	}
	cl := siteForURL("https://claude.ai/")
	if !cl.matchesEndpoint("https://claude.ai/api/organizations/o1/chat_conversations/c1/completion") {
		t.Error("claude completion should match")
	}
	// Claude requires BOTH markers — a bare chat_conversations GET must not match.
	if cl.matchesEndpoint("https://claude.ai/api/organizations/o1/chat_conversations/c1") {
		t.Error("claude bare conversation must not match")
	}
}

func TestSweepTokenFieldsNonSSE(t *testing.T) {
	body := `{"usage":{"total_tokens":40},"nested":{"prompt_tokens":10}}`
	tf := sweepTokenFields(body)
	if len(tf) < 2 {
		t.Errorf("expected >=2 token fields, got %+v", tf)
	}
}

// TestSweepTokenFieldsExcludesAuth pins the privacy fix: arbitrary "*_token"
// auth fields (conduit_token JWT, read_write_token) must NOT be swept, while
// usage/count fields still are.
func TestSweepTokenFieldsExcludesAuth(t *testing.T) {
	body := `{"conduit_token":"aa.bb.cc","read_write_token":"secret","access_token":"x",` +
		`"usage":{"total_tokens":5,"input_tokens":3,"output_tokens":2}}`
	tf := sweepTokenFields(body)
	for _, f := range tf {
		if strings.Contains(f.Path, "conduit_token") ||
			strings.Contains(f.Path, "read_write_token") ||
			strings.Contains(f.Path, "access_token") {
			t.Errorf("auth token leaked into sweep: %s", f.Path)
		}
	}
	foundTotal, foundInput := false, false
	for _, f := range tf {
		if strings.HasSuffix(f.Path, "total_tokens") {
			foundTotal = true
		}
		if strings.HasSuffix(f.Path, "input_tokens") {
			foundInput = true
		}
	}
	if !foundTotal || !foundInput {
		t.Errorf("usage count fields missing from sweep: %+v", tf)
	}
}
