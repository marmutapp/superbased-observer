package clinecli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/contentcap"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// userInputPattern matches the `<user_input mode="...">...</user_input>`
// wrapper Cline CLI uses around every operator prompt. Captured at
// `formatUserInputBlock` in @cline/shared. The `mode` attribute is
// `act` / `plan` / `chat`; for v1 we don't surface it (would need an
// ActionMetadata extension), just strip the wrapper.
var userInputPattern = regexp.MustCompile(`(?s)^<user_input(?:\s+mode="[^"]*")?>(.*)</user_input>\s*$`)

// terminalStatus is the set of sessions.status values that signal a
// session is done. Cline CLI strings: per @cline/shared the lifecycle
// transitions through running → idle → completed (or failed /
// cancelled / stopped / aborted). `idle` is intentionally non-terminal
// — the agent's done with its current turn but the session can resume.
var terminalStatus = map[string]string{
	"completed": "completed",
	"failed":    "failed",
	"cancelled": "cancelled",
	"stopped":   "stopped",
	"aborted":   "aborted",
}

// normalizeProjectRoot canonicalises sessions.cwd through the cross-
// mount translator so a Windows-native Cline CLI install observed
// from a WSL2 observer lands on /mnt/c/... rather than the verbatim
// C:\... form that would fail git.Resolve + filepath.Abs (CWD-prefix-
// onto-observer's-own-repo trap; see [[feedback-foreign-path-git-resolve]]).
// No-op on Linux-native paths and on already-translated /mnt/c/... .
func normalizeProjectRoot(cwd string) string {
	if cwd == "" {
		return ""
	}
	return crossmount.TranslateForeignPath(cwd)
}

// actionMetadataFor builds an ActionMetadata carrying the sub-agent
// linkage + team scope for a sessionRow. Returns nil when none of
// the fields would be populated (zero-valued struct would marshal
// to non-NULL "{}" in actions.metadata via the store layer's
// non-nil check — see Invariant #50 / TestActionMetadata_IsZeroCoversEveryField).
//
// Surfaced fields:
//
//   - ParentSessionID / ParentAgentID / AgentID — the parent-child
//     linkage Cline CLI tracks for spawned subagents and teammates.
//     The lead session has nil parent_session_id; teammates and
//     spawn_agent children carry the lead's session id verbatim.
//   - IsSubagent — sessions.is_subagent boolean. The clinecli model
//     distinguishes lead (is_subagent=0) from sub-agent (is_subagent=1)
//     sessions; the dashboard's parent-child grouping keys off this
//     flag.
//   - TeamName — the workspace's team scope (Cline CLI's teams.db
//     namespace). Phase 0 reality-check: team_name can be populated
//     on non-team sessions when the workspace has a team config —
//     surface as observability metadata but DO NOT use it as a
//     subagent signal.
func actionMetadataFor(s *sessionRow) *models.ActionMetadata {
	m := models.ActionMetadata{
		IsSubagent: s.IsSubagent == 1,
	}
	if s.ParentSessionID.Valid {
		m.ParentSessionID = s.ParentSessionID.String
	}
	if s.ParentAgentID.Valid {
		m.ParentAgentID = s.ParentAgentID.String
	}
	if s.AgentID.Valid {
		m.AgentID = s.AgentID.String
	}
	if s.TeamName.Valid {
		m.TeamName = s.TeamName.String
	}
	if m.IsZero() {
		return nil
	}
	return &m
}

