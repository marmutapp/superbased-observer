package hermes

import (
	"context"
	"database/sql"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestIsHarnessSyntheticUserMessage covers the classifier's real-
// shaped cases: the WP-T6 finding H1 verification-loop reminder and
// the codex-ack continuation both grounded against Hermes's own
// source (agent/verification_stop.py + agent/conversation_loop.py,
// see usermessage.go), a genuine human prompt, and edge cases
// (leading whitespace, empty, near-miss prefixes that must NOT match).
func TestIsHarnessSyntheticUserMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{
			name:    "verify_on_stop_nudge",
			content: "[System: You edited code in this turn, but the workspace does not have fresh passing verification evidence yet.\n\nVerification status: unverified",
			want:    true,
		},
		{
			name:    "codex_ack_continuation",
			content: "[System: Continue now. Execute the required tool calls and only send your final answer after completing the task.]",
			want:    true,
		},
		{
			name: "leading_whitespace_tolerated",
			content: "\n\n  [System: Continue now. Execute the required tool calls and only " +
				"send your final answer after completing the task.]",
			want: true,
		},
		{
			// Near-miss: the marker plus the START of a template head,
			// but not enough of it. Prefix matching is exact — a
			// truncated head is not a grounded template.
			name:    "truncated_template_head_not_matched",
			content: "[System: Continue now.]",
			want:    false,
		},
		{
			name:    "genuine_human_prompt",
			content: "Can you run it in a venv?",
			want:    false,
		},
		{
			// F6: a HUMAN message that happens to open with the bare
			// "[System: " marker is a real prompt. The bare marker was
			// too loose a predicate — it silently ate this turn out of
			// the user_prompt boundary count it was meant to protect.
			name:    "human_prompt_opening_with_bare_system_marker",
			content: "[System: production outage] Please inspect the gateway logs and tell me what broke.",
			want:    false,
		},
		{
			// The three stream-recovery continuations from
			// conversation_loop.py::_get_continuation_prompt — all three
			// are appended with role="user" at :1730-1737, exactly like
			// the two above, so all three must classify the same way.
			name: "continuation_partial_stub_dropped_tools",
			content: "[System: Your previous tool call (apply_patch) was too large and the stream timed out " +
				"before it could be delivered. Do NOT retry the same tool call with the same large content.]",
			want: true,
		},
		{
			name: "continuation_network_cutoff",
			content: "[System: The previous response was cut off by a network error mid-stream. " +
				"Continue exactly where you left off.]",
			want: true,
		},
		{
			name: "continuation_length_truncated",
			content: "[System: Your previous response was truncated by the output length limit. " +
				"Continue exactly where you left off.]",
			want: true,
		},
		{
			// tui_gateway/server.py:3755 / :3761 — the personality
			// pivot markers, also appended with role="user".
			name: "gateway_personality_set",
			content: "[System: The user has changed the assistant's personality. From this point forward, " +
				"adopt the following persona and respond accordingly: terse]",
			want: true,
		},
		{
			name: "gateway_personality_cleared",
			content: "[System: The user has cleared the personality overlay. " +
				"From this point forward, respond in your normal default style.]",
			want: true,
		},
		{
			// tui_gateway/server.py:2183 is appended with role="system",
			// never role="user", so it can never reach this classifier —
			// and an operator message quoting it must stay a prompt.
			name:    "gateway_model_change_marker_is_system_role_not_user",
			content: "[System: The active model for this chat has changed to gpt-5.6 via provider openai.]",
			want:    false,
		},
		{
			name:    "empty",
			content: "",
			want:    false,
		},
		{
			name:    "system_note_family_not_matched",
			content: "[System note: The following is recalled memory context, NOT new user input.]",
			want:    false,
		},
		{
			name:    "mentions_system_but_not_prefix",
			content: "Please check the [System] logs for errors.",
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isHarnessSyntheticUserMessage(tc.content)
			if got != tc.want {
				t.Errorf("isHarnessSyntheticUserMessage(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
}

// TestBuildEvents_HarnessSyntheticUserMessagesNotCountedAsUserPrompt is
// the WP-T6 H1 regression: a single human prompt followed by Hermes's
// own verify-on-stop nudge and codex-ack continuation (both role=user)
// must yield exactly ONE ActionUserPrompt row, not three — a synthetic
// boundary must not inflate the count internal/predict's turns-per-
// message fan-out ladder resolves T from.
func TestBuildEvents_HarnessSyntheticUserMessagesNotCountedAsUserPrompt(t *testing.T) {
	t.Parallel()
	sess := sessionRow{
		ID:     "sess1",
		Model:  "nvidia/nemotron-3-ultra-550b-a55b:free",
		CWD:    "/tmp/wpt6/hermes",
		Source: "cli",
	}
	sessions := map[string]sessionRow{"sess1": sess}

	messages := []messageRow{
		{
			ID:        1,
			SessionID: "sess1",
			Role:      "user",
			Content:   sql.NullString{String: "Can you run it in a venv?", Valid: true},
			Timestamp: 1780674253.0,
		},
		{
			ID:        2,
			SessionID: "sess1",
			Role:      "assistant",
			Content:   sql.NullString{String: "Sure, running it now.", Valid: true},
			Timestamp: 1780674254.0,
		},
		{
			// The verify-on-stop nudge (WP-T6 H1).
			ID:        3,
			SessionID: "sess1",
			Role:      "user",
			Content: sql.NullString{
				String: "[System: You edited code in this turn, but the workspace does not have " +
					"fresh passing verification evidence yet.\n\nVerification status: unverified]",
				Valid: true,
			},
			Timestamp: 1780674255.0,
		},
		{
			ID:        4,
			SessionID: "sess1",
			Role:      "assistant",
			Content:   sql.NullString{String: "Running the test suite now.", Valid: true},
			Timestamp: 1780674256.0,
		},
		{
			// The codex-ack continuation (same "[System: " family).
			ID:        5,
			SessionID: "sess1",
			Role:      "user",
			Content: sql.NullString{
				String: "[System: Continue now. Execute the required tool calls and only send your final answer after completing the task.]",
				Valid:  true,
			},
			Timestamp: 1780674257.0,
		},
	}

	toolEvents, _, warnings := buildEvents(context.Background(), sessions, messages, "state.db", nil, nil)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}

	var userPrompts []models.ToolEvent
	for _, e := range toolEvents {
		if e.ActionType == models.ActionUserPrompt {
			userPrompts = append(userPrompts, e)
		}
	}
	if len(userPrompts) != 1 {
		t.Fatalf("len(userPrompts) = %d, want 1 (one human prompt, two synthetic reminders excluded); rows: %+v", len(userPrompts), userPrompts)
	}
	if userPrompts[0].Target != "Can you run it in a venv?" {
		t.Errorf("surviving user_prompt Target = %q, want the genuine human message", userPrompts[0].Target)
	}

	// The two synthetic rows must not surface under ANY action type —
	// they are skipped entirely (the honest choice per the task: a
	// harness reminder is not developer activity), not remapped to a
	// different bucket.
	for _, e := range toolEvents {
		if e.MessageID == "user:hermes-msg-3" || e.MessageID == "user:hermes-msg-5" {
			t.Errorf("synthetic message id=3/5 produced an event: %+v", e)
		}
	}
}
