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

// DefaultBaseURL is the production Anthropic endpoint; tests inject
// an httptest.Server URL via [SummarizerOptions.BaseURL].
const DefaultBaseURL = "https://api.anthropic.com"

// DefaultModel is the cheap-but-capable summariser default. Caller
// can override per-deploy via config.
const DefaultModel = "claude-haiku-4-5"

// DefaultAnthropicVersion is the Anthropic API version header value
// the summariser sends. Mirrors the SDK's pinned version so the
// summary call rides the same surface as the regular request.
const DefaultAnthropicVersion = "2023-06-01"

// DefaultMaxTokens caps the summary length so we don't accidentally
// burn budget on a verbose summariser response. 512 tokens is plenty
// for "list 6 facts" — the model rarely stretches past that.
const DefaultMaxTokens = 512

// SummarizerOptions configures an [AnthropicSummarizer].
type SummarizerOptions struct {
	// BaseURL overrides the Anthropic endpoint. Tests inject the
	// httptest.Server URL here. Empty = [DefaultBaseURL].
	BaseURL string
	// Model selects the summarisation model. Empty = [DefaultModel].
	Model string
	// AnthropicVersion sets the `anthropic-version` header. Empty =
	// [DefaultAnthropicVersion].
	AnthropicVersion string
	// MaxTokens caps the summary response. ≤0 = [DefaultMaxTokens].
	MaxTokens int
	// HTTPClient overrides the HTTP client. Nil = a client with a 60s
	// timeout — long enough for a Haiku summary, short enough that a
	// stuck request doesn't hang the parent proxy request indefinitely.
	HTTPClient *http.Client
	// Recorder receives one [SummaryCall] per successful Summarize().
	// Optional — without it, summary calls don't land in the
	// summary_calls DB table and the D20 cost-net dashboard card
	// shows zeros. Production should always wire this; tests stub it
	// or leave it nil.
	Recorder CallRecorder
	// SessionID is the session_id this summariser is bound to. The
	// recorder uses it to populate summary_calls.session_id so the
	// dashboard can group spend per session. Empty allowed.
	SessionID string
}

// CallRecorder receives one row per successful summary call so the
// D20 cost-net dashboard surface can join the Haiku spend against
// rolling_summary savings. The interface lets messagesummary tests
// stub it without importing the cost engine.
type CallRecorder interface {
	Record(ctx context.Context, call SummaryCall) error
}

// SummaryCall is one row of summary_calls. Populated by the
// AnthropicSummarizer parsing Anthropic's response usage block.
type SummaryCall struct {
	SessionID           string
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// CostUSD is computed by the recorder from (model, tokens) using
	// the cost.Engine's pricing table — the AnthropicSummarizer
	// itself doesn't know pricing, only token counts.
}

// AnthropicSummarizer implements [conversation.Summarizer] by calling
// the Anthropic Messages API with credentials from an [AuthCache].
// One summarizer is constructed per (cache, sessionID) pair via
// [Factory]; the AnthropicSummarizer itself is stateless modulo its
// auth + config.
type AnthropicSummarizer struct {
	creds            AuthCredentials
	baseURL          string
	model            string
	anthropicVersion string
	maxTokens        int
	client           *http.Client
	recorder         CallRecorder
	sessionID        string
}

// NewAnthropicSummarizer constructs a summariser with the given
// credentials + options. The auth is captured at construction time
// (not per-call) — the [Factory] re-fetches credentials per session
// so a token rotation gets picked up on the next summary call.
func NewAnthropicSummarizer(creds AuthCredentials, opts SummarizerOptions) *AnthropicSummarizer {
	s := &AnthropicSummarizer{
		creds:            creds,
		baseURL:          firstNonEmpty(opts.BaseURL, DefaultBaseURL),
		model:            firstNonEmpty(opts.Model, DefaultModel),
		anthropicVersion: firstNonEmpty(opts.AnthropicVersion, DefaultAnthropicVersion),
		maxTokens:        opts.MaxTokens,
		client:           opts.HTTPClient,
		recorder:         opts.Recorder,
		sessionID:        opts.SessionID,
	}
	if s.maxTokens <= 0 {
		s.maxTokens = DefaultMaxTokens
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 60 * time.Second}
	}
	return s
}

