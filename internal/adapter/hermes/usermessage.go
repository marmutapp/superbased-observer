package hermes

import "strings"

// harnessSyntheticUserPrefixes are the literal heads of the templates
// Hermes's own agent loop injects into the conversation as though the
// user had sent them — never something the human operator typed.
//
// Each entry is VERBATIM the first source-literal chunk of its template
// in Hermes's own source (~/.hermes/hermes-agent, live install,
// 2026-07-31), so the grounding is reproducible by grepping the cited
// file:line. Every one of these sites appends its text with
// `"role": "user"`, which is what makes it reachable from here:
//
//   - agent/verification_stop.py:155 (build_verify_on_stop_nudge,
//     injected at conversation_loop.py:4690 with role="user") — the
//     "you edited code but there is no fresh passing verification
//     evidence" reminder. WP-T6 probe finding H1,
//     /tmp/wpt6/hermes-findings.md.
//   - agent/conversation_loop.py:4627 — the codex_ack continuation
//     (role="user" at :4625).
//   - agent/conversation_loop.py:407, :421, :428 — the three
//     _get_continuation_prompt stream-recovery continuations
//     (oversized tool call / network cutoff / output-length
//     truncation), all injected at :1730-1737 with role="user".
//   - tui_gateway/server.py:3755, :3761 — the personality pivot
//     markers, appended to session history with role="user".
//
// MATCHING THE BARE "[System: " MARKER IS NOT ENOUGH (F6). It is also
// a plausible opening for a REAL human message — e.g.
// "[System: production outage] Please inspect the gateway logs" — and
// swallowing that turn is the exact bias H1 set out to remove, just
// with the sign flipped: a genuine prompt boundary disappears from the
// count internal/predict's fan-out ladder resolves T from. Matching the
// template heads instead keeps every grounded synthetic out while
// leaving human text alone.
//
// DELIBERATELY ABSENT:
//
//   - tui_gateway/server.py:2183, the active-model-change marker. It is
//     appended with `{"role": "system"}`, not "user", so it can never
//     reach this classifier; a role='user' row that opens with that
//     text is therefore an operator quoting it, and stays a prompt.
//   - The "[System note: " family (browser connect/disconnect,
//     onboarding, recalled-memory framing, gateway auto-continue) —
//     grounded in source (hermes_cli/cli_commands_mixin.py,
//     agent/onboarding.py, agent/memory_manager.py, hermes_cli/config.py)
//     but NEVER observed as a live messages.content row (0 matches for
//     `content LIKE '%System note%'`). Per the no-heuristic-lies rule
//     this list stays pinned to grounded templates; widen it against new
//     live evidence, don't extrapolate.
//
// Live confirmation of the one template that has actually landed in this
// corpus:
//
//	sqlite3 ~/.hermes/state.db \
//	  "SELECT role, substr(content,1,60), count(*) FROM messages \
//	   WHERE content LIKE '[System%' GROUP BY role, substr(content,1,60)"
//	-> user|[System: You edited code in this turn, ...|3
var harnessSyntheticUserPrefixes = []string{
	// agent/verification_stop.py:155
	"[System: You edited code in this turn, but the workspace does not have ",
	// agent/conversation_loop.py:4627
	"[System: Continue now. Execute the required tool calls and only ",
	// agent/conversation_loop.py:407
	"[System: Your previous tool call ",
	// agent/conversation_loop.py:421
	"[System: The previous response was cut off by a ",
	// agent/conversation_loop.py:428
	"[System: Your previous response was truncated by the output ",
	// tui_gateway/server.py:3755
	"[System: The user has changed the assistant's personality. ",
	// tui_gateway/server.py:3761
	"[System: The user has cleared the personality overlay. ",
}

// isHarnessSyntheticUserMessage reports whether a role='user' message
// body is one of Hermes's own harness-injected pseudo-user turns
// (matched against harnessSyntheticUserPrefixes) rather than something
// the human operator typed.
//
// This is the single classifier both hermes capture paths must use so
// they can't disagree (WP-T6 finding H1): the SQLite backfill path
// consults it in userPromptEvent's call site (parse.go's "user" case
// in buildEvents). The hook path (hook.go) has no user-message hook
// event at all today — the Python plugin bridge only fires
// tool_call/session_start/session_end/api_request/subagent_stop (see
// hook.go's EventXxx constants) — so there is currently no second call
// site to wire; if a future plugin revision adds one, it must route
// through this same function rather than re-deriving the prefixes.
//
// Counting a synthetic reminder as a real user_prompt row shrinks the
// observed prompt-boundary count that internal/predict's turns-per-
// message fan-out ladder resolves T from, silently biasing the next-
// message cost band low. Dropping a real prompt that merely opens with
// "[System: " biases it the other way (F6) — hence exact template
// heads, not the bare marker.
func isHarnessSyntheticUserMessage(content string) bool {
	trimmed := strings.TrimSpace(content)
	for _, p := range harnessSyntheticUserPrefixes {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}
