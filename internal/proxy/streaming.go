package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// teeStream forwards each chunk of an SSE body to the client as it arrives
// (with Flusher so the AI tool sees events immediately) and also copies the
// bytes into an in-memory buffer for end-of-stream usage extraction. Chunk
// forwarding — rather than line-buffered forwarding — keeps latency low
// even when an upstream event straddles a read boundary.
//
// The copy is capped by maxStreamBufferBytes so a runaway stream can't blow
// memory; once the cap is hit we stop accumulating (but keep forwarding).
// Turns from truncated captures will have zero tokens and are dropped
// by the caller, same as any other unparseable capture.
func (p *Proxy) teeStream(ctx context.Context, w http.ResponseWriter, body io.Reader) []byte {
	const maxStreamBufferBytes = 4 * 1024 * 1024
	flusher, _ := w.(http.Flusher)
	var buf bytes.Buffer
	chunk := make([]byte, 4*1024)
	for {
		if err := ctx.Err(); err != nil {
			return buf.Bytes()
		}
		n, err := body.Read(chunk)
		if n > 0 {
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return buf.Bytes()
			}
			if flusher != nil {
				flusher.Flush()
			}
			if buf.Len() < maxStreamBufferBytes {
				// Cap the copy; stream still forwards fully.
				remaining := maxStreamBufferBytes - buf.Len()
				if n > remaining {
					buf.Write(chunk[:remaining])
				} else {
					buf.Write(chunk[:n])
				}
			}
		}
		if err != nil {
			return buf.Bytes()
		}
	}
}

// streamResult is the accumulated usage+metadata extracted from an SSE
// capture. Zero values mean "not observed". The caller fills in any fields
// that the request body carries (model, message/tool counts).
type streamResult struct {
	Model                 string
	RequestID             string
	InputTokens           int64
	OutputTokens          int64
	CacheReadTokens       int64
	CacheCreationTokens   int64
	CacheCreation1hTokens int64
	StopReason            string
}

// parseSSEStream walks an SSE byte stream and returns the accumulated usage
// numbers for the given provider. Unknown events are ignored; a partial
// capture returns whatever was seen so far.
func parseSSEStream(body []byte, provider string) streamResult {
	if provider == models.ProviderAnthropic {
		return parseAnthropicStream(body)
	}
	return parseOpenAIStream(body)
}

// splitSSEEvents walks the body as an SSE stream and yields one event at a
// time as a slice of "field: value" lines (newlines stripped). An event ends
// at a blank line; the terminal event without a trailing blank line is
// emitted when the body ends. Comment lines (leading ":") are skipped.
func splitSSEEvents(body []byte, yield func(lines [][]byte)) {
	var current [][]byte
	start := 0
	for i := 0; i <= len(body); i++ {
		if i == len(body) || body[i] == '\n' {
			line := body[start:i]
			// Strip trailing \r for \r\n line endings.
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if len(line) == 0 {
				if len(current) > 0 {
					yield(current)
					current = nil
				}
			} else if len(line) > 0 && line[0] != ':' {
				current = append(current, line)
			}
			start = i + 1
		}
	}
	if len(current) > 0 {
		yield(current)
	}
}

// parseAnthropicStream extracts usage from an Anthropic Messages SSE stream.
// Anthropic emits input_tokens + cache_creation_input_tokens +
// cache_read_input_tokens in message_start.message.usage, and output_tokens
// in message_delta.usage. stop_reason lives on message_delta.delta.
// See https://docs.anthropic.com/en/api/messages-streaming.
func parseAnthropicStream(body []byte) streamResult {
	var result streamResult
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
				Type    string `json:"type"`
				Message struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage struct {
						InputTokens              int64 `json:"input_tokens"`
						OutputTokens             int64 `json:"output_tokens"`
						CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
						CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
						CacheCreation            struct {
							Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
							Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
						} `json:"cache_creation"`
					} `json:"usage"`
				} `json:"message"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					InputTokens              int64 `json:"input_tokens"`
					OutputTokens             int64 `json:"output_tokens"`
					CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
					CacheCreation            struct {
						Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
						Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
					} `json:"cache_creation"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			switch ev.Type {
			case "message_start":
				if ev.Message.Model != "" {
					result.Model = ev.Message.Model
				}
				if ev.Message.ID != "" {
					result.RequestID = ev.Message.ID
				}
				if ev.Message.Usage.InputTokens > 0 {
					result.InputTokens = ev.Message.Usage.InputTokens
				}
				if ev.Message.Usage.CacheReadInputTokens > 0 {
					result.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
				}
				cc := ev.Message.Usage.CacheCreationInputTokens
				if cc == 0 {
					cc = ev.Message.Usage.CacheCreation.Ephemeral5mInputTokens +
						ev.Message.Usage.CacheCreation.Ephemeral1hInputTokens
				}
				if cc > 0 {
					result.CacheCreationTokens = cc
				}
				if ev.Message.Usage.CacheCreation.Ephemeral1hInputTokens > 0 {
					result.CacheCreation1hTokens = ev.Message.Usage.CacheCreation.Ephemeral1hInputTokens
				}
				if ev.Message.Usage.OutputTokens > 0 {
					// Newer API builds emit output_tokens on message_start
					// (initial placeholder). message_delta overrides.
					result.OutputTokens = ev.Message.Usage.OutputTokens
				}
			case "message_delta":
				if ev.Delta.StopReason != "" {
					result.StopReason = ev.Delta.StopReason
				}
				if ev.Usage.OutputTokens > 0 {
					result.OutputTokens = ev.Usage.OutputTokens
				}
				if ev.Usage.InputTokens > 0 {
					result.InputTokens = ev.Usage.InputTokens
				}
				// Cache-creation tier breakdown can also land on
				// message_delta (some Anthropic SSE variants emit it
				// here once the upstream has finalized the tier split).
				cc := ev.Usage.CacheCreationInputTokens
				if cc == 0 {
					cc = ev.Usage.CacheCreation.Ephemeral5mInputTokens +
						ev.Usage.CacheCreation.Ephemeral1hInputTokens
				}
				if cc > 0 {
					result.CacheCreationTokens = cc
				}
				if ev.Usage.CacheCreation.Ephemeral1hInputTokens > 0 {
					result.CacheCreation1hTokens = ev.Usage.CacheCreation.Ephemeral1hInputTokens
				}
			}
		}
	})
	return result
}

// parseOpenAIStream extracts usage from an OpenAI SSE stream. OpenAI emits
// `data: {...}` chunks terminated by `data: [DONE]`. When the caller sets
// stream_options={"include_usage":true}, a final chunk before [DONE] carries
// a top-level {usage: {...}} with accurate counts. Without that option, no
// usage is available; we return zero values so the caller can drop the turn.
func parseOpenAIStream(body []byte) streamResult {
	var result streamResult
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
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			if ev.Model != "" {
				result.Model = ev.Model
			}
			if ev.ID != "" {
				result.RequestID = ev.ID
			}
			for _, c := range ev.Choices {
				if c.FinishReason != "" {
					result.StopReason = c.FinishReason
				}
			}
			if ev.Usage.PromptTokens > 0 {
				result.InputTokens = ev.Usage.PromptTokens
			} else if ev.Usage.InputTokens > 0 {
				result.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CompletionTokens > 0 {
				result.OutputTokens = ev.Usage.CompletionTokens
			} else if ev.Usage.OutputTokens > 0 {
				result.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.PromptTokensDetails.CachedTokens > 0 {
				result.CacheReadTokens = ev.Usage.PromptTokensDetails.CachedTokens
			}
		}
	})
	return result
}
