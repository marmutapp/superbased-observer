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
// # One turn renders as a chat: user row + assistant row
//
// A captured turn is a full prompt→response exchange, so at content-bearing
// granularity (redacted / full) it normalizes to TWO ToolEvents, exactly like
// the coding-agent adapters:
//
//   - an ActionUserPrompt event carrying the scrubbed prompt in RawToolInput,
//     keyed with the dashboard's synthetic MessageID "user:<mid>", so the
//     timeline renders it as a USER message row; and
//   - the ActionAssistantMessage event carrying the scrubbed response in
//     ToolOutput and the real message id (mid), rendered as the ASSISTANT
//     row.
//
// The prompt lives ONLY on the user_prompt event and the response ONLY on the
// assistant event — content is never stored twice. The two events carry
// DISTINCT SourceEventIDs (assistant keeps sourceEventID(turn); the
// user_prompt appends ":user"; the token event appends ":tok"), so the
// (source_file, source_event_id) idempotency key in store.InsertActions can
// never dedup-swallow one event against another.
//
// At usage_only there is no content, so only the single metadata-only
// assistant event is emitted — a user row with no text would be noise.
//
// # API-call detail on the assistant row
//
// The assistant event ALWAYS carries an ActionMetadata blob with the turn's
// API-call detail: request_url, id_source, the EFFECTIVE (post-clamp)
// granularity, and the estimated prompt/response token counts. These are
// non-content fields, safe at every granularity; request_url is scrubbed as a
// backstop and is a path, never prompt/response content. Latency is already
// on the event's DurationMs, so it is not duplicated in the metadata.
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
//	  "conversation_id":      "…",             // string, becomes the observer SessionID when present (optional — an id-less turn is NOT dropped; it ingests under a fallback key: message_id, then capture_id, then captured_at)
//	  "message_id":           "…",             // string, per-turn id (optional; the next fallback session key after conversation_id)
//	  "model":                "gpt-4o",        // string, model label if visible (optional)
//	  "request_url":          "https://chatgpt.com/backend-api/conversation",
//	  "prompt_text":          "…",             // string, gated by granularity
//	  "response_text":        "…",             // string, gated by granularity
//	  "prompt_tokens_est":    123,             // int64, client estimate (0 ⇒ server estimates)
//	  "response_tokens_est":  456,             // int64, client estimate (0 ⇒ server estimates)
//	  "latency_ms":           1200,            // int64, first-byte-to-done latency
//	  "captured_at":          "2026-07-10T…Z", // RFC3339 or unix seconds (string); "" ⇒ now
//	  "granularity":          "usage_only",    // "usage_only" | "redacted" | "full"; "" ⇒ usage_only
//	  "title":                "…",             // string, conversation title if visible (optional)
//	  "id_source":            "request",       // "none"|"request"|"stream"|"resume"|"url"|"chain": conversation_id provenance (optional; "" ⇒ unknown). Routed to browser-health telemetry, not a DB column.
//	  "capture_id":           "…"              // string, opaque random per-turn id (crypto.randomUUID); the id-less-turn fallback session key below conversation_id/message_id. Content-free, so it never crosses the org-push privacy boundary. (optional; older bridges omit it)
//	}
//
// The synthesized ProjectRoot is browser://<host> (e.g. browser://chatgpt.com)
// so browser turns group under a stable, obviously-synthetic root that can
// never collide with a real filesystem path. The SessionID is the site's
// conversation id when present; a turn that arrives without one is NOT dropped
// — it ingests under a fallback session key (message_id, then the opaque
// capture_id, then a captured_at last-resort for old bridges), so a multi-turn
// conversation aggregates into one session while id-less turns still land as
// their own single-turn sessions.
package browserchat
