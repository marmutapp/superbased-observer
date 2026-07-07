package antigravity

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// parseMarkdownConversation walks the Markdown returned by
// ConvertTrajectoryToMarkdown and emits ToolEvent records.
//
// Antigravity's Markdown structure (observed):
//
//	# Chat Conversation
//	(blurb)
//	### User Input
//	  <user prompt body>
//	### Planner Response
//	  <assistant body>
//	  *Viewed [filename](file://...)*       ← inline tool action
//	  *Searched filesystem*                 ← search tool
//	  *Grep searched codebase*              ← grep
//	  *Edited file [filename](file://...)*  ← edit
//	  *Ran command `cmd`*                   ← run command
//	### User Input
//	  ...
//
// We emit:
//   - one ActionUserPrompt per "### User Input" block
//   - one ActionTaskComplete per "### Planner Response" block
//     (the Markdown body becomes the `ToolOutput`)
//   - inline tool actions parsed from `*<verb> ...*` lines as
//     normalized actions where the verb maps cleanly. Actions that
//     don't map are skipped — the conversation flow is preserved
//     in the Planner Response body.
//
// Reliability: `approximate` (we have the conversation flow but no
// per-tool args, no token counts). Token rows are NOT emitted from
// the Markdown — we only get them from the original .pb decryption
// path (which currently fails on Windows).
//
// skipInlineTools suppresses the `*Edited file…*` / `*Viewed…*` /
// `*Searched…*` extraction inside Planner Response bodies. Set to
// true when the caller has already gathered authoritative tool
// events from the structured trajectory (Tier 1 of the deep-
// extraction handoff); set to false when running the markdown path
// alone. The user/assistant text rows still emit either way.
//
// skipUserInputs suppresses `### User Input` emission. Set to true
// when the caller has structured user_prompt rows from 1.2.19.2
// (Tier 2). Markdown's user_input is deduplicate against structured
// — the structured rows carry deterministic SourceEventIDs tied to
// real per-step timestamps.
//
// skipPlannerResponse suppresses `### Planner Response` emission.
// Set to true when the caller has structured assistant_text rows
// from 1.2.20.1 (Tier 3, enum=15 = PLANNER_RESPONSE). Same dedup
// logic as skipUserInputs.
func parseMarkdownConversation(path, conversationID, projectRoot string, ts time.Time, scrubber stringScrubber, markdown string, skipInlineTools, skipUserInputs, skipPlannerResponse bool) adapter.ParseResult {
	res := adapter.ParseResult{}
	if conversationID == "" {
		conversationID = "antigravity-unknown"
	}
	sessionID := conversationID

	sections := splitSections(markdown)
	turnIdx := 0
	for _, sec := range sections {
		header := strings.TrimSpace(sec.header)
		body := strings.TrimSpace(sec.body)
		switch {
		case header == "" && strings.HasPrefix(strings.TrimSpace(sec.body), "# Chat Conversation"):
			// Top-of-file blurb; skip.
			continue
		case strings.EqualFold(header, "### User Input"):
			if body == "" {
				continue
			}
			if skipUserInputs {
				turnIdx++
				continue
			}
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:    path,
				SourceEventID: stableMarkdownID("md-user", path, conversationID, turnIdx),
				SessionID:     sessionID,
				ProjectRoot:   projectRoot,
				Timestamp:     bumpTimestamp(ts, turnIdx),
				Tool:          models.ToolAntigravity,
				ActionType:    models.ActionUserPrompt,
				Target:        truncate(body, 200),
				Success:       true,
				RawToolName:   "markdown.user_input",
				RawToolInput:  scrubber.String(body),
				MessageID:     "antigravity-md:" + conversationID + ":user:" + intStr(turnIdx),
			})
			turnIdx++
		case strings.EqualFold(header, "### Planner Response"):
			if body == "" {
				continue
			}
			// Extract inline tool calls before emitting the body event.
			// When the caller has structured tool events (Tier 1
			// dedup), strip the inline lines from the body but skip
			// emitting them as their own ToolEvents.
			toolEvents, remainingBody := extractInlineTools(path, conversationID, projectRoot, sessionID, ts, turnIdx, scrubber, body)
			if !skipInlineTools {
				res.ToolEvents = append(res.ToolEvents, toolEvents...)
			}
			if skipPlannerResponse {
				turnIdx++
				continue
			}

			// Emit the assistant turn as task_complete.
			res.ToolEvents = append(res.ToolEvents, models.ToolEvent{
				SourceFile:         path,
				SourceEventID:      stableMarkdownID("md-assistant", path, conversationID, turnIdx),
				SessionID:          sessionID,
				ProjectRoot:        projectRoot,
				Timestamp:          bumpTimestamp(ts, turnIdx),
				Tool:               models.ToolAntigravity,
				ActionType:         models.ActionTaskComplete,
				Target:             truncate(remainingBody, 200),
				Success:            true,
				RawToolName:        "markdown.planner_response",
				PrecedingReasoning: truncate(remainingBody, 200),
				ToolOutput:         scrubber.String(remainingBody),
				MessageID:          "antigravity-md:" + conversationID + ":assistant:" + intStr(turnIdx),
			})
			turnIdx++
		}
	}
	return res
}

// section is one Markdown header + the body that follows it.
type section struct {
	header string
	body   string
}

// splitSections splits Markdown into (header, body) pairs based on
// h1-h3 headers. Lines before the first header become the synthetic
// "" header section.
func splitSections(md string) []section {
	var out []section
	var current section
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "#") && strings.Contains(line, " ") {
			out = append(out, current)
			current = section{header: line}
			continue
		}
		current.body += line + "\n"
	}
	out = append(out, current)
	return out
}

