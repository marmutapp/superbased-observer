package messagesummary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation"
)

// DefaultOpenAIBaseURL is the production OpenAI endpoint for the
// API-key auth path. Tests inject an httptest.Server URL via
// [OpenAISummarizerOptions.BaseURL].
//
// ChatGPT-Plus subscription auth (Bearer eyJ... JWT) is NOT supported
// by this summariser — JWTs would require path-translation to
// chatgpt.com/backend-api/codex/responses and have scope constraints
// that make ad-hoc Responses API calls unreliable. [NewOpenAIFactory]
// returns nil for sessions whose Authorization header looks like a JWT,
// so the rolling-summ pass no-ops gracefully on those sessions. They
// still get every other compression benefit (per-block, tool-defs trim,
// stash, prefix-cache invariance via the fast-path early-return).
const DefaultOpenAIBaseURL = "https://api.openai.com"

// DefaultOpenAIModel is gpt-5-nano: codex's cheap-fast tier (free per
// OpenAI's 2026-04-29 catalog, see internal/intelligence/cost/pricing.go).
// Caller can override per-deploy via config.
const DefaultOpenAIModel = "gpt-5-nano"

// DefaultOpenAIMaxTokens caps the Responses API summary output. 512
// tokens is plenty for the "dense paragraph of facts" prompt.
const DefaultOpenAIMaxTokens = 512

// OpenAISummarizerOptions configures an [OpenAISummarizer]. Same shape
// as [SummarizerOptions] minus the AnthropicVersion field (OpenAI
// doesn't use a version header) and with OpenAI-specific defaults.
type OpenAISummarizerOptions struct {
	BaseURL    string
	Model      string
	MaxTokens  int
	HTTPClient *http.Client
	Recorder   CallRecorder
	SessionID  string
}

// OpenAISummarizer implements [conversation.Summarizer] by calling
// OpenAI's Responses API with credentials from an [AuthCache]. One
// summarizer is constructed per (cache, sessionID) pair via
// [OpenAIFactory]; the OpenAISummarizer itself is stateless modulo its
// auth + config.
type OpenAISummarizer struct {
	creds     AuthCredentials
	baseURL   string
	model     string
	maxTokens int
	client    *http.Client
	recorder  CallRecorder
	sessionID string
}

// NewOpenAISummarizer constructs a summariser with the given credentials
// + options. Auth is captured at construction time (not per-call) — the
// [OpenAIFactory] re-fetches credentials per session so a token rotation
// gets picked up on the next summary call.
func NewOpenAISummarizer(creds AuthCredentials, opts OpenAISummarizerOptions) *OpenAISummarizer {
	s := &OpenAISummarizer{
		creds:     creds,
		baseURL:   firstNonEmpty(opts.BaseURL, DefaultOpenAIBaseURL),
		model:     firstNonEmpty(opts.Model, DefaultOpenAIModel),
		maxTokens: opts.MaxTokens,
		client:    opts.HTTPClient,
		recorder:  opts.Recorder,
		sessionID: opts.SessionID,
	}
	if s.maxTokens <= 0 {
		s.maxTokens = DefaultOpenAIMaxTokens
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 60 * time.Second}
	}
	return s
}

// Summarize implements [conversation.Summarizer]. Returns a single-
// paragraph summary of msgs suitable for inline embedding in the
// conversation. Errors propagate so the pipeline can fall back to the
// original messages without summarisation.
func (s *OpenAISummarizer) Summarize(ctx context.Context, msgs []conversation.Message) (string, error) {
	if s.creds.Authorization == "" {
		return "", errors.New("messagesummary: no Authorization header")
	}
	if len(msgs) == 0 {
		return "", errors.New("messagesummary: empty messages")
	}

	body, err := json.Marshal(buildOpenAIRequestBody(msgs, s.model, s.maxTokens))
	if err != nil {
		return "", fmt.Errorf("messagesummary: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("messagesummary: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.creds.Authorization)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("messagesummary: do: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("messagesummary: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("messagesummary: upstream %d: %s", resp.StatusCode, truncateForError(respBody))
	}

	summary, usage, err := extractOpenAISummaryAndUsage(respBody)
	if err != nil {
		return "", err
	}
	if summary == "" {
		return "", errors.New("messagesummary: empty summary")
	}

	if s.recorder != nil {
		_ = s.recorder.Record(ctx, SummaryCall{
			SessionID:           s.sessionID,
			Model:               s.model,
			InputTokens:         usage.InputTokens,
			OutputTokens:        usage.OutputTokens,
			CacheReadTokens:     usage.CacheReadTokens,
			CacheCreationTokens: usage.CacheCreationTokens,
		})
	}
	return summary, nil
}

// buildOpenAIRequestBody is the Responses API envelope the OpenAI
// summariser sends. Mirrors the Anthropic shape: instructions
// (system-prompt equivalent) + a single user-role message carrying the
// serialised older-messages content. `stream: false` so we can read
// the body in one shot.
func buildOpenAIRequestBody(msgs []conversation.Message, model string, maxTokens int) map[string]any {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Text)
		sb.WriteString("\n\n")
	}
	return map[string]any{
		"model":             model,
		"instructions":      summarisationPrompt,
		"max_output_tokens": maxTokens,
		"stream":            false,
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": sb.String()},
				},
			},
		},
	}
}

