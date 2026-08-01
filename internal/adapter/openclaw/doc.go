// Package openclaw parses OpenClaw's local agent state into observer
// ToolEvents / TokenEvents.
//
// # Storage layout
//
// Three formats, one adapter (dispatched in ParseSessionFile):
//
//	~/.openclaw/tasks/runs.sqlite                        task lifecycle rows
//	~/.openclaw/agents/<agent>/sessions/sessions.json    the session INDEX
//	~/.openclaw/agents/<agent>/sessions/<id>.jsonl       the message log
//	~/.openclaw/agents/<agent>/sessions/<id>.trajectory.jsonl  the trace
//
// A single run therefore writes TWO files that both describe it: the message
// log (`<id>.jsonl`) and the trajectory trace (`<id>.trajectory.jsonl`). They
// are parsed by separate ParseSessionFile calls, so everything below exists
// to keep the two halves landing on ONE observer session and counting each
// model call ONCE.
//
// # Session identity
//
// sessions.json is the canonical owner of the session key. Its MAP KEY (e.g.
// "agent:main:explicit:wpt6probe") is the id both paths must produce; the
// per-entry `sessionId` ("wpt6probe") is the file stem and is NOT it.
//
//   - Message-log path: applySessionAlias / lookupSessionAlias reads
//     sessions.json and rewrites the state's session id to the map key.
//   - Trajectory path: OpenClaw stamps the same key onto every trace event as
//     `sessionKey`, so trajectorySessionID prefers that, and otherwise routes
//     the event's `sessionId` through the same applySessionAlias owner.
//
// WP-T6 finding O1 was the trajectory path taking the event's raw `sessionId`
// verbatim, which split one run into two observer sessions
// ("agent:main:explicit:wpt6probe" with the actions, bare "wpt6probe" with a
// duplicate token row). ProjectRoot split the same way; it now falls back to
// the trace's own `workspaceDir` when the alias lookup finds nothing, instead
// of the "[openclaw]" placeholder.
//
// # Tokens — which file wins, and why
//
// Both files carry provider usage, at different granularity:
//
//   - `<id>.jsonl` records `message.usage` on EVERY assistant message, i.e.
//     every model call of the run.
//   - `<id>.trajectory.jsonl` emits ONE `model.completed` per run whose
//     `data.promptCache.lastCallUsage` describes only that run's LAST call
//     (`data.usage` is a running session total and is deliberately ignored).
//
// On the 2026-07-31 live capture the run produced five message-log usage
// records and one model.completed. The trajectory's lastCallUsage was
// byte-identical to the fifth message-log record (368/542/14848/0) and the
// five records summed exactly to the trajectory's session total (16139 in /
// 1076 out / 58368 cacheRead). So the trajectory is a strict SUBSET, not a
// better source — deferring to it would have dropped four of five turns.
//
// Dedup therefore runs the other way: parseTrajectoryJSONL SUPPRESSES a
// model.completed whose call the message log already covers. The join key is
// the model call's epoch-ms timestamp — `data.messagesSnapshot`'s last
// usage-bearing assistant entry (fallback:
// `data.promptCache.lastCacheTouchAt`) versus the message log's
// `message.timestamp`; the two matched exactly on both live traces. The check
// reads the sibling file from DISK, so it does not depend on which file the
// watcher parses first.
//
// The store cannot do this for us. token_usage's UNIQUE(source_file,
// source_event_id) plus its ON CONFLICT MAX-upgrade only collapse rows that
// share BOTH columns, and these rows share neither; the cross-source-file
// sweeps in store.InsertTokenEvents are explicit per-tool allow-lists
// (copilot-cli, claude-code, codex) that openclaw is deliberately not on.
// Adapter-side suppression is the only mechanism the store's semantics
// support here.
//
// Gateway-injected turns (model="gateway-injected", every usage field 0) are
// the case the trajectory tier exists for: the shared hasUsage predicate
// drops them on the message-log side AND leaves them uncovered on the dedup
// side, so the trajectory row survives as their only source. The message
// log's limitation is coverage, not precision — both paths emit
// ReliabilityAccurate, because both read the same provider usage object.
//
// Write ORDER cannot defeat the dedup, and the residual that can. OpenClaw
// persists an assistant message with appendFileSync the moment its
// message_end fires (pi-coding-agent SessionManager._persist), while
// model.completed is recorded at RUN end through an async queued writer
// (dist/selection-*.js) — on the live 2026-07-31 trace the event landed
// 11:50:30.085Z for a call whose message-log record was already at
// 11:50:25.318Z. So the message-log bytes are always on disk first, and
// because messageLogUsageTimestamps re-reads the sibling from offset 0 the
// watcher's own parse order is irrelevant. The ONE way a call gets two rows
// is a sibling that is no longer readable when the trajectory is (re-)parsed:
// `openclaw doctor` archives orphaned logs to `<id>.jsonl.deleted.<ts>` (and
// session reset to `.reset.<ts>`), after which a forced `observer scan
// --force` re-emits the model.completed the archived log had already covered.
// Consulting those archives would close it but would INVERT the adapter's
// stated policy — a log archived before observer ever parsed it would then
// silently drop its turns instead of duplicating one — so the fail-open
// stands deliberately: better a rare duplicate than a silent loss.
//
// # Known limitation — historical split sessions
//
// Rows ingested BEFORE the O1 fix keep the split: token rows written under
// the bare stem (e.g. "wpt6probe", "4de1bfff-50c3-…") stay on their own
// sessions row, separate from the aliased session that holds the actions. No
// repair migration ships this round. A repair is a pure alias join — for each
// sessions row whose id equals some sessions.json entry's `sessionId`, and
// where a sessions row named by that entry's MAP KEY also exists, re-point
// the orphan's token_usage/actions to the canonical id and drop the orphan —
// but it needs sessions.json (node-local, possibly rotated) to be readable at
// migration time, so it is deliberately deferred rather than guessed at.
// Trajectory SourceEventIDs are keyed on the FILE STEM rather than the
// resolved session id, so a rescan after the fix MAX-upgrades the pre-fix row
// in place instead of adding a third one.
//
// # User prompts and the bootstrap preamble
//
// On a bootstrap-pending workspace OpenClaw prepends a ~700-char harness
// preamble to the operator's prompt, in ONE text part, so actions.target used
// to show harness boilerplate (WP-T6 finding O2). splitBootstrapPrompt
// recovers the human half; its boundary is grounded verbatim in OpenClaw's
// own prompt assembly (`${userPromptPrefixText}\n\n${effectivePrompt}` where
// the prefix is a "\n"-join of non-empty lines starting "[Bootstrap
// pending]"), not in a byte offset. RawToolInput keeps the full text, so the
// preamble is preserved rather than discarded.
//
// The marker is only a pre-filter. The split point is CORROBORATED against
// the run's own preamble string, which OpenClaw echoes into the sibling
// trajectory as trace.metadata data.prompting.userPromptPrefixText — with it
// the text must literally start with `prefix + "\n\n"`, so a human prompt
// that merely opens "[Bootstrap pending]" and contains a blank line keeps its
// first paragraph. When no trajectory is readable (rotated away,
// OPENCLAW_TRAJECTORY=0, pre-metadata build) the original marker +
// first-"\n\n" heuristic stands, and its residual false positive is exactly
// that mimicking prompt — preview-only, never the stored RawToolInput.
package openclaw