// buildEvents converts the parsed session set into the
// adapter.ParseResult-shaped ToolEvent + TokenEvent slices the
// watcher's store layer consumes.
//
// Per session it emits (all idempotent via content-derived
// SourceEventIDs):
//
//   - one ActionSessionStart row (SourceFile = dbPath, SourceEventID
//     = "ss:<sid>") with Target = source ("cli" / "subagent" / "cron")
//   - one ActionSessionEnd row when sessions.status is terminal
//     (SourceEventID = "se:<sid>") with Target = the status
//   - one session-level TokenEvent (SourceFile = dbPath, SourceEventID
//     = "tk:<sid>:session_usage") carrying metadata_json.usage —
//     Tier 2 approximate. Emitted ONLY for terminal-status sessions
//     whose paired messages.json carries no per-message metrics
//     blocks (pre-metrics cline builds): when metrics exist, the
//     per-message TokenEvents ARE the session's usage, and emitting
//     the aggregate too double-counts every token downstream — the
//     two rows differ on BOTH halves of the store's
//     UNIQUE(source_file, source_event_id) key, so the MAX-upgrade
//     conflict path never merges them, and the cost engine's dedup
//     only pairs proxy↔jsonl rows (operator-reported 2026-06-11:
//     every cline-cli session surfaced at exactly 2× its real
//     tokens/cost)
//
// Per message inside the paired messages.json (SourceFile = the
// .messages.json absolute path):
//
//   - user role: one ActionUserPrompt row per text block, with the
//     `<user_input mode="...">` wrapper stripped from Target
//   - assistant role: one ActionTaskComplete row per text block
//     ("assistant_text" — cline-vscode precedent); one ToolEvent
//     per tool_use block dispatched via normalizeToolName; per-
//     message TokenEvent from `metrics` block (Tier 2 per-message
//     — Phase 0 reality-check upgrade vs plan §7's session-level-only
//     claim)
//   - tool_result blocks pair to their owning tool_use by tool_use_id
//     and fill ToolOutput / Success / ErrorMessage on the existing
//     row (no standalone tool_result emission)
//
// Subagent linkage (parent_session_id / agent_id / is_subagent) is
// surfaced on ActionMetadata in commit 7 of the plan §12 build
// order; this commit lays the per-row plumbing without filling it
// in.
//
// Warnings accumulate non-fatal parse hiccups (malformed tool_use
// input, unknown content-block type, mis-paired tool_result) so the
// watcher can log them without aborting the scan.
func buildEvents(
	ctx context.Context,
	sessions []sessionRow,
	dbPath string,
	sc *scrub.Scrubber,
	hookCheck SessionHookChecker,
) (toolEvents []models.ToolEvent, tokenEvents []models.TokenEvent, warnings []string) {
	for i := range sessions {
		s := &sessions[i]
		startedTs := parseIso8601Time(s.StartedAt)
		updatedTs := parseIso8601Time(s.UpdatedAt)
		projectRoot := normalizeProjectRoot(s.CWD)

		// Common per-session metadata carrying the sub-agent + team
		// linkage. nil when none of the fields would populate.
		meta := actionMetadataFor(s)

		// Cross-path dedup gate: when a hook checker is wired in AND
		// it reports this session is fully covered by hook-emitted
		// rows, skip the SQLite-path emit entirely. Avoids double-
		// counting per-turn rows (the H1 finding from the hermes
		// audit applies identically here — both paths emit ToolEvents
		// with distinct SourceFile values, so the
		// UNIQUE(source_file, source_event_id) constraint doesn't
		// catch the cross-path duplicate). On checker error: log a
		// warning + fall back to emitting (safer to over-count than
		// silently drop).
		if hookCheck != nil {
			hooked, err := hookCheck(ctx, s.ID)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("clinecli.buildEvents: hookCheck for session %s failed (%v); falling back to SQLite emission", s.ID, err))
			} else if hooked {
				continue
			}
		}

		// ----- session_start ---------------------------------------
		toolEvents = append(toolEvents, sessionStartEvent(s, dbPath, projectRoot, startedTs, meta))

		// ----- session_end (status-terminal sessions only) ---------
		if mapped, ok := terminalStatus[s.Status]; ok {
			endTs := updatedTs
			if s.EndedAt.Valid {
				if t := parseIso8601Time(s.EndedAt.String); !t.IsZero() {
					endTs = t
				}
			}
			toolEvents = append(toolEvents, sessionEndEvent(s, dbPath, projectRoot, endTs, mapped, meta))
		}

		// ----- session-level token aggregate -----------------------
		// Fallback for sessions whose messages.json carries no
		// per-message metrics blocks — when metrics exist, the
		// per-message rows below are the session's usage and the
		// aggregate would double-count (see the buildEvents doc).
		// Terminal-status gate: the aggregate lands once, after the
		// session settles, so a mid-session scan racing the first
		// metrics flush into messages.json can't strand a permanent
		// duplicate row (re-parses can't retract an emitted event).
		if _, terminal := terminalStatus[s.Status]; terminal && !hasPerMessageMetrics(s) {
			if u := s.Metadata.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
				tokenEvents = append(tokenEvents, sessionTokenEvent(s, dbPath, projectRoot, updatedTs, u))
			}
		}

		// ----- per-message events ----------------------------------
		msgPath := resolveMessagesPath(s, dbPath)
		if msgPath == "" || len(s.Messages.Messages) == 0 {
			continue
		}
		pendingByCallID := map[string]int{}
		for mi := range s.Messages.Messages {
			m := &s.Messages.Messages[mi]
			ts := unixMilliToTime(m.Ts)
			if ts.IsZero() {
				ts = updatedTs
			}

			switch m.Role {
			case "user":
				// User-role messages carry either inbound prompts
				// (text blocks; one or more `<user_input>` wrappers)
				// or paired tool_result blocks. We handle both —
				// text blocks become ActionUserPrompt rows; tool_result
				// blocks lookup-and-fill the matching tool_use row.
				userMsgEvents := walkUserMessage(s, m, msgPath, projectRoot, ts, sc, pendingByCallID, toolEvents, meta)
				toolEvents = userMsgEvents
			case "assistant":
				// Per-message TokenEvent from metrics (Phase 0
				// reality-check find). Skip when no metrics block.
				if m.Metrics != nil {
					tokenEvents = append(tokenEvents, perMessageTokenEvent(s, m, msgPath, projectRoot, ts))
				}
				asstEvents, asstWarnings := walkAssistantMessage(s, m, msgPath, projectRoot, ts, sc, pendingByCallID, len(toolEvents), meta)
				toolEvents = append(toolEvents, asstEvents...)
				warnings = append(warnings, asstWarnings...)
			default:
				warnings = append(warnings, fmt.Sprintf("clinecli.buildEvents: session=%s message=%s unknown role %q", s.ID, m.ID, m.Role))
			}
		}
	}
	return toolEvents, tokenEvents, warnings
}

