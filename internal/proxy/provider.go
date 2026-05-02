package proxy

import (
	"encoding/json"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// providerForPath routes a request path to the upstream provider. Anthropic's
// Messages API lives under /v1/messages; everything else under /v1 (chat
// completions, responses, embeddings, models) goes to OpenAI. The default is
// anthropic — Claude Code sets ANTHROPIC_BASE_URL to the root of the proxy
// and hits paths that don't always begin with /v1 (e.g. health probes).
func providerForPath(path string) string {
	if strings.HasPrefix(path, "/v1/messages") {
		return models.ProviderAnthropic
	}
	if strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/responses") ||
		strings.HasPrefix(path, "/v1/completions") ||
		strings.HasPrefix(path, "/v1/embeddings") {
		return models.ProviderOpenAI
	}
	return models.ProviderAnthropic
}

// requestShape captures the fields extracted from the client's request body
// before forwarding. All fields are optional — a body that doesn't parse is
// not an error; we just store less metadata.
type requestShape struct {
	Model            string
	MessageCount     int
	ToolUseCount     int
	SystemPromptHash string
	Stream           bool
}

// parseRequest inspects the JSON request body and extracts the pieces we want
// to log with the turn. Both Anthropic and OpenAI use {model, messages,
// tools, stream, system} at the top level; we treat them uniformly.
func parseRequest(body []byte) requestShape {
	var shape requestShape
	if len(body) == 0 {
		return shape
	}
	var raw struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Messages []json.RawMessage `json:"messages"`
		Tools    []json.RawMessage `json:"tools"`
		System   json.RawMessage   `json:"system"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return shape
	}
	shape.Model = raw.Model
	shape.MessageCount = len(raw.Messages)
	shape.ToolUseCount = len(raw.Tools)
	shape.Stream = raw.Stream
	if len(raw.System) > 0 && string(raw.System) != "null" {
		shape.SystemPromptHash = sha256Hex(raw.System)
	}
	return shape
}

// responseShape is the usage+meta extracted from a non-streaming response
// body. Fields are 0/"" when the provider didn't supply them.
type responseShape struct {
	Model                 string
	RequestID             string
	InputTokens           int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64
	StopReason            string
}

// parseAnthropicResponse extracts usage and metadata from a non-streaming
// Anthropic Messages API response body. Unknown JSON is tolerated — the
// returned shape just carries zero values.
//
// Anthropic exposes the cache-creation tier breakdown via
// usage.cache_creation.{ephemeral_5m_input_tokens, ephemeral_1h_input_tokens}.
// usage.cache_creation_input_tokens (legacy single field) carries the total.
// We capture the total in CacheCreationTokens and the 1h subset in
// CacheCreation1hTokens — the engine subtracts to get the 5m portion.
func parseAnthropicResponse(body []byte) responseShape {
	var raw struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheCreation            struct {
				Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
				Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return responseShape{}
	}
	total := raw.Usage.CacheCreationInputTokens
	if total == 0 {
		// Newer API builds may emit only the breakdown, not the total.
		total = raw.Usage.CacheCreation.Ephemeral5mInputTokens +
			raw.Usage.CacheCreation.Ephemeral1hInputTokens
	}
	return responseShape{
		Model:                 raw.Model,
		RequestID:             raw.ID,
		InputTokens:           raw.Usage.InputTokens,
		OutputTokens:          raw.Usage.OutputTokens,
		CacheReadTokens:       raw.Usage.CacheReadInputTokens,
		CacheCreationTokens:   total,
		CacheCreation1hTokens: raw.Usage.CacheCreation.Ephemeral1hInputTokens,
		StopReason:            raw.StopReason,
	}
}

// parseOpenAIResponse extracts usage and metadata from a non-streaming OpenAI
// Chat Completions response body. OpenAI uses {usage: {prompt_tokens,
// completion_tokens}} at the top level. The /v1/responses endpoint uses
// {usage: {input_tokens, output_tokens}} so both key sets are tried.
func parseOpenAIResponse(body []byte) responseShape {
	var raw struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			InputTokens         int64 `json:"input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return responseShape{}
	}
	shape := responseShape{
		Model:           raw.Model,
		RequestID:       raw.ID,
		CacheReadTokens: raw.Usage.PromptTokensDetails.CachedTokens,
	}
	if raw.Usage.PromptTokens > 0 {
		shape.InputTokens = raw.Usage.PromptTokens
	} else {
		shape.InputTokens = raw.Usage.InputTokens
	}
	if raw.Usage.CompletionTokens > 0 {
		shape.OutputTokens = raw.Usage.CompletionTokens
	} else {
		shape.OutputTokens = raw.Usage.OutputTokens
	}
	if len(raw.Choices) > 0 {
		shape.StopReason = raw.Choices[0].FinishReason
	}
	return shape
}
