// Package main — sbo-capture pure extraction core.
//
// These helpers are a Go port of browser-extension/src/parsers.js. They exist
// so the CDP dump surfaces the SAME field paths the extension's parsers pull,
// making a diff between a live capture and parsers.js direct. Everything in
// this file is pure (no I/O, no CDP) and unit-tested in extract_test.go with
// synthetic payloads — no live Chrome needed.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ---- tunables (mirror capture-harness.js) --------------------------------

const (
	maxVal        = 80  // truncate string VALUES in the structure dump
	maxFrames     = 12  // number of streamed frame samples kept
	maxFrameChars = 240 // truncate each frame sample
)

// truncStr shortens s to n chars with a "+N chars" marker.
func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf(" …[+%d chars]", len(s)-n)
}

// describe walks a decoded JSON value and returns a STRUCTURE-only clone:
// string VALUES longer than maxVal become a "<str:N chars> sample=…" marker,
// arrays are summarized by length + first/last element shape. This is the
// request_body_key_structure surface — structure, never full content.
func describe(v interface{}, depth int) interface{} {
	if depth > 8 {
		return "<max-depth>"
	}
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if len(x) <= maxVal {
			return x
		}
		return fmt.Sprintf("<str:%d chars> sample=%q …[truncated, redact before sharing]", len(x), x[:maxVal])
	case bool:
		return x
	case float64:
		return x
	case json.Number:
		return x.String()
	case []interface{}:
		o := map[string]interface{}{"__array_len": len(x)}
		if len(x) > 0 {
			o["__first_elem"] = describe(x[0], depth+1)
		}
		if len(x) > 1 {
			o["__last_elem"] = describe(x[len(x)-1], depth+1)
		}
		return o
	case map[string]interface{}:
		o := map[string]interface{}{}
		for k, val := range x {
			o[k] = describe(val, depth+1)
		}
		return o
	}
	return fmt.Sprintf("<%T>", v)
}

// describeBody parses a request body and returns its structure clone plus a
// parse error string (empty on success). Non-JSON bodies get a length marker.
func describeBody(body string) (interface{}, string) {
	if strings.TrimSpace(body) == "" {
		return nil, ""
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		sample := body
		if len(sample) > maxVal {
			sample = sample[:maxVal]
		}
		marker := fmt.Sprintf("<non-JSON body, len=%d> sample=%q", len(body), sample)
		return marker, err.Error()
	}
	return describe(v, 0), ""
}

// ---- extraction result ----------------------------------------------------

// TokenField is a token/usage field found anywhere in the parsed stream, with
// the JSON path it came from (the path is the point — it tunes the parser).
type TokenField struct {
	Path  string      `json:"path"`
	Value interface{} `json:"value"`
}

// Extraction is the best-effort {field, path} extraction that lines up
// field-for-field with the Go CapturedTurn struct + the parser it validates.
type Extraction struct {
	ConversationID     string       `json:"conversation_id"`
	ConversationIDPath string       `json:"conversation_id_path"`
	MessageID          string       `json:"message_id"`
	MessageIDPath      string       `json:"message_id_path"`
	Model              string       `json:"model"`
	ModelPath          string       `json:"model_path"`
	PromptTextSample   string       `json:"prompt_text_sample"`
	PromptTextPath     string       `json:"prompt_text_path"`
	ResponseTextSample string       `json:"response_text_sample"`
	ResponseTextPath   string       `json:"response_text_path"`
	TokenFieldsFound   []TokenField `json:"token_fields_found"`
	Note               string       `json:"note,omitempty"`
	ExtractError       string       `json:"extract_error,omitempty"`
}