// resolveMessagesPath returns the path to use as SourceFile on
// per-message rows. Prefers sessions.messages_path verbatim (cross-
// mount-translated) so the dashboard's drill-down "where did this
// event come from" points at the canonical on-disk file. Falls back
// to the derived `<rootDir>/sessions/<id>/<id>.messages.json` shape
// when the column is empty, mirroring loadMessagesJSON's resolution.
func resolveMessagesPath(s *sessionRow, dbPath string) string {
	if s.MessagesPath.Valid && s.MessagesPath.String != "" {
		return crossmount.TranslateForeignPath(s.MessagesPath.String)
	}
	root := deriveRootDir(dbPath)
	if root == "" {
		return ""
	}
	return root + "/sessions/" + s.ID + "/" + s.ID + ".messages.json"
}

// sessionStartEvent emits the lifecycle row for a session's start.
// SourceFile = the dbPath because the row derives from sessions.db,
// not from the per-session JSON.
func sessionStartEvent(s *sessionRow, dbPath, projectRoot string, ts time.Time, meta *models.ActionMetadata) models.ToolEvent {
	if ts.IsZero() {
		ts = parseIso8601Time(s.UpdatedAt)
	}
	return models.ToolEvent{
		SourceFile:    dbPath,
		SourceEventID: "ss:" + s.ID,
		SessionID:     s.ID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		Model:         s.Model,
		Tool:          models.ToolClineCLI,
		ActionType:    models.ActionSessionStart,
		Target:        s.Source,
		Success:       true,
		RawToolName:   "clinecli.session_start",
		Metadata:      meta,
	}
}

// sessionEndEvent emits the lifecycle row for a session reaching a
// terminal status. Status maps directly to Target so the dashboard
// can surface the variant (completed / failed / cancelled / …).
func sessionEndEvent(s *sessionRow, dbPath, projectRoot string, ts time.Time, status string, meta *models.ActionMetadata) models.ToolEvent {
	if ts.IsZero() {
		ts = parseIso8601Time(s.UpdatedAt)
	}
	return models.ToolEvent{
		SourceFile:    dbPath,
		SourceEventID: "se:" + s.ID,
		SessionID:     s.ID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		Model:         s.Model,
		Tool:          models.ToolClineCLI,
		ActionType:    models.ActionSessionEnd,
		Target:        status,
		Success:       status == "completed",
		RawToolName:   "clinecli.session_end",
		Metadata:      meta,
	}
}