// openaiUsageBlock mirrors OpenAI's response.usage shape so we can
// extract token counts for the call recorder.
//
// Responses API exposes:
//
//	"usage": {
//	  "input_tokens": N,
//	  "output_tokens": M,
//	  "input_tokens_details": {"cached_tokens": K},
//	  "output_tokens_details": {"reasoning_tokens": Q}
//	}
//
// We map cached_tokens → CacheReadTokens for parity with the Anthropic
// summary_calls schema. CacheCreationTokens is left zero on the OpenAI
// path — the Responses API doesn't break out a cache_creation tier
// (writes are folded into input_tokens).
type openaiUsageBlock struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// extractOpenAISummaryAndUsage pulls the assistant text + token-usage
// block from a Responses API non-streaming response. The shape we
// expect:
//
//	{
//	  "output": [
//	    {"type":"message","content":[{"type":"output_text","text":"..."}]}
//	  ],
//	  "usage": {"input_tokens": N, ...}
//	}
//
// Concatenates every output_text block (defensive — multiple message
// items can occur), trims whitespace.
func extractOpenAISummaryAndUsage(body []byte) (string, openaiUsageBlock, error) {
	var parsed struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", openaiUsageBlock{}, fmt.Errorf("messagesummary: parse response: %w", err)
	}
	var sb strings.Builder
	for _, item := range parsed.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				sb.WriteString(c.Text)
			}
		}
	}
	return strings.TrimSpace(sb.String()), openaiUsageBlock{
		InputTokens:     parsed.Usage.InputTokens,
		OutputTokens:    parsed.Usage.OutputTokens,
		CacheReadTokens: parsed.Usage.InputTokensDetails.CachedTokens,
	}, nil
}

// OpenAIFactory implements [conversation.SummarizerFactory] for the
// OpenAI / codex side. Each For() call constructs a fresh
// [OpenAISummarizer] bound to that session's most-recently-observed
// credentials.
//
// Returns nil when the session has no credentials cached, or when the
// cached Authorization is a ChatGPT-Plus JWT (Bearer eyJ...). JWT users
// get every other compression benefit; D20 specifically degrades for
// them because routing through chatgpt.com/backend-api requires
// path-translation + scope handling we don't ship in Phase 2.
type OpenAIFactory struct {
	cache *AuthCache
	opts  OpenAISummarizerOptions
}

// NewOpenAIFactory constructs an OpenAIFactory.
func NewOpenAIFactory(cache *AuthCache, opts OpenAISummarizerOptions) *OpenAIFactory {
	return &OpenAIFactory{cache: cache, opts: opts}
}

// For implements [conversation.SummarizerFactory].
func (f *OpenAIFactory) For(sessionID string) conversation.Summarizer {
	if f == nil || f.cache == nil {
		return nil
	}
	creds, ok := f.cache.Get(sessionID)
	if !ok {
		return nil
	}
	if creds.Authorization == "" {
		return nil
	}
	// ChatGPT-Plus JWT: Bearer eyJ... — degrade gracefully. The
	// detection mirrors internal/proxy/provider.go::isChatGPTAuthRequest.
	authz := creds.Authorization
	const bearer = "Bearer "
	if strings.HasPrefix(authz, bearer) {
		token := authz[len(bearer):]
		if strings.HasPrefix(token, "eyJ") {
			return nil
		}
	}
	opts := f.opts
	opts.SessionID = sessionID
	return NewOpenAISummarizer(creds, opts)
}
