package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Guard-layer integration (guard spec §8). The proxy holds only this
// file's plain types — the Compressor/CacheSink precedent: the guard
// composition layer and the store are wired behind the GuardScanner
// interface by cmd/observer, and no guard/policy type crosses into
// this package (§17.2).
//
// Placement contract (§8.1): serve() makes ONE ScanRequest call per
// request, immediately after compression produces the final outbound
// body (the same post-compression position cachetrack hashes), and
// one InspectResponse call on the parsed response surface (stream and
// non-stream). Proxy-only, zero effect on other capture paths — the
// compression precedent.

// GuardScanner is the proxy's view of the guard layer. Implementations
// (the cmd/observer adapter) own verdict persistence and alerting;
// the proxy only acts on the returned request decision.
type GuardScanner interface {
	// ScanRequest scans the final outbound body. The proxy forwards,
	// masks, or denies per the result. Synchronous on the request
	// path — implementations hold the §17.9 ≤10ms p99 budget.
	ScanRequest(ctx context.Context, provider string, body []byte, sessionID string) GuardRequestResult
	// InspectResponse receives the response's tool_use blocks (the
	// model's intended next actions, §8.3). Called after the client
	// already has its bytes — off the latency path. Flag/alert only;
	// the proxy never rewrites responses (v1). apiTurnID is the
	// api_turn this response was captured as (0 when the turn was
	// dropped) — implementations anchor the persisted verdict to it so
	// the obs trajectory enrichment can surface the verdict on the span.
	InspectResponse(ctx context.Context, sessionID string, apiTurnID int64, tools []GuardToolUse)
}

// GuardRequestResult is the §8.2 egress decision the proxy acts on.
type GuardRequestResult struct {
	// Action is "" or "allow" (forward unchanged), "mask" (forward
	// Body instead), or "deny" (synthetic 403, §8.5).
	Action string
	// Body is the masked body when Action == "mask".
	Body []byte
	// RuleID / Reason feed the §8.5 provider-shaped error body on
	// deny ("[observer-guard R-172] ...").
	RuleID string
	Reason string
}

// GuardToolUse is one tool_use block extracted from a response.
type GuardToolUse struct {
	// Name is the tool name as the model emitted it.
	Name string
	// Input is the JSON input object. For OpenAI shapes the
	// `arguments` JSON string is decoded to the object form first, so
	// consumers see one shape.
	Input json.RawMessage
}

// guardDenyBody renders the §8.5 provider-shaped error body: clients
// surface it as a normal API error (never connection-drop — clients
// retry-storm on those), with the rule ID and remediation inline so
// the AGENT reads it and self-corrects.
func guardDenyBody(provider, ruleID, reason string) []byte {
	msg := fmt.Sprintf("[observer-guard %s] request blocked by the egress policy: %s", ruleID, reason)
	type errObj struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if provider == models.ProviderAnthropic {
		body, _ := json.Marshal(struct {
			Type  string `json:"type"`
			Error errObj `json:"error"`
		}{Type: "error", Error: errObj{Type: "invalid_request_error", Message: msg}})
		return body
	}
	body, _ := json.Marshal(struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Param   *string `json:"param"`
			Code    string  `json:"code"`
		} `json:"error"`
	}{Error: struct {
		Message string  `json:"message"`
		Type    string  `json:"type"`
		Param   *string `json:"param"`
		Code    string  `json:"code"`
	}{Message: msg, Type: "invalid_request_error", Code: "observer_guard_denied"}})
	return body
}

// extractToolUses pulls the tool_use blocks out of a response in any
// of the wire shapes the proxy serves: Anthropic JSON + SSE, OpenAI
// Chat Completions JSON + SSE deltas, Responses API JSON + SSE
// (response.completed). Tolerant — unknown shapes return nil.
func extractToolUses(provider string, body []byte, isStream bool) []GuardToolUse {
	if len(body) == 0 {
		return nil
	}
	if isStream {
		if provider == models.ProviderAnthropic {
			return extractAnthropicStreamToolUses(body)
		}
		return extractOpenAIStreamToolUses(body)
	}
	if provider == models.ProviderAnthropic {
		return extractAnthropicToolUses(body)
	}
	return extractOpenAIToolUses(body)
}

// extractAnthropicToolUses reads content[].type=="tool_use" blocks
// from a non-streaming Messages API response.
func extractAnthropicToolUses(body []byte) []GuardToolUse {
	var raw struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	var out []GuardToolUse
	for _, c := range raw.Content {
		if c.Type == "tool_use" && c.Name != "" {
			out = append(out, GuardToolUse{Name: c.Name, Input: c.Input})
		}
	}
	return out
}