// hasPerMessageMetrics reports whether any assistant message in the
// session's paired messages.json carries a metrics block — i.e.
// whether buildEvents will emit per-message TokenEvents for this
// session. Gates the session-aggregate fallback: aggregate and
// per-message rows must never coexist for one session (their
// SourceEventIDs differ, so the store's UNIQUE conflict path can't
// merge them and every downstream SUM would read 2×).
func hasPerMessageMetrics(s *sessionRow) bool {
	for i := range s.Messages.Messages {
		m := &s.Messages.Messages[i]
		if m.Role == "assistant" && m.Metrics != nil {
			return true
		}
	}
	return false
}

// sessionTokenEvent emits the session-level usage aggregate row from
// metadata_json.usage. Tier 2 approximate. Emitted only when the
// session has no per-message metrics rows (see buildEvents /
// hasPerMessageMetrics) — for metrics-less sessions this is the only
// token signal; for current cline builds the per-message rows
// supersede it entirely.
func sessionTokenEvent(s *sessionRow, dbPath, projectRoot string, ts time.Time, u metadataUsage) models.TokenEvent {
	if ts.IsZero() {
		ts = parseIso8601Time(s.UpdatedAt)
	}
	return models.TokenEvent{
		SourceFile:          dbPath,
		SourceEventID:       "tk:" + s.ID + ":session_usage",
		SessionID:           s.ID,
		ProjectRoot:         projectRoot,
		Timestamp:           ts,
		Tool:                models.ToolClineCLI,
		Model:               s.Model,
		InputTokens:         u.InputTokens,
		OutputTokens:        u.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens,
		CacheCreationTokens: u.CacheWriteTokens,
		EstimatedCostUSD:    u.TotalCost,
		Source:              models.TokenSourceJSONL,
		Reliability:         models.ReliabilityApproximate,
	}
}

// perMessageTokenEvent emits a TokenEvent for one assistant message's
// metrics block. Phase 0 reality-check upgrade: Cline CLI carries
// per-message {inputTokens, outputTokens, cacheReadTokens,
// cacheWriteTokens, cost} on every assistant message in messages.json,
// finer-grained than the session-level metadata_json.usage.
//
// SourceFile = the messages.json path (so re-reads of the same file
// dedup correctly via UNIQUE(source_file, source_event_id)). Model
// preferred from m.modelInfo.id when present, falling back to the
// session-level model.
func perMessageTokenEvent(s *sessionRow, m *messageRecord, msgPath, projectRoot string, ts time.Time) models.TokenEvent {
	model := s.Model
	if m.ModelInfo != nil && m.ModelInfo.ID != "" {
		model = m.ModelInfo.ID
	}
	return models.TokenEvent{
		SourceFile:          msgPath,
		SourceEventID:       "m" + m.ID + ":metrics",
		SessionID:           s.ID,
		ProjectRoot:         projectRoot,
		Timestamp:           ts,
		Tool:                models.ToolClineCLI,
		Model:               model,
		InputTokens:         m.Metrics.InputTokens,
		OutputTokens:        m.Metrics.OutputTokens,
		CacheReadTokens:     m.Metrics.CacheReadTokens,
		CacheCreationTokens: m.Metrics.CacheWriteTokens,
		EstimatedCostUSD:    m.Metrics.Cost,
		Source:              models.TokenSourceJSONL,
		Reliability:         models.ReliabilityApproximate,
		MessageID:           m.ID,
	}
}

// walkUserMessage handles role='user' content blocks. Text blocks
// emit ActionUserPrompt rows (with the `<user_input>` wrapper
// stripped from Target); tool_result blocks lookup-and-fill the
// matching tool_use row via pendingByCallID and DO NOT emit
// separately.
//
// Returns the (possibly-mutated) toolEvents slice — tool_result
// pairing edits existing rows in place; new user_prompt rows are
// appended.
func walkUserMessage(
	s *sessionRow,
	m *messageRecord,
	msgPath, projectRoot string,
	ts time.Time,
	sc *scrub.Scrubber,
	pendingByCallID map[string]int,
	toolEvents []models.ToolEvent,
	meta *models.ActionMetadata,
) []models.ToolEvent {
	for bi, b := range m.Content {
		switch b.Type {
		case "text":
			body := stripUserInputWrapper(b.Text)
			if strings.TrimSpace(body) == "" {
				continue
			}
			toolEvents = append(toolEvents, userPromptEvent(s, m, msgPath, projectRoot, ts, bi, body, sc, meta))
		case "tool_result":
			idx, ok := pendingByCallID[b.ToolUseID]
			if !ok {
				continue
			}
			output, success := decodeToolResultContent(b)
			scrubbed := sc.String(output)
			toolEvents[idx].ToolOutput = scrubbed
			if !success || b.IsError {
				toolEvents[idx].Success = false
				toolEvents[idx].ErrorMessage = truncate(scrubbed, 2048)
			}
			delete(pendingByCallID, b.ToolUseID)
		}
	}
	return toolEvents
}