// Summarize implements [conversation.Summarizer]. Returns a single-
// paragraph summary of msgs suitable for inline embedding in the
// conversation. Errors propagate so the pipeline can fall back to
// the original messages without summarisation (the no-op contract
// of [Pipeline.summarizeIfThreshold]).
func (s *AnthropicSummarizer) Summarize(ctx context.Context, msgs []conversation.Message) (string, error) {
	if s.creds.Empty() {
		return "", errors.New("messagesummary: no auth credentials")
	}
	if len(msgs) == 0 {
		return "", errors.New("messagesummary: empty messages")
	}

	body, err := json.Marshal(buildRequestBody(msgs, s.model, s.maxTokens))
	if err != nil {
		return "", fmt.Errorf("messagesummary: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("messagesummary: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", s.anthropicVersion)
	if s.creds.Authorization != "" {
		req.Header.Set("Authorization", s.creds.Authorization)
	}
	if s.creds.APIKey != "" {
		req.Header.Set("x-api-key", s.creds.APIKey)
	}

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

	summary, usage, err := extractSummaryAndUsage(respBody)
	if err != nil {
		return "", err
	}
	if summary == "" {
		return "", errors.New("messagesummary: empty summary")
	}

	// Record the call for the D20 cost-net dashboard surface. Errors
	// are swallowed — losing a single ledger row must never fail the
	// summarisation flow.
	if s.recorder != nil {
		_ = s.recorder.Record(ctx, SummaryCall{
			SessionID:           s.sessionID,
			Model:               s.model,
			InputTokens:         usage.InputTokens,
			OutputTokens:        usage.OutputTokens,
			CacheReadTokens:     usage.CacheReadInputTokens,
			CacheCreationTokens: usage.CacheCreationInputTokens,
		})
	}
	return summary, nil
}

// usageBlock mirrors Anthropic's response.usage shape so we can
// extract token counts for the call recorder.
type usageBlock struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// summarisationPrompt is the system instruction that asks for a
// dense, factual summary suitable for re-injection. Phrased to bias
// the model toward facts (file paths, decisions, failures) rather
// than narrative — the marker sits inline in a long conversation and
// shorter, denser content is more useful per byte.
const summarisationPrompt = `You are summarising part of an ongoing AI coding conversation. The summary will replace the original messages inline so a downstream model can pick up where the user left off without re-reading every message.

Produce ONE dense paragraph (no headings, no bullet points, no markdown). Cover only facts that are load-bearing for continuing the work: files read, files edited, decisions made, errors encountered, current goal. Skip pleasantries, exploratory back-and-forth, and anything the model's tools can re-derive cheaply.

Maximum length: 200 words. Be specific (file paths, function names, error messages) rather than generic ("we discussed code").`

// buildRequestBody is the Anthropic request envelope the summariser
// sends. Kept simple: one user-role message carrying the serialised
// older-messages content, system prompt as the summarisation
// instruction.
func buildRequestBody(msgs []conversation.Message, model string, maxTokens int) map[string]any {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		sb.WriteString(m.Text)
		sb.WriteString("\n\n")
	}
	return map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"system":     summarisationPrompt,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": sb.String(),
			},
		},
	}
}

// extractSummaryAndUsage pulls the assistant text + token-usage block
// from an Anthropic Messages API response. The response shape we
// expect:
//
//	{
//	  "content": [{"type":"text","text":"..."}],
//	  "usage":   {"input_tokens": N, "output_tokens": M, ...}
//	}
//
// Concatenates every text block (defensive — the API can return
// multiple), trims whitespace, returns usage for the recorder.
func extractSummaryAndUsage(body []byte) (string, usageBlock, error) {
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage usageBlock `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", usageBlock{}, fmt.Errorf("messagesummary: parse response: %w", err)
	}
	var sb strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return strings.TrimSpace(sb.String()), parsed.Usage, nil
}

// truncateForError caps the bytes embedded in error messages so a
// large upstream error response doesn't blow up our log lines.
func truncateForError(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Factory implements [conversation.SummarizerFactory] by combining an
// [AuthCache] with [SummarizerOptions]. Each For() call constructs a
// fresh [AnthropicSummarizer] bound to that session's most-recently-
// observed credentials. Returns nil when the session has no
// credentials cached — the pipeline then no-ops the rolling-summ
// pass for that session.
type Factory struct {
	cache *AuthCache
	opts  SummarizerOptions
}

// NewFactory constructs a Factory.
func NewFactory(cache *AuthCache, opts SummarizerOptions) *Factory {
	return &Factory{cache: cache, opts: opts}
}

// For implements [conversation.SummarizerFactory]. Each call binds a
// fresh AnthropicSummarizer to the session's most-recently-observed
// credentials and threads the per-session id through to the call
// recorder so summary_calls.session_id is populated.
func (f *Factory) For(sessionID string) conversation.Summarizer {
	if f == nil || f.cache == nil {
		return nil
	}
	creds, ok := f.cache.Get(sessionID)
	if !ok || creds.Empty() {
		return nil
	}
	opts := f.opts
	opts.SessionID = sessionID
	return NewAnthropicSummarizer(creds, opts)
}
