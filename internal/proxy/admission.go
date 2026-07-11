package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// Admitter is the optional pre-forward input-admission gate (admission spec
// §6.2 — the SECONDARY seam; the SDK admit() front-door is primary, AD7).
// When non-nil on [Options], the proxy calls it BEFORE forwarding a request
// upstream, passing the resolved provider, the incoming user message, and the
// session id. In enforce mode the gate may block the request, in which case
// the proxy returns a provider-shaped refusal and never forwards; in observe
// mode the gate records the shadow verdict and never blocks.
//
// Bound at the obs wiring point (cmd/observer/obs_wire.go) so internal/proxy
// never imports internal/obs — the reverse-import boundary. nil ⇒ admission
// disabled, zero overhead.
type Admitter interface {
	Admit(ctx context.Context, in AdmitInput) AdmitResult
}

// AdmitInput is the plain request the proxy hands the gate. The proxy owns
// provider-shape knowledge (it already parses these bodies), so the gate
// receives a resolved user message rather than raw bytes — keeping the seam a
// value-in/value-out contract with no obs type leakage.
type AdmitInput struct {
	Provider  string
	Text      string
	SessionID string
	RequestID string
	// User is the end-user identity the app shares on a proxy-routed request
	// (the AdmissionUserHeader integration requirement — org-hosted-app model).
	// Empty ⇒ the per-end-user budget gate is inert; the app-wide policy still
	// applies.
	User string
}

// AdmitResult is the gate's decision. Block is true only in enforce mode on a
// terminal (ask/deny) verdict; Reason/Criterion annotate the refusal.
type AdmitResult struct {
	Block     bool
	Reason    string
	Criterion string
}

// admit runs the pre-forward gate on a parsed request body. It returns the
// gate's result and true when the proxy should short-circuit (block); false
// means forward as usual. Safe to call with a nil admitter (returns forward).
func (p *Proxy) admit(ctx context.Context, provider string, body []byte, userID string) (AdmitResult, bool) {
	if p.admitter == nil {
		return AdmitResult{}, false
	}
	text := extractLastUserText(provider, body)
	if text == "" {
		// Nothing to judge (a tool-result-only turn, or an unparseable
		// body) — never gate.
		return AdmitResult{}, false
	}
	res := p.admitter.Admit(ctx, AdmitInput{
		Provider:  provider,
		Text:      text,
		SessionID: sessionIDForProvider(provider, body),
		User:      userID,
	})
	return res, res.Block
}

// admissionUser reads the end-user identity the app shares on a proxy-routed
// request (the AdmissionUserHeader integration requirement — org-hosted-app
// model). An empty header name or an absent header yields "" (the per-end-user
// budget gate is then inert; the app-wide policy still applies).
func (p *Proxy) admissionUser(r *http.Request) string {
	if p.admissionUserHeader == "" {
		return ""
	}
	return strings.TrimSpace(r.Header.Get(p.admissionUserHeader))
}

// writeAdmissionRefusal writes the provider-shaped refusal the proxy returns
// when admission blocks a request in enforce mode. A provider-shaped error
// lets the calling app's SDK surface it as a normal API error rather than a
// malformed response.
func writeAdmissionRefusal(w http.ResponseWriter, provider, reason string) {
	if reason == "" {
		reason = "Request blocked by admission policy."
	}
	var body []byte
	if provider == models.ProviderOpenAI {
		body, _ = json.Marshal(map[string]any{
			"error": map[string]any{
				"message": reason,
				"type":    "invalid_request_error",
				"code":    "admission_denied",
			},
		})
	} else {
		// Anthropic-shaped error (the default provider lane).
		body, _ = json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "permission_error",
				"message": reason,
			},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(body)
}

// sessionIDForProvider resolves the session id from a request body using the
// same per-provider extractors the compression path uses.
func sessionIDForProvider(provider string, body []byte) string {
	if provider == models.ProviderOpenAI {
		return extractOpenAISessionID(body)
	}
	return extractAnthropicSessionID(body)
}

// extractLastUserText returns the text of the LAST user message in a provider
// request body — the "incoming user request" admission gates. Empty when no
// user text is present (a tool-result-only turn or an unparseable body);
// admission treats empty as nothing to judge. Anthropic + OpenAI (chat and
// Responses) shapes are handled; other providers return "" (best-effort).
func extractLastUserText(provider string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if provider == models.ProviderOpenAI {
		return lastUserTextOpenAI(body)
	}
	return lastUserTextAnthropic(body)
}

// lastUserTextAnthropic reads the last user message from a Messages API body.
func lastUserTextAnthropic(body []byte) string {
	var raw struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	for i := len(raw.Messages) - 1; i >= 0; i-- {
		if raw.Messages[i].Role != "user" {
			continue
		}
		if t := decodeMessageText(raw.Messages[i].Content); t != "" {
			return t
		}
	}
	return ""
}

// lastUserTextOpenAI reads the last user message from a Chat Completions
// (messages[]) body, falling back to the Responses API (input string or
// input[] of role/content items).
func lastUserTextOpenAI(body []byte) string {
	var raw struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	for i := len(raw.Messages) - 1; i >= 0; i-- {
		if raw.Messages[i].Role != "user" {
			continue
		}
		if t := decodeMessageText(raw.Messages[i].Content); t != "" {
			return t
		}
	}
	// Responses API: input is a string, or an array of {role, content} items.
	in := bytes.TrimSpace(raw.Input)
	if len(in) == 0 {
		return ""
	}
	switch in[0] {
	case '"':
		var s string
		if json.Unmarshal(in, &s) == nil {
			return strings.TrimSpace(s)
		}
	case '[':
		var items []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(in, &items) == nil {
			for i := len(items) - 1; i >= 0; i-- {
				if items[i].Role != "user" {
					continue
				}
				if t := decodeMessageText(items[i].Content); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// decodeMessageText renders a message content field to plain text. Content is
// either a JSON string or an array of content blocks; any block carrying a
// non-empty "text" field contributes (covers Anthropic text, OpenAI chat
// text, and Responses API input_text). Non-text blocks (images, tool_result)
// are skipped.
func decodeMessageText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	if raw[0] != '[' {
		return ""
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(bl.Text)
	}
	return strings.TrimSpace(b.String())
}
