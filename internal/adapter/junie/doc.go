// Package junie parses JetBrains Junie session logs.
//
// # The tool
//
// Junie is JetBrains' agentic coding assistant, run as a TUI embedded
// inside JetBrains IDEs (IntelliJ IDEA, PyCharm, etc.). Phase-0 grounding
// ran against two real `hello world`-scale sessions captured on the
// operator's own WSL2 Ubuntu install on 2026-08-16.
//
// # Storage layout
//
//	~/.junie/
//	    settings.json                       model/provider config (fields
//	                                         read; credentials NEVER READ)
//	    secure_credentials.json              auth material     NEVER READ
//	    trust/                               per-project trust decisions
//	                                                            NEVER READ
//	    sessions/
//	        index.jsonl                      one line per session:
//	                                         sessionId/createdAt/updatedAt/
//	                                         projectDir/taskName — used ONLY
//	                                         as a project-root fallback
//	        <session-id>/
//	            events.jsonl                 THE session log — the only
//	                                         file this adapter parses
//	            state.json                   UI-resume snapshot   not parsed
//	            transcript.md                human-readable render, mode
//	                                         600 on real sessions, not parsed
//	            task-<task-id>/.matterhorn/…  internal scratch dirs, not
//	                                         parsed
//
// The session id lives in the enclosing DIRECTORY name, not inside the
// file — sessionIDFromPath recovers it from the path
// (`…/sessions/<session-id>/events.jsonl`) rather than from any record
// field, since no record states its own session id.
//
// # Off-limits files
//
// This adapter reads `events.jsonl` (per session) and, as a fallback only,
// the sibling `index.jsonl`. It NEVER opens:
//
//   - `~/.junie/secure_credentials.json` — auth material.
//   - `~/.junie/trust/` — per-project trust grants.
//   - `~/.junie/settings.json`'s credential fields (its model/provider
//     fields would be harmless to read, but this adapter does not read the
//     file at all — Junie's model id is instead read per-call off
//     `LlmResponseMetadataEvent.modelUsage[].model`, which is more precise
//     since a session can switch models mid-task).
//   - `<session>/state.json`, `<session>/transcript.md`,
//     `<session>/task-*/.matterhorn/…` — UI/scratch state, none of it
//     needed; `transcript.md` in particular is mode 600 on a real
//     install, matching the sensitivity of the events log it's rendered
//     from.
//
// # Record shape
//
// `events.jsonl` is an append-only, event-sourced stream — not a chat
// transcript. Every line is one JSON object discriminated by a top-level
// `kind`:
//
//	UserPromptEvent                  the operator's verbatim prompt
//	TaskStartedEvent                 a task (turn) begins
//	SessionA2uxEvent                 the workhorse — see below
//	UserMessagesCommittedToHistory   correlates prompt ids already
//	                                 captured by UserPromptEvent; no new
//	                                 information, skipped silently
//	TaskState                        session-level state changes
//	                                 ("COMPLETED" observed); SKIPPED (see
//	                                 "Why TaskState is skipped" below)
//
// A `SessionA2uxEvent` wraps the real inner discriminated union one level
// down, at `event.agentEvent.kind` — NOT at the envelope's own top level:
//
//	{"kind":"SessionA2uxEvent","timestampMs":...,
//	 "event":{"state":"IN_PROGRESS","agentEvent":{"kind":"TerminalBlockUpdatedEvent", …}}}
//
// `event.state` ("IN_PROGRESS"/"COMPLETED") is a SIBLING of `agentEvent`,
// tracking the enclosing task's run state, not the block's own status.
// Only on a `ResultBlockUpdatedEvent` envelope, a `completion` object
// appears as a SIBLING OF `event` (not nested inside it):
// `{"event":{...},"completion":{"startedAtMs":...,"endedAtMs":...,"taskCostUsd":...}}`.
//
// 13 distinct `agentEvent.kind` values were observed; 6 have a
// normalized-action counterpart and are acted on (see records.go's kind
// constants). The rest — `AgentCurrentStatusUpdatedEvent`,
// `EnvironmentVariablesUpdatedEvent`, `TipSuggestionCreatedEvent`,
// `AgentTaskNameUpdatedEvent`, `ContextWindowReportEvent`,
// `AgentPatchCreatedEvent`, `NextPromptSuggestionEvent` — are scheduler /
// UI / diagnostic bookkeeping with no counterpart and are skipped
// silently.
//
// # Block collapse by stepId, and the rebroadcast-after-completion finding
//
// Terminal, FileChanges and Result blocks each carry a stable `stepId`
// that recurs across the block's own lifecycle: an `IN_PROGRESS`
// occurrence, then a terminal-status (`COMPLETED`/`FAILED`) occurrence.
// Once the ENCLOSING TASK finishes (`event.state` reaches `COMPLETED`),
// every block belonging to it is re-broadcast ONE MORE TIME, byte-for-byte
// identical except for a several-hundred-millisecond timestamp jitter —
// confirmed against all 6 stepId chains in the Phase-0 fixture
// (`testdata/junie/session-260816-220304-lrfz/events.jsonl`), e.g. the
// Terminal block keyed `62c01dad-eff2-4fcc-bde3-332dde2a43c5` at lines
// 41 -> 45 -> 69 -> 208 (its terminal-status occurrence at line 69 reports
// FAILED). The parser's `stepIdx` map updates the existing ToolEvent row
// in place on every later occurrence of the same stepId, so the
// rebroadcast produces no duplicate row.
//
// # Why TaskState is skipped
//
// An earlier draft of this adapter mapped `TaskState:"COMPLETED"` to a
// session-end marker. That was wrong on two counts: TaskState carries no
// task id of its own (a session can run several tasks/turns in sequence),
// and it fires essentially simultaneously with the matching
// `ResultBlockUpdatedEvent`, which this adapter already turns into an
// `ActionTaskComplete` row per task. Emitting a second, taskless
// session-end row alongside that would be redundant and, on a multi-task
// session, actively misleading (which task ended?). TaskState is now
// skipped entirely — there is no dispatch case for it.
//
// # Why there is no pending.go
//
// Muse's parser defers an unpaired tail record across parse calls via a
// byte-offset rewind (pending.go), because a Muse tool-call record only
// becomes actionable once its LATER result record arrives, and the two
// can straddle a poll boundary.
//
// Junie's block records don't have that shape: every occurrence of a
// stepId — the IN_PROGRESS creation, the terminal-status update, and the
// completion rebroadcast — is a SELF-SUFFICIENT single-line record that
// already carries everything needed (command/output/status/details) to
// stand on its own. Nothing here is ever incomplete pending a later line.
//
// Two duplicate-suppression cases follow from that:
//
//   - IN-WINDOW duplicates (the same stepId recurring within one
//     ParseSessionFile call) are handled in memory via `parseState.stepIdx`,
//     which updates the existing `res.ToolEvents[idx]` row in place.
//   - CROSS-WINDOW duplicates (a stepId whose earlier occurrence was
//     emitted by a PRIOR parse call, e.g. the terminal-status update
//     arriving in a later poll tick than the IN_PROGRESS creation) are
//     handled by simply appending a new row with the SAME
//     SourceEventID — relying on the store's own
//     `ON CONFLICT(source_file, source_event_id) DO UPDATE` self-heal
//     (internal/store's insertActionSQL): `success` flips 1->0 only when
//     the new row also carries a non-empty `error_message` (which a
//     FAILED terminal-status update always does), `duration_ms` only
//     updates 0->nonzero, and `raw_tool_output`/`content_bytes` merge by
//     taking the longer/larger value — exactly the direction every later
//     occurrence of a Junie block moves in (IN_PROGRESS has the least
//     information, the terminal update has more, the rebroadcast is
//     identical to the terminal update). No separate deferral mechanism is
//     needed.
//
// # Tokens: NET input, provider-stated cost
//
// `LlmResponseMetadataEvent.modelUsage[]` carries one entry per model
// invoked to produce a turn. `inputTokens` is already NET of
// `cacheInputTokens` (both observed rows in the Phase-0 capture carried
// `cacheInputTokens: 0`, and JetBrains documents the field as the cache
// portion already reflected in, not additional to, the billed input) — no
// gross-vs-net subtraction is applied, unlike several other adapters in
// this codebase. `cost` is a genuine per-call dollar figure the log
// states directly, unlike Muse (which states none): it is carried
// straight through to `TokenEvent.EstimatedCostUSD`. Reliability is
// `models.ReliabilityAccurate` — the same tag cowork/openclaw/cursor use
// for provider-stated-exact counts — not `ReliabilityApproximate`.
//
// `completion.taskCostUsd` is a separate, TASK-level total (the sum of
// every modelUsage[].cost billed across the whole task); it is NOT added
// to any per-call Cost and is not currently surfaced on any emitted event.
// Only `completion.startedAtMs`/`endedAtMs` feed the Result action's
// DurationMs.
//
// # Project root resolution
//
// Unlike Muse (which states its workspace_root once, on essentially the
// first line, via a dedicated `runtime.session.metadata` record), Junie's
// `CurrentDirectoryUpdatedEvent` was observed roughly a quarter of the way
// into the fixture, and a resumed parse (fromOffset > 0) would never see
// it if the parser only read forward. ParseSessionFile therefore ALWAYS
// re-scans from byte 0 (readHeader / scanCurrentDirectory, bounded to
// headerScanLines) on every call, mirroring Muse's own "always re-read the
// header" convention, before seeking to fromOffset for the real parse
// pass. When no such event is found within the bound (an interrupted
// session with no terminal/file-change block, for instance), the sibling
// `index.jsonl` is consulted by session id as a fallback
// (indexProjectDir).
//
// # Known gaps
//
//   - Only 6 of 13 observed `agentEvent.kind` values have a normalized
//     action; the rest are scheduler/UI bookkeeping (see above).
//   - `UserMessagesCommittedToHistory` and `TaskState` are both skipped —
//     see their sections above.
//   - No proxy route, no MCP entry, no verified interactive-resume/inject
//     contract exists yet for Junie (see
//     internal/integration/integration.go's Capability row); this is a
//     local-capture-only adapter today.
package junie