// Extract dispatches to the per-site extractor. site is the "*-web" tag.
// reqBody is the raw request postData; respBody is the accumulated response
// body; reqHeaders are the (lower-cased-key) request headers; reqURL is the
// completion URL. Fail-soft: any panic/parse error is recorded, never fatal.
func Extract(site, reqURL, reqBody, respBody string, reqHeaders map[string]string) Extraction {
	ex := Extraction{}
	switch site {
	case "chatgpt-web":
		extractChatGPT(reqBody, respBody, &ex)
	case "claude-web":
		extractClaude(reqURL, reqBody, respBody, &ex)
	case "perplexity-web":
		extractPerplexity(reqBody, respBody, &ex)
	case "gemini-web":
		extractGemini(reqBody, respBody, reqHeaders, &ex)
	case "copilot-web":
		ex.Note = "Copilot uses a WebSocket transport — see ws_frames on this record, not an SSE/RPC body."
	}
	ex.TokenFieldsFound = sweepTokenFields(respBody)
	return ex
}

// ---- helpers to walk decoded JSON -----------------------------------------

func jsonDecode(s string) (interface{}, bool) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

func asObj(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func asArr(v interface{}) []interface{} {
	a, _ := v.([]interface{})
	return a
}

func asStr(v interface{}) string {
	s, _ := v.(string)
	return s
}

// joinParts concatenates the string members of a parts array (ChatGPT).
func joinParts(parts []interface{}) string {
	var b strings.Builder
	for _, p := range parts {
		if s, ok := p.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// sseLines yields the trimmed non-empty lines of an SSE body.
func sseLines(body string) []string {
	raw := strings.Split(body, "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		t := strings.TrimRight(l, "\r")
		out = append(out, t)
	}
	return out
}

// dataPayload returns the JSON payload of a `data:` SSE line, or "" if the
// line is not a data line or is a terminator.
func dataPayload(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "data:") {
		return "", false
	}
	p := strings.TrimSpace(t[len("data:"):])
	if p == "" || p == "[DONE]" {
		return "", false
	}
	return p, true
}

// ---- ChatGPT ---------------------------------------------------------------

// extractChatGPT ports parseChatGPTRequest + makeChatGPTAccumulator: request
// model at body.model / prompt at last author.role==user parts; response text
// = every `v` string appended at a path containing content/parts/0 WITH sticky
// o/p inheritance across frames (the #1 silent-failure mode).
func extractChatGPT(reqBody, respBody string, ex *Extraction) {
	if j, ok := jsonDecode(reqBody); ok {
		o := asObj(j)
		if m := asStr(o["model"]); m != "" {
			ex.Model, ex.ModelPath = m, "request.model"
		}
		if c := asStr(o["conversation_id"]); c != "" {
			ex.ConversationID, ex.ConversationIDPath = c, "request.conversation_id"
		}
		msgs := asArr(o["messages"])
		for i := len(msgs) - 1; i >= 0; i-- {
			mm := asObj(msgs[i])
			if asStr(asObj(mm["author"])["role"]) == "user" {
				parts := asArr(asObj(mm["content"])["parts"])
				ex.PromptTextSample = truncStr(joinParts(parts), maxVal)
				ex.PromptTextPath = fmt.Sprintf("request.messages[%d].content.parts[]  (last author.role=user)", i)
				break
			}
		}
	}

	// Response accumulator with sticky o/p inheritance.
	var text, snapshotText string
	var appendedAny bool
	curPath := ""
	var apply func(path string, value interface{})
	apply = func(path string, value interface{}) {
		switch vv := value.(type) {
		case []interface{}: // patch batch: array of sub-ops each with own o/p/v
			for _, sub := range vv {
				so := asObj(sub)
				if so == nil {
					continue
				}
				sp := path
				if p, ok := so["p"].(string); ok {
					sp = p
				}
				apply(sp, so["v"])
			}
		case map[string]interface{}:
			if msg := asObj(vv["message"]); msg != nil {
				if c := asStr(vv["conversation_id"]); c != "" {
					ex.ConversationID, ex.ConversationIDPath = c, "data.v.conversation_id  (snapshot)"
				}
				if asStr(asObj(msg["author"])["role"]) == "assistant" {
					if id := asStr(msg["id"]); id != "" {
						ex.MessageID, ex.MessageIDPath = id, "data.v.message.id  (role=assistant snapshot)"
					}
					if slug := asStr(asObj(msg["metadata"])["model_slug"]); slug != "" {
						ex.Model, ex.ModelPath = slug, "data.v.message.metadata.model_slug  (server echo)"
					}
					joined := joinParts(asArr(asObj(msg["content"])["parts"]))
					if len(joined) > len(snapshotText) {
						snapshotText = joined
					}
				}
			}
		case string:
			if strings.Contains(path, "content/parts/0") {
				text += vv
				appendedAny = true
				if ex.ResponseTextPath == "" {
					ex.ResponseTextPath = "data.v string appended at path …content/parts/0 (sticky-inherited o/p)"
				}
			}
		}
	}

	for _, line := range sseLines(respBody) {
		payload, ok := dataPayload(line)
		if !ok {
			continue
		}
		ev, ok := jsonDecode(payload)
		if !ok {
			continue
		}
		o := asObj(ev)
		if c := asStr(o["conversation_id"]); c != "" && ex.ConversationID == "" {
			ex.ConversationID, ex.ConversationIDPath = c, "data.conversation_id"
		}
		if p, ok := o["p"].(string); ok {
			curPath = p
		}
		if v, present := o["v"]; present {
			apply(curPath, v)
		}
	}
	if !appendedAny && len(snapshotText) > len(text) {
		text = snapshotText
		ex.ResponseTextPath = "data.v.message.content.parts[]  (snapshot-only, no appends)"
	}
	ex.ResponseTextSample = truncStr(text, maxVal)
}

// ---- Claude ---------------------------------------------------------------

// extractClaude ports parseClaudeRequest + makeClaudeAccumulator: conv id from
// the URL path, model+usage from message_start, response text from
// content_block_delta.text_delta.text (thinking_delta excluded).
func extractClaude(reqURL, reqBody, respBody string, ex *Extraction) {
	if j, ok := jsonDecode(reqBody); ok {
		o := asObj(j)
		if m := asStr(o["model"]); m != "" {
			ex.Model, ex.ModelPath = m, "request.model"
		}
		if p := asStr(o["prompt"]); p != "" {
			ex.PromptTextSample, ex.PromptTextPath = truncStr(p, maxVal), "request.prompt"
		} else if msgs := asArr(o["messages"]); len(msgs) > 0 {
			ex.PromptTextPath = "request.messages[last].content[]"
		}
	}
	// conversation id from URL /chat_conversations/{id}/completion
	if i := strings.Index(reqURL, "/chat_conversations/"); i != -1 {
		rest := reqURL[i+len("/chat_conversations/"):]
		if j := strings.IndexByte(rest, '/'); j != -1 {
			ex.ConversationID = rest[:j]
			ex.ConversationIDPath = "request_url path /chat_conversations/{id}/"
		}
	}

	var text string
	for _, line := range sseLines(respBody) {
		payload, ok := dataPayload(line)
		if !ok {
			continue
		}
		ev, ok := jsonDecode(payload)
		if !ok {
			continue
		}
		o := asObj(ev)
		switch asStr(o["type"]) {
		case "content_block_delta":
			d := asObj(o["delta"])
			if asStr(d["type"]) == "text_delta" {
				if s := asStr(d["text"]); s != "" {
					text += s
					ex.ResponseTextPath = "data.delta.text  (type=content_block_delta, delta.type=text_delta)"
				}
			}
		case "message_start":
			msg := asObj(o["message"])
			if id := asStr(msg["id"]); id != "" {
				ex.MessageID, ex.MessageIDPath = id, "data.message.id  (message_start)"
			}
			if m := asStr(msg["model"]); m != "" {
				ex.Model, ex.ModelPath = m, "data.message.model  (message_start)"
			}
		default:
			if c := asStr(o["completion"]); c != "" {
				text += c
				if ex.ResponseTextPath == "" {
					ex.ResponseTextPath = "data.completion  (legacy)"
				}
			}
		}
	}
	ex.ResponseTextSample = truncStr(text, maxVal)
}

// ---- Perplexity -----------------------------------------------------------

// extractPerplexity ports parsePerplexityRequest + makePerplexityAccumulator:
// prompt at query_str, model at params.model_preference; response text is the
// TRIPLE-nested decode data.text → steps[] → FINAL.content → .answer.
func extractPerplexity(reqBody, respBody string, ex *Extraction) {
	if j, ok := jsonDecode(reqBody); ok {
		o := asObj(j)
		if q := asStr(o["query_str"]); q != "" {
			ex.PromptTextSample, ex.PromptTextPath = truncStr(q, maxVal), "request.query_str"
		} else if q := asStr(o["q"]); q != "" {
			ex.PromptTextSample, ex.PromptTextPath = truncStr(q, maxVal), "request.q"
		}
		params := asObj(o["params"])
		if m := asStr(params["model_preference"]); m != "" {
			ex.Model, ex.ModelPath = m, "request.params.model_preference"
		} else if m := asStr(params["model"]); m != "" {
			ex.Model, ex.ModelPath = m, "request.params.model"
		}
		if c := asStr(o["frontend_context_uuid"]); c != "" {
			ex.ConversationID, ex.ConversationIDPath = c, "request.frontend_context_uuid"
		}
	}

	best := ""
	for _, line := range sseLines(respBody) {
		payload, ok := dataPayload(line)
		if !ok {
			continue
		}
		ev, ok := jsonDecode(payload)
		if !ok {
			continue
		}
		o := asObj(ev)
		if c := asStr(o["backend_uuid"]); c != "" {
			ex.ConversationID, ex.ConversationIDPath = c, "data.backend_uuid"
		}
		if u := asStr(o["uuid"]); u != "" && ex.MessageID == "" {
			ex.MessageID, ex.MessageIDPath = u, "data.uuid"
		}
		if ans, path := perplexityFinalAnswer(o); ans != "" && len(ans) > len(best) {
			best = ans
			ex.ResponseTextPath = path
		}
	}
	ex.ResponseTextSample = truncStr(best, maxVal)
	ex.Note = "Perplexity answer is TRIPLE-nested (text→steps[]→FINAL.content→answer); verify against the frame samples."
}

// perplexityFinalAnswer performs the triple-nested decode, returning the
// answer + the path it was pulled from.
func perplexityFinalAnswer(o map[string]interface{}) (string, string) {
	textStr := asStr(o["text"])
	if textStr != "" {
		if steps, ok := jsonDecode(textStr); ok {
			for _, st := range asArr(steps) {
				so := asObj(st)
				if asStr(so["step_type"]) != "FINAL" {
					continue
				}
				switch content := so["content"].(type) {
				case string:
					if fin, ok := jsonDecode(content); ok {
						if a := asStr(asObj(fin)["answer"]); a != "" {
							return unwrapSchematizedAnswer(a), "data.text → steps[] → FINAL.content → .answer  (triple decode)"
						}
					}
				case map[string]interface{}:
					if a := asStr(content["answer"]); a != "" {
						return unwrapSchematizedAnswer(a), "data.text → steps[] → FINAL.content.answer  (object, schematized double-wrap)"
					}
				}
			}
		}
	}
	if a := asStr(o["answer"]); a != "" {
		return unwrapSchematizedAnswer(a), "data.answer  (fallback)"
	}
	return "", ""
}

// unwrapSchematizedAnswer peels Perplexity's schematized-API double wrap.
// LIVE-CONFIRMED 2026-07-10: with params.use_schematized_api=true (default on
// the pplx_pro_upgraded build) FINAL.content.answer is ITSELF a JSON string
// `{"answer":"…"}` rather than plain prose, so a single .answer read returns the
// opaque wrapper. Peel any {answer:string} wrapper (bounded depth; plain-text
// answers never decode to an object with a string .answer, so legacy shapes are
// untouched).
func unwrapSchematizedAnswer(s string) string {
	cur := s
	for i := 0; i < 4; i++ {
		t := strings.TrimSpace(cur)
		if len(t) < 2 || t[0] != '{' {
			break
		}
		v, ok := jsonDecode(t)
		if !ok {
			break
		}
		inner := asStr(asObj(v)["answer"])
		if inner == "" {
			break
		}
		cur = inner
	}
	return cur
}

// ---- Gemini ---------------------------------------------------------------

// extractGemini ports makeGeminiAccumulator: strip )]}' XSSI guard, walk
// UTF-16-length-prefixed frames, decode wrb.fr entry[2], read candidate text.
// The model rides the request header x-goog-ext-525001261-jspb (opaque).
func extractGemini(reqBody, respBody string, reqHeaders map[string]string, ex *Extraction) {
	// Model header (opaque hex→name table).
	for k, v := range reqHeaders {
		if strings.EqualFold(k, "x-goog-ext-525001261-jspb") {
			ex.Model = truncStr(v, maxVal)
			ex.ModelPath = "request header x-goog-ext-525001261-jspb  (opaque jspb — validate hex→model-name table)"
			break
		}
	}
	ex.Note = "Gemini uses batchexecute RPC. Model is in the request HEADER (opaque), NOT the body. " +
		"Prompt is nested in the form-encoded f.req arg array. response_text via UTF-16-length frames → wrb.fr entry[2] → candidate[1][0]."

	best := ""
	for _, frame := range geminiFrames(respBody) {
		arr := asArr(frame)
		for _, entry := range arr {
			e := asArr(entry)
			if len(e) < 3 || asStr(e[0]) != "wrb.fr" {
				continue
			}
			payloadStr := asStr(e[2])
			if payloadStr == "" {
				continue
			}
			pjV, ok := jsonDecode(payloadStr)
			if !ok {
				continue
			}
			pj := asArr(pjV)
			if len(pj) > 1 {
				meta := asArr(pj[1])
				if len(meta) > 0 {
					if c := asStr(meta[0]); c != "" {
						ex.ConversationID, ex.ConversationIDPath = c, "part_json[1][0]  (cid)"
					}
				}
				if len(meta) > 1 {
					if r := asStr(meta[1]); r != "" {
						ex.MessageID, ex.MessageIDPath = r, "part_json[1][1]  (rid)"
					}
				}
			}
			if len(pj) > 4 {
				for _, cand := range asArr(pj[4]) {
					c := asArr(cand)
					var txt, path string
					if len(c) > 1 {
						if a := asArr(c[1]); len(a) > 0 {
							if s := asStr(a[0]); s != "" {
								txt, path = s, "candidate[1][0]"
							}
						}
					}
					if txt == "" && len(c) > 22 {
						if a := asArr(c[22]); len(a) > 0 {
							if s := asStr(a[0]); s != "" {
								txt, path = s, "candidate[22][0]  (fallback)"
							}
						}
					}
					if txt != "" && len(txt) > len(best) {
						best = txt
						ex.ResponseTextPath = "wrb.fr entry[2] → " + path + "  (snapshot, longest per rcid)"
					}
				}
			}
		}
	}
	ex.ResponseTextSample = truncStr(best, maxVal)
}

// stripXSSI removes the leading )]}' guard + surrounding whitespace.
func stripXSSI(s string) string {
	if i := strings.Index(s, ")]}'"); i != -1 {
		s = s[i+4:]
	}
	return strings.TrimLeft(s, " \t\r\n")
}

// geminiFrames walks the UTF-16-code-unit-length-prefixed frames of a
// batchexecute envelope, returning each frame's JSON as a decoded value.
// Incomplete trailing data is left unparsed (fail-soft).
func geminiFrames(body string) []interface{} {
	u := utf16.Encode([]rune(stripXSSI(body)))
	var frames []interface{}
	pos := 0
	for pos < len(u) {
		nl := indexU16(u, pos, '\n')
		if nl == -1 {
			break
		}
		lenStr := strings.TrimSpace(string(utf16.Decode(u[pos:nl])))
		if lenStr == "" || !isAllDigits(lenStr) {
			pos = nl + 1
			continue
		}
		n, err := strconv.Atoi(lenStr)
		if err != nil {
			pos = nl + 1
			continue
		}
		start := nl + 1
		end := start + n
		if end > len(u) {
			break
		}
		jsonStr := string(utf16.Decode(u[start:end]))
		if v, ok := jsonDecode(jsonStr); ok {
			frames = append(frames, v)
		}
		pos = end
	}
	return frames
}

func indexU16(u []uint16, from int, target uint16) int {
	for i := from; i < len(u); i++ {
		if u[i] == target {
			return i
		}
	}
	return -1
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ---- generic token/usage sweep --------------------------------------------

// sweepTokenFields walks every decodable JSON `data:` frame of an SSE body (or
// the whole body if not SSE) and returns paths of numeric/string fields whose
// key matches token|usage. Mirrors the harness findKeys sweep.
func sweepTokenFields(body string) []TokenField {
	var out []TokenField
	seen := map[string]bool{}
	consider := func(v interface{}) {
		findTokenKeys(v, "data", &out, seen, 0)
	}
	found := false
	for _, line := range sseLines(body) {
		if payload, ok := dataPayload(line); ok {
			if v, ok := jsonDecode(payload); ok {
				consider(v)
				found = true
			}
		}
		if len(out) >= 20 {
			break
		}
	}
	if !found {
		if v, ok := jsonDecode(body); ok {
			consider(v)
		}
	}
	return out
}

func findTokenKeys(v interface{}, path string, out *[]TokenField, seen map[string]bool, depth int) {
	if depth > 8 || len(*out) >= 20 {
		return
	}
	switch x := v.(type) {
	case map[string]interface{}:
		for k, val := range x {
			child := path + "." + k
			if tokenKeyRe(k) {
				switch val.(type) {
				case json.Number, float64, string, bool:
					if !seen[child] {
						seen[child] = true
						*out = append(*out, TokenField{Path: child, Value: val})
					}
				}
			}
			findTokenKeys(val, child, out, seen, depth+1)
		}
	case []interface{}:
		for i, val := range x {
			if i >= 50 {
				break
			}
			findTokenKeys(val, fmt.Sprintf("%s[%d]", path, i), out, seen, depth+1)
		}
	}
}

// tokenKeyRe reports whether a field name is a USAGE / token-COUNT field worth
// recording — NOT an arbitrary "token" auth field. A live capture surfaced a
// conduit_token JWT and a read_write_token via the old "contains token" rule;
// restricting to count/usage fields keeps auth secrets out of dumps.
func tokenKeyRe(k string) bool {
	lk := strings.ToLower(k)
	if strings.Contains(lk, "usage") {
		return true
	}
	switch lk {
	case "input_tokens", "output_tokens", "total_tokens", "prompt_tokens",
		"completion_tokens", "cache_read_tokens", "cache_creation_tokens",
		"cache_read_input_tokens", "cache_creation_input_tokens",
		"reasoning_tokens", "cached_tokens", "tokens", "token_count":
		return true
	}
	// Plural "*_tokens" count fields (e.g. thoughts_tokens) — but never the
	// singular "*_token" auth fields (conduit_token, read_write_token,
	// access_token, id_token, csrf_token, …).
	return strings.HasSuffix(lk, "_tokens")
}
