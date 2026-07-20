// Package m365copilotanalytics is the server-side poller for Microsoft 365
// Copilot's org-tier telemetry (World 1 — native-console instance #4, Rail C). It
// is the fourth sibling of internal/orgserver/ccanalytics /
// internal/orgserver/codexanalytics / internal/orgserver/copilotanalytics and
// mirrors their layout (auth.go / poller.go / resolver.go / scheduler.go /
// surface*.go / doc.go) exactly.
//
// IMPORTANT — this is MICROSOFT's M365 Copilot, NOT GitHub Copilot. GitHub
// Copilot is already covered by internal/orgserver/copilotanalytics (instance
// #3). The two share only the word "Copilot": a categorically different vendor
// surface (graph.microsoft.com vs api.github.com; Entra app-only Graph perms vs a
// GitHub PAT; actual prompt+response CONTENT vs engagement/seat/billing metrics).
// Conflating them would violate CLAUDE.md anti-spaghetti rule #3.
//
// Two rails, resolved once at construction into a surfaceSpec strategy so the
// poll loop never branches on rail identity:
//
//   - SurfaceGraph (Rail A, the rich content rail) — per licensed user,
//     GET graph.microsoft.com/v1.0/copilot/users/{id}/interactionHistory/
//     getAllEnterpriseInteractions, paged by $skiptoken / @odata.nextLink,
//     $top=100. Parses aiInteraction objects (interactionType =
//     userPrompt | aiResponse; the text in body.content; appClass surface tag)
//     into per-(day, user, appClass) metric rollups AND the content bodies.
//   - SurfacePurview (Rail B, the metadata governance rail) — the Office 365
//     Management Activity API (RecordType CopilotInteraction 261, Workload
//     Copilot). Metadata only (no prompt/response text): AppHost, Contexts,
//     ThreadID, Messages[], AccessedResources w/ sensitivity labels, grounding
//     flag. SCAFFOLDED — the subscription/blob flow is documented with a TODO;
//     Rail A ships complete. Complementary, not interchangeable: Rail B = broad
//     governance metadata; Rail A = license-gated actual content.
//
// Metrics normalize to DailyMetric and land in m365_copilot_analytics_daily
// (server migration 018) keyed (day, user_key, surface, app_class, metric). Rail
// A content bodies land in m365_copilot_content, one row per aiInteraction:
// content_hash (content-free) ALWAYS present, content disclosed only behind the
// ADMIN-TIER view gate (a distinct audit row before any read; see content.go),
// mirroring the otel_content message-content viewer. Admin-keyed, server-side
// only — NEVER on the agent wire (this table is polled from Graph, not pushed by
// a node; it is absent from internal/store/orgpush.go by construction, exactly
// like the three sibling analytics tables).
//
// GTM honesty facts (Phase-0b, HARD — reflect these, never soften):
//
//   - aiInteractionHistory is NOT metered today, but this is genuinely
//     undocumented — RE-VERIFY per release. NEVER say "free".
//   - GLOBAL COMMERCIAL CLOUD ONLY. GCC-High, DoD, and 21Vianet are NOT
//     supported — the connector must not claim sovereign-cloud coverage.
//   - Application-only permission AiEnterpriseInteraction.Read.All (delegated is
//     NOT supported); admin consent; User.Read.All may be needed for license
//     lookups. The queried user must hold an M365 Copilot license.
//   - GA on v1.0 (use v1.0, not beta). Copilot Studio agents are EXCLUDED.
//     Delta queries are unsupported. Throttling inherits the Teams Export API
//     limits ($top=100 + exponential backoff on 429/503).
//   - Even official MS telemetry has silent blind spots (the 2025 "Pistachio"
//     AccessedResources incident): content (Rail A) and metadata (Rail B) are
//     not interchangeable, which is why we build both.
//
// Schema confidence: built against the proposal §4.1 documented shapes and the
// public getAllEnterpriseInteractions response structure. The exact office-app
// appClass strings and the Management Activity blob flow carry live-tenant
// residuals flagged inline. Lock both against a real payload from an
// operator-provisioned Entra app + licensed tenant before trusting GTM copy.
package m365copilotanalytics
