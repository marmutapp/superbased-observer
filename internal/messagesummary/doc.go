// Package messagesummary is the production summariser for the D20
// rolling-summarisation feature (Tier 2; v1.4.43+). Three pieces:
//
//   - [AuthCache] — process-wide map keyed by session_id, holding the
//     Authorization / x-api-key headers the proxy most recently
//     intercepted on a regular Anthropic request. Populated by the
//     proxy's serve() before invoking the compressor.
//   - [AnthropicSummarizer] — calls the Anthropic Messages API
//     (`/v1/messages`) with the captured headers, returning a short
//     summary string that the conversation pipeline embeds inline as
//     a `[<N> earlier messages summarized: ...]` marker.
//   - [Factory] — combines the cache + summariser into the
//     [conversation.SummarizerFactory] interface that
//     `Pipeline.WithSummarizerFactory` consumes.
//
// The split keeps the conversation pipeline import-cycle-free: it only
// depends on its own [conversation.Summarizer] interface; the HTTP /
// auth machinery lives here.
package messagesummary