// walkAssistantMessage handles role='assistant' content blocks.
// Text blocks emit ActionTaskComplete ("assistant_text") rows; tool_use
// blocks emit ToolEvents with normalized ActionType + extracted target;
// thinking blocks are skipped (Anthropic-extended-thinking trace —
// observability-only; the dashboard surfaces them via the parent
// message's PrecedingReasoning when needed).
//
// `firstIdx` is the index into the OUTER toolEvents slice where this
// message's first event will land. Used to populate pendingByCallID
// so subsequent user-message tool_result blocks can find this
// message's tool_use rows by call_id.
//
// Returns the new events to append + any warnings.
func walkAssistantMessage(
	s *sessionRow,
	m *messageRecord,
	msgPath, projectRoot string,
	ts time.Time,
	sc *scrub.Scrubber,
	pendingByCallID map[string]int,
	firstIdx int,
	meta *models.ActionMetadata,
) (events []models.ToolEvent, warnings []string) {
	for bi, b := range m.Content {
		switch b.Type {
		case "text":
			body := strings.TrimSpace(b.Text)
			if body == "" {
				continue
			}
			events = append(events, assistantTextEvent(s, m, msgPath, projectRoot, ts, bi, body, sc, meta))
		case "thinking":
			body := strings.TrimSpace(b.Thinking)
			if body == "" {
				continue
			}
			events = append(events, reasoningEvent(s, m, msgPath, projectRoot, ts, bi, body, sc, meta))
		case "tool_use":
			evt := toolUseEvent(s, m, msgPath, projectRoot, ts, b, sc, meta)
			pendingByCallID[b.ID] = firstIdx + len(events)
			events = append(events, evt)
		case "":
			warnings = append(warnings, fmt.Sprintf("clinecli.walkAssistantMessage: session=%s message=%s block=%d empty type", s.ID, m.ID, bi))
		default:
			// Unknown block types are common when Cline CLI versions
			// roll forward with new content shapes — log + skip.
			warnings = append(warnings, fmt.Sprintf("clinecli.walkAssistantMessage: session=%s message=%s block=%d unknown type %q", s.ID, m.ID, bi, b.Type))
		}
	}
	return events, warnings
}

// stripUserInputWrapper removes the `<user_input mode="...">…</user_input>`
// wrapper Cline CLI uses around operator prompts. Returns the inner
// text verbatim when matched; the original string when no wrapper
// present.
func stripUserInputWrapper(s string) string {
	m := userInputPattern.FindStringSubmatch(s)
	if len(m) < 2 {
		return s
	}
	return m[1]
}

