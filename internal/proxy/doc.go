// Package proxy is the API reverse proxy that intercepts Anthropic Messages
// API and OpenAI Chat Completions calls for accurate token/cost capture. It
// forwards every request unchanged to the configured upstream, tees the
// response for logging, and writes one row per turn to the `api_turns` table.
//
// SSE streams are passed through with http.Flusher so the AI tool sees bytes
// as they arrive; the tee parses the terminal event for usage counts.
//
// P1 (spec §2): the proxy never breaks the host tool. Upstream failures are
// forwarded unchanged with a Bad Gateway response only when the upstream is
// unreachable — errors from the provider are passed through.
//
// See spec §9.
package proxy
