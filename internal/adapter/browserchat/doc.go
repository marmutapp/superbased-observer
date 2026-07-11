// Package browserchat normalizes captured turns from browser-based AI
// chatbots (Phase 1: ChatGPT at chatgpt.com) into observer's ToolEvent /
// TokenEvent transport types. It is the server-side landing point for the
// opt-in MV3 browser extension: the extension's MAIN-world interceptor taps
// a completion turn, relays a JSON payload through the native-messaging
// bridge to `observer browser hook`, and this package turns that payload
// into normalized events that flow through the UNCHANGED store.Ingest seam.
//
// # Site is a data discriminator, not five packages
//
// Per the anti-spaghetti discipline (CLAUDE.md #3/#5), the per-site
// differences are a lookup table (siteRules), never a `switch site {}` in
// the normalizer. Phase 1 registers exactly one site (chatgpt-web); Phase 2
// adds claude-web / perplexity-web / gemini-web / copilot-web as additional
// rows, no new package and no new code branch.
//
// # Tokens are ALWAYS estimated
//
// No target UI returns authoritative token counts. The extension estimates
// client-side (the intended dependency is gpt-tokenizer for the OpenAI
// family; a chars/4 stub is acceptable in Phase 1). The server prefers the
// client estimate when present and otherwise recomputes a chars/4 heuristic
// over whatever content granularity allowed it to keep. Either way the
// TokenEvent carries Source = TokenSourceEstimated and Reliability =
// ReliabilityUnreliable — the honest floor. The dashboard must label these
// as estimates.
//
// # Granularity is a capability, not a code branch
//
// The captured-turn payload declares a granularity level; the normalizer
// reads a granularityRules row to decide whether to construct content
// fields at all (§5.3 of the proposal). Phase 1 defaults to usage-only
// (counts + model + latency, NO content). Redacted / full levels carry
// content that STILL hits the ingest-time scrub seam (scrub.Scrubber.String
// / RawJSON — never ScrubForward, which is proxy-outbound-only). The seam is
// wired now even though the default is usage-only, so no source-identity
// branch is needed later.
//
// # The wire contract (single-sourced here + in the extension README)
//
// The extension → native host → `observer browser hook` all agree on ONE
// JSON schema, the CapturedTurn type below. The schema is versioned
// (SchemaVersion); per the distribution convention the extension version is
// stamped in lockstep with the observer release tag, so an upgrade never
// leaves a version-skewed bridge. Fields:
//
//	{
//	  "schema_version":       1,               // int, CapturedTurnSchemaVersion
//	  "site":                 "chatgpt-web",   // string, must be a known siteRules key
//	  "conversation_id":      "…",             // string, becomes the observer SessionID (required)
//	  "message_id":           "…",             // string, per-turn id (optional; synthesized if absent)
//	  "model":                "gpt-4o",        // string, model label if visible (optional)
//	  "request_url":          "https://chatgpt.com/backend-api/conversation",
//	  "prompt_text":          "…",             // string, gated by granularity
//	  "response_text":        "…",             // string, gated by granularity
//	  "prompt_tokens_est":    123,             // int64, client estimate (0 ⇒ server estimates)
//	  "response_tokens_est":  456,             // int64, client estimate (0 ⇒ server estimates)
//	  "latency_ms":           1200,            // int64, first-byte-to-done latency
//	  "captured_at":          "2026-07-10T…Z", // RFC3339 or unix seconds (string); "" ⇒ now
//	  "granularity":          "usage_only",    // "usage_only" | "redacted" | "full"; "" ⇒ usage_only
//	  "title":                "…"              // string, conversation title if visible (optional)
//	}
//
// The synthesized ProjectRoot is browser://<host> (e.g. browser://chatgpt.com)
// so browser turns group under a stable, obviously-synthetic root that can
// never collide with a real filesystem path. The SessionID is the site's
// conversation id, so a multi-turn conversation aggregates into one session.
package browserchat