// inlineToolPattern matches Antigravity's italicized inline-action
// lines. Examples:
//
//	*Viewed [ai_api_specs.md](file:///c:/...)*
//	*Searched filesystem*
//	*Grep searched codebase*
//	*Edited file [main.go](file:///...)*
//	*Ran command ` + "`" + `npm test` + "`" + `*
var inlineToolPattern = regexp.MustCompile(`\*([^*]{2,200})\*`)

// extractInlineTools walks the Planner Response body for `*verb...*`
// lines and emits ToolEvents for each recognized action. Returns
// the events and the body with the inline lines removed (so the
// task_complete row's text doesn't double-count them).
func extractInlineTools(path, conversationID, projectRoot, sessionID string, ts time.Time, turnIdx int, scrubber stringScrubber, body string) ([]models.ToolEvent, string) {
	var events []models.ToolEvent
	matches := inlineToolPattern.FindAllStringSubmatchIndex(body, -1)
	if len(matches) == 0 {
		return events, body
	}
	out := body
	idx := 0
	for _, m := range matches {
		raw := body[m[2]:m[3]]
		actionType, target := classifyInlineTool(raw)
		if actionType == "" {
			continue
		}
		events = append(events, models.ToolEvent{
			SourceFile:    path,
			SourceEventID: stableMarkdownID("md-tool", path, conversationID, turnIdx, idx),
			SessionID:     sessionID,
			ProjectRoot:   projectRoot,
			Timestamp:     bumpTimestamp(ts, turnIdx),
			Tool:          models.ToolAntigravity,
			ActionType:    actionType,
			Target:        truncate(target, 200),
			Success:       true,
			RawToolName:   "markdown.inline." + actionType,
			RawToolInput:  scrubber.String(raw),
			MessageID:     "antigravity-md:" + conversationID + ":tool:" + intStr(turnIdx) + ":" + intStr(idx),
		})
		idx++
		// Strip the inline tool line from the body. We replace the
		// LITERAL match string globally; it's idempotent.
		out = strings.Replace(out, body[m[0]:m[1]], "", 1)
	}
	return events, out
}

// classifyInlineTool maps an inline-tool string to (action_type, target).
func classifyInlineTool(raw string) (actionType, target string) {
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "viewed "), strings.HasPrefix(lower, "read "):
		return models.ActionReadFile, extractMarkdownLinkTarget(raw)
	case strings.HasPrefix(lower, "edited file "), strings.HasPrefix(lower, "edited "):
		return models.ActionEditFile, extractMarkdownLinkTarget(raw)
	case strings.HasPrefix(lower, "wrote file "), strings.HasPrefix(lower, "wrote "), strings.HasPrefix(lower, "created "):
		return models.ActionWriteFile, extractMarkdownLinkTarget(raw)
	case strings.HasPrefix(lower, "ran command"), strings.HasPrefix(lower, "ran "):
		return models.ActionRunCommand, extractBacktickedTarget(raw)
	case strings.HasPrefix(lower, "grep searched"), strings.Contains(lower, "grep search"):
		return models.ActionSearchText, raw
	case strings.HasPrefix(lower, "searched filesystem"), strings.HasPrefix(lower, "searched "):
		return models.ActionSearchFiles, raw
	case strings.HasPrefix(lower, "fetched"), strings.HasPrefix(lower, "web fetch"):
		return models.ActionWebFetch, extractMarkdownLinkTarget(raw)
	case strings.HasPrefix(lower, "searched the web"), strings.Contains(lower, "web search"):
		return models.ActionWebSearch, raw
	default:
		return "", ""
	}
}

var markdownLinkRE = regexp.MustCompile(`\[([^\]]+)\]\(([^\)]+)\)`)

// extractMarkdownLinkTarget pulls the target URI from a string like
// `Viewed [foo.md](file:///c:/...)`. Returns the URI when present,
// else the inner [...] text, else the original string.
func extractMarkdownLinkTarget(raw string) string {
	m := markdownLinkRE.FindStringSubmatch(raw)
	if len(m) >= 3 {
		uri := m[2]
		// Convert file:/// URLs to a more readable path prefix.
		if strings.HasPrefix(uri, "file:///") {
			uri = strings.TrimPrefix(uri, "file:///")
		}
		return uri
	}
	return raw
}

var backtickRE = regexp.MustCompile("`([^`]+)`")

func extractBacktickedTarget(raw string) string {
	m := backtickRE.FindStringSubmatch(raw)
	if len(m) >= 2 {
		return m[1]
	}
	return raw
}

// stableMarkdownID generates a deterministic SourceEventID derived
// from the file + conversation + turn position. Repeated parses
// produce the same id so the store's (source_file, source_event_id)
// dedup collapses re-runs.
func stableMarkdownID(kind, path, conversationID string, indices ...int) string {
	parts := []string{kind, path, conversationID}
	for _, i := range indices {
		parts = append(parts, intStr(i))
	}
	return contentHash(parts...)
}

// bumpTimestamp shifts ts by turnIdx milliseconds so the events
// have stable, monotonic ordering even when the original .pb's
// per-message timestamps aren't recoverable from the Markdown.
func bumpTimestamp(ts time.Time, turnIdx int) time.Time {
	return ts.Add(time.Duration(turnIdx) * time.Millisecond)
}

func intStr(n int) string { return fmt.Sprintf("%d", n) }

// stringScrubber is the slice of *scrub.Scrubber's API the markdown
// parser needs. Defining the interface locally keeps imports minimal.
type stringScrubber interface {
	String(s string) string
}