// extractAnthropicStreamToolUses assembles tool_use blocks from an
// SSE capture: content_block_start carries the name per block index;
// input_json_delta events stream the input object as partial_json
// fragments concatenated per index.
func extractAnthropicStreamToolUses(body []byte) []GuardToolUse {
	type blockAcc struct {
		name  string
		input bytes.Buffer
	}
	blocks := map[int]*blockAcc{}
	var order []int
	splitSSEEvents(body, func(lines [][]byte) {
		for _, line := range lines {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) == 0 {
				continue
			}
			var ev struct {
				Type         string `json:"type"`
				Index        int    `json:"index"`
				ContentBlock struct {
					Type  string          `json:"type"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content_block"`
				Delta struct {
					Type        string `json:"type"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "content_block_start":
				if ev.ContentBlock.Type != "tool_use" || ev.ContentBlock.Name == "" {
					continue
				}
				acc := &blockAcc{name: ev.ContentBlock.Name}
				// Some builds emit a (usually empty) input object on
				// the start event; keep it as the seed in case no
				// deltas follow.
				if len(ev.ContentBlock.Input) > 0 && !bytes.Equal(ev.ContentBlock.Input, []byte("{}")) {
					acc.input.Write(ev.ContentBlock.Input)
				}
				blocks[ev.Index] = acc
				order = append(order, ev.Index)
			case "content_block_delta":
				if ev.Delta.Type != "input_json_delta" {
					continue
				}
				if acc, ok := blocks[ev.Index]; ok {
					acc.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		}
	})
	var out []GuardToolUse
	for _, idx := range order {
		acc := blocks[idx]
		input := acc.input.Bytes()
		if len(input) == 0 {
			input = []byte("{}")
		}
		out = append(out, GuardToolUse{Name: acc.name, Input: json.RawMessage(input)})
	}
	return out
}

// extractOpenAIToolUses reads tool calls from a non-streaming OpenAI
// body: Chat Completions choices[].message.tool_calls and Responses
// API output[].type=="function_call". Both key sets are tried.
func extractOpenAIToolUses(body []byte) []GuardToolUse {
	var raw struct {
		Choices []struct {
			Message struct {
				ToolCalls []openAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	var out []GuardToolUse
	for _, ch := range raw.Choices {
		for _, tc := range ch.Message.ToolCalls {
			if tu, ok := tc.toGuardToolUse(); ok {
				out = append(out, tu)
			}
		}
	}
	for _, o := range raw.Output {
		if o.Type == "function_call" && o.Name != "" {
			out = append(out, GuardToolUse{Name: o.Name, Input: decodeArguments(o.Arguments)})
		}
	}
	return out
}

// openAIToolCall is the Chat Completions tool-call shape (full or
// delta — deltas carry an index and fragmentary arguments).
type openAIToolCall struct {
	Index    int `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toGuardToolUse converts a complete tool call.
func (tc *openAIToolCall) toGuardToolUse() (GuardToolUse, bool) {
	if tc.Function.Name == "" {
		return GuardToolUse{}, false
	}
	return GuardToolUse{Name: tc.Function.Name, Input: decodeArguments(tc.Function.Arguments)}, true
}

// decodeArguments normalizes OpenAI's arguments JSON STRING into the
// object form Anthropic uses, so downstream consumers see one shape.
func decodeArguments(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage("{}")
	}
	if json.Valid([]byte(args)) {
		return json.RawMessage(args)
	}
	return json.RawMessage("{}")
}

// extractOpenAIStreamToolUses assembles tool calls from an OpenAI SSE
// capture. Two shapes: the Responses API's terminal response.completed
// event embeds the full output array (codex's path); Chat Completions
// streams choices[].delta.tool_calls fragments accumulated per index.
func extractOpenAIStreamToolUses(body []byte) []GuardToolUse {
	type callAcc struct {
		name string
		args bytes.Buffer
	}
	deltas := map[int]*callAcc{}
	var order []int
	var completed []GuardToolUse
	splitSSEEvents(body, func(lines [][]byte) {
		for _, line := range lines {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						ToolCalls []openAIToolCall `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
				Response struct {
					Output []struct {
						Type      string `json:"type"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"output"`
				} `json:"response"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			for _, o := range ev.Response.Output {
				if o.Type == "function_call" && o.Name != "" {
					completed = append(completed, GuardToolUse{Name: o.Name, Input: decodeArguments(o.Arguments)})
				}
			}
			for _, ch := range ev.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					acc := deltas[tc.Index]
					if acc == nil {
						acc = &callAcc{}
						deltas[tc.Index] = acc
						order = append(order, tc.Index)
					}
					if tc.Function.Name != "" {
						acc.name = tc.Function.Name
					}
					acc.args.WriteString(tc.Function.Arguments)
				}
			}
		}
	})
	// response.completed output is authoritative when present (the
	// deltas would describe the same calls).
	if len(completed) > 0 {
		return completed
	}
	var out []GuardToolUse
	for _, idx := range order {
		acc := deltas[idx]
		if acc.name == "" {
			continue
		}
		out = append(out, GuardToolUse{Name: acc.name, Input: decodeArguments(acc.args.String())})
	}
	return out
}