// toolResultContentItem is one element of tool_result.content — the
// structured-dict shape Phase 0 found (plan §4 said "structured JSON"
// without enumerating the per-item keys).
type toolResultContentItem struct {
	Query   string `json:"query,omitempty"`
	Result  string `json:"result,omitempty"`
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

// decodeToolResultContent extracts a human-readable output blob +
// overall success flag from a tool_result block. Cline CLI tool_result
// content is a LIST of {query, result, success, error} dicts (Phase 0
// reality-check) — joins all `result` fields into one body string and
// ANDs all `success` flags. Falls back to a bare string decode for
// future Cline CLI versions that use a different shape.
func decodeToolResultContent(b messageBlock) (output string, success bool) {
	if len(b.Content) == 0 {
		return "", !b.IsError
	}
	// First try the structured-list shape.
	var items []toolResultContentItem
	if err := json.Unmarshal(b.Content, &items); err == nil && len(items) > 0 {
		var out strings.Builder
		allOK := true
		for i, it := range items {
			if i > 0 {
				out.WriteByte('\n')
			}
			if it.Result != "" {
				out.WriteString(it.Result)
			}
			if it.Error != "" {
				if out.Len() > 0 {
					out.WriteByte('\n')
				}
				out.WriteString(it.Error)
			}
			if !it.Success {
				allOK = false
			}
		}
		return out.String(), allOK && !b.IsError
	}
	// Fallback: bare string content.
	var s string
	if err := json.Unmarshal(b.Content, &s); err == nil {
		return s, !b.IsError
	}
	// Last-ditch: dump the raw JSON.
	return string(b.Content), !b.IsError
}

// userPromptEvent emits one ActionUserPrompt row per text block on a
// role='user' message. Target carries the body preview; RawToolInput
// the full scrubbed body. MessageID anchors the row to the parent
// message for cross-row dedup.
func userPromptEvent(
	s *sessionRow,
	m *messageRecord,
	msgPath, projectRoot string,
	ts time.Time,
	blockIdx int,
	body string,
	sc *scrub.Scrubber,
	meta *models.ActionMetadata,
) models.ToolEvent {
	scrubbed := sc.String(body)
	return models.ToolEvent{
		SourceFile:    msgPath,
		SourceEventID: "m" + m.ID + ":user:" + fmt.Sprintf("%d", blockIdx),
		SessionID:     s.ID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		Model:         s.Model,
		Tool:          models.ToolClineCLI,
		ActionType:    models.ActionUserPrompt,
		Target:        truncate(scrubbed, 200),
		Success:       true,
		RawToolName:   "user_message",
		RawToolInput:  sc.String(contentcap.Cap(body, contentcap.DefaultMaxBytes)),
		MessageID:     "user:" + m.ID,
		Metadata:      meta,
	}
}

// assistantTextEvent emits one ActionTaskComplete row per text block
// on a role='assistant' message. Same convention as the cline-vscode
// adapter's assistantTextEvent + the hermes V3 fix
// ("<tool>.assistant_response" / "<tool>.assistant_text" RawToolName).
func assistantTextEvent(
	s *sessionRow,
	m *messageRecord,
	msgPath, projectRoot string,
	ts time.Time,
	blockIdx int,
	body string,
	sc *scrub.Scrubber,
	meta *models.ActionMetadata,
) models.ToolEvent {
	preview := truncate(sc.String(body), 200)
	hash := shortHash(body)
	model := s.Model
	if m.ModelInfo != nil && m.ModelInfo.ID != "" {
		model = m.ModelInfo.ID
	}
	return models.ToolEvent{
		SourceFile:         msgPath,
		SourceEventID:      "m" + m.ID + ":asst:" + fmt.Sprintf("%d", blockIdx) + ":" + hash,
		SessionID:          s.ID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		Model:              model,
		Tool:               models.ToolClineCLI,
		ActionType:         models.ActionAssistantMessage,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "clinecli.assistant_text",
		ToolOutput:         sc.String(contentcap.Cap(body, contentcap.DefaultMaxBytes)),
		MessageID:          "asst:" + m.ID,
		Metadata:           meta,
	}
}

// reasoningEvent shapes a `thinking` block into a standalone, visible
// reasoning row (was dropped as "observability-only") so the model's
// chain-of-thought is in the timeline, matching the cross-adapter
// convention (cline/gemini/cowork/openclaw/…).
func reasoningEvent(
	s *sessionRow,
	m *messageRecord,
	msgPath, projectRoot string,
	ts time.Time,
	blockIdx int,
	thinking string,
	sc *scrub.Scrubber,
	meta *models.ActionMetadata,
) models.ToolEvent {
	preview := truncate(sc.String(thinking), 200)
	hash := shortHash(thinking)
	model := s.Model
	if m.ModelInfo != nil && m.ModelInfo.ID != "" {
		model = m.ModelInfo.ID
	}
	return models.ToolEvent{
		SourceFile:         msgPath,
		SourceEventID:      "m" + m.ID + ":reasoning:" + fmt.Sprintf("%d", blockIdx) + ":" + hash,
		SessionID:          s.ID,
		ProjectRoot:        projectRoot,
		Timestamp:          ts,
		Model:              model,
		Tool:               models.ToolClineCLI,
		ActionType:         models.ActionTaskComplete,
		Target:             preview,
		Success:            true,
		PrecedingReasoning: preview,
		RawToolName:        "clinecli.reasoning",
		ToolOutput:         sc.String(contentcap.Cap(thinking, contentcap.DefaultMaxBytes)),
		MessageID:          "asst:" + m.ID,
		Metadata:           meta,
	}
}

// toolUseEvent emits one ToolEvent per tool_use block. ActionType
// comes from normalizeToolName; Target is extracted per-tool from the
// input args (when the tool has a canonical target field).
//
// Success defaults to true here; the paired tool_result block on a
// subsequent role='user' message lookup-and-fills ToolOutput +
// (when is_error / success=false) Success + ErrorMessage.
func toolUseEvent(
	s *sessionRow,
	m *messageRecord,
	msgPath, projectRoot string,
	ts time.Time,
	b messageBlock,
	sc *scrub.Scrubber,
	meta *models.ActionMetadata,
) models.ToolEvent {
	actionType := normalizeToolName(b.Name)
	scrubbedInput := sc.RawJSON(b.Input)
	target := extractTarget(b.Name, b.Input, sc)
	model := s.Model
	if m.ModelInfo != nil && m.ModelInfo.ID != "" {
		model = m.ModelInfo.ID
	}
	return models.ToolEvent{
		SourceFile:    msgPath,
		SourceEventID: "m" + m.ID + ":tool_use:" + b.ID,
		SessionID:     s.ID,
		ProjectRoot:   projectRoot,
		Timestamp:     ts,
		Model:         model,
		Tool:          models.ToolClineCLI,
		ActionType:    actionType,
		Target:        target,
		Success:       true,
		RawToolName:   b.Name,
		RawToolInput:  firstNonEmpty(scrubbedInput, scrub.Truncate(string(b.Input))),
		MessageID:     "asst:" + m.ID,
		Metadata:      meta,
	}
}

// extractTarget pulls the user-visible target string out of a
// tool_use's input args. Per-tool: read_files / run_commands /
// apply_patch all carry batched inputs (the plural names!) so we
// summarise the first item + count.
func extractTarget(toolName string, rawInput json.RawMessage, sc *scrub.Scrubber) string {
	if len(rawInput) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(rawInput, &m); err != nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	pickList := func(key, itemKey string) (first string, count int) {
		raw, ok := m[key]
		if !ok {
			return "", 0
		}
		list, ok := raw.([]any)
		if !ok {
			return "", 0
		}
		count = len(list)
		if count == 0 {
			return "", 0
		}
		head, ok := list[0].(map[string]any)
		if !ok {
			return "", count
		}
		if s, ok := head[itemKey].(string); ok {
			return s, count
		}
		return "", count
	}

	switch toolName {
	case "read_files":
		first, n := pickList("files", "path")
		if n == 0 {
			return ""
		}
		if n == 1 {
			return first
		}
		return fmt.Sprintf("%s (+%d more)", first, n-1)
	case "run_commands":
		first, n := pickList("commands", "command")
		if n == 0 {
			return ""
		}
		if n == 1 {
			return sc.String(first)
		}
		return sc.String(fmt.Sprintf("%s (+%d more)", first, n-1))
	case "apply_patch":
		first, n := pickList("patches", "path")
		if n == 0 {
			// apply_patch with single {path, old_string, new_string}
			return pick("path", "file_path")
		}
		if n == 1 {
			return first
		}
		return fmt.Sprintf("%s (+%d more)", first, n-1)
	case "editor":
		return pick("path", "file_path")
	case "search_codebase":
		return pick("pattern", "query", "regex")
	case "fetch_web_content":
		return pick("url")
	case "ask_question":
		return sc.String(pick("question"))
	case "spawn_agent":
		return sc.String(pick("task", "prompt"))
	case "submit_and_exit":
		return sc.String(pick("summary", "result"))
	case "team_send_message", "team_broadcast":
		return pick("to", "team_name", "recipient")
	case "team_spawn_teammate":
		return pick("name", "agent_id")
	case "team_run_task", "team_task":
		return pick("task_id", "title")
	}
	return ""
}

// --- helpers ---------------------------------------------------------

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func parseIso8601Time(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Time{}
}

func unixMilliToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
