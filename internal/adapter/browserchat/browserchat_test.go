package browserchat

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

func chatGPTTurn() CapturedTurn {
	return CapturedTurn{
		SchemaVersion:     CapturedTurnSchemaVersion,
		Site:              models.ToolChatGPTWeb,
		ConversationID:    "conv-123",
		MessageID:         "msg-abc",
		Model:             "gpt-4o",
		RequestURL:        "https://chatgpt.com/backend-api/conversation",
		PromptText:        "what is the capital of France?",
		ResponseText:      "The capital of France is Paris.",
		PromptTokensEst:   8,
		ResponseTokensEst: 7,
		LatencyMs:         1200,
		CapturedAt:        "2026-07-10T12:00:00Z",
		Granularity:       string(GranularityFull),
		Title:             "geography",
		IDSource:          "request",
		CaptureID:         "cap-xyz",
	}
}

// findEvent returns the first tool event whose ActionType matches, and
// whether one was found.
func findEvent(evs []models.ToolEvent, actionType string) (models.ToolEvent, bool) {
	for _, e := range evs {
		if e.ActionType == actionType {
			return e, true
		}
	}
	return models.ToolEvent{}, false
}

// TestBuildToolEvent exercises the exported BuildToolEvent seam, which now
// surfaces the ASSISTANT event of a turn (the prompt lives on the separate
// user_prompt event — see TestBuildToolEventsTwoEventShape). The assistant
// event never carries the prompt in RawToolInput; at content granularity it
// carries the response in ToolOutput, at usage_only nothing.
func TestBuildToolEvent(t *testing.T) {
	sc := scrub.New()
	tests := []struct {
		name          string
		mutate        func(*CapturedTurn)
		wantOK        bool
		wantErr       bool
		wantOutputSet bool // expect a non-empty ToolOutput on the assistant event
	}{
		{
			name:          "full granularity carries response on the assistant event",
			mutate:        func(c *CapturedTurn) { c.Granularity = string(GranularityFull) },
			wantOK:        true,
			wantOutputSet: true,
		},
		{
			name:          "redacted granularity carries response on the assistant event",
			mutate:        func(c *CapturedTurn) { c.Granularity = string(GranularityRedacted) },
			wantOK:        true,
			wantOutputSet: true,
		},
		{
			name:          "usage-only constructs no content",
			mutate:        func(c *CapturedTurn) { c.Granularity = string(GranularityUsageOnly) },
			wantOK:        true,
			wantOutputSet: false,
		},
		{
			name:          "empty granularity defaults to usage-only",
			mutate:        func(c *CapturedTurn) { c.Granularity = "" },
			wantOK:        true,
			wantOutputSet: false,
		},
		{
			name:          "unknown granularity defaults to usage-only",
			mutate:        func(c *CapturedTurn) { c.Granularity = "bogus" },
			wantOK:        true,
			wantOutputSet: false,
		},
		{
			name:    "unknown site is an error",
			mutate:  func(c *CapturedTurn) { c.Site = "definitely-not-a-site" },
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := chatGPTTurn()
			tc.mutate(&turn)
			ev, ok, err := BuildToolEvent(turn, sc)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (ok=%v)", ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ev.Tool != models.ToolChatGPTWeb {
				t.Errorf("Tool = %q, want %q", ev.Tool, models.ToolChatGPTWeb)
			}
			if ev.ActionType != models.ActionAssistantMessage {
				t.Errorf("ActionType = %q, want %q", ev.ActionType, models.ActionAssistantMessage)
			}
			if ev.SessionID != "conv-123" {
				t.Errorf("SessionID = %q, want conv-123", ev.SessionID)
			}
			if ev.ProjectRoot != "browser://chatgpt.com" {
				t.Errorf("ProjectRoot = %q, want browser://chatgpt.com", ev.ProjectRoot)
			}
			if ev.RawToolName != "chatgpt-web.assistant_text" {
				t.Errorf("RawToolName = %q, want chatgpt-web.assistant_text", ev.RawToolName)
			}
			if ev.SourceFile != "chatgpt-web:extension" {
				t.Errorf("SourceFile = %q, want chatgpt-web:extension", ev.SourceFile)
			}
			if ev.DurationMs != 1200 {
				t.Errorf("DurationMs = %d, want 1200", ev.DurationMs)
			}
			// The assistant event NEVER carries the prompt in RawToolInput —
			// it lives on the paired user_prompt event.
			if ev.RawToolInput != "" {
				t.Errorf("assistant RawToolInput = %q, want empty (prompt lives on the user_prompt event)", ev.RawToolInput)
			}
			if tc.wantOutputSet && ev.ToolOutput == "" {
				t.Errorf("expected non-empty ToolOutput")
			}
			if !tc.wantOutputSet && ev.ToolOutput != "" {
				t.Errorf("expected empty ToolOutput, got %q", ev.ToolOutput)
			}
			// The assistant event always carries API-call detail metadata.
			if ev.Metadata == nil {
				t.Fatalf("assistant Metadata is nil; want API-call detail")
			}
		})
	}
}

// TestBuildToolEventsTwoEventShape pins the core reshape: at content-bearing
// granularity a turn normalizes to a user_prompt event AND an
// assistant_message event, with the content split (prompt only on the user
// row, response only on the assistant row) and the dashboard's grouping keys
// (MessageID "user:<mid>" / "<mid>"; distinct SourceEventIDs).
func TestBuildToolEventsTwoEventShape(t *testing.T) {
	for _, gran := range []Granularity{GranularityFull, GranularityRedacted} {
		t.Run(string(gran), func(t *testing.T) {
			turn := chatGPTTurn()
			turn.Granularity = string(gran)
			body, _ := json.Marshal(turn)
			toolEvents, _, err := Normalize(body, scrub.New())
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			if len(toolEvents) != 2 {
				t.Fatalf("toolEvents = %d, want 2 (user_prompt + assistant)", len(toolEvents))
			}

			user, ok := findEvent(toolEvents, models.ActionUserPrompt)
			if !ok {
				t.Fatalf("no user_prompt event emitted")
			}
			assistant, ok := findEvent(toolEvents, models.ActionAssistantMessage)
			if !ok {
				t.Fatalf("no assistant_message event emitted")
			}

			// Content split: prompt only on the user row.
			if user.RawToolInput != "what is the capital of France?" {
				t.Errorf("user RawToolInput = %q, want the prompt", user.RawToolInput)
			}
			if user.ToolOutput != "" {
				t.Errorf("user ToolOutput = %q, want empty", user.ToolOutput)
			}
			// Response only on the assistant row; prompt not restated.
			if assistant.RawToolInput != "" {
				t.Errorf("assistant RawToolInput = %q, want empty (prompt lives on the user row)", assistant.RawToolInput)
			}
			if assistant.ToolOutput != "The capital of France is Paris." {
				t.Errorf("assistant ToolOutput = %q, want the response", assistant.ToolOutput)
			}

			// Dashboard grouping keys.
			if user.MessageID != "user:msg-abc" {
				t.Errorf("user MessageID = %q, want user:msg-abc", user.MessageID)
			}
			if assistant.MessageID != "msg-abc" {
				t.Errorf("assistant MessageID = %q, want msg-abc", assistant.MessageID)
			}
			if user.RawToolName != "chatgpt-web.user_prompt" {
				t.Errorf("user RawToolName = %q, want chatgpt-web.user_prompt", user.RawToolName)
			}
			if user.Target != "what is the capital of France?" {
				t.Errorf("user Target = %q, want a prompt preview", user.Target)
			}

			// Both events share the session key but have distinct dedup ids.
			if user.SessionID != assistant.SessionID {
				t.Errorf("session keys diverge: user=%q assistant=%q", user.SessionID, assistant.SessionID)
			}
			if user.SourceEventID == assistant.SourceEventID {
				t.Errorf("user + assistant SourceEventID collide: %q", user.SourceEventID)
			}
			if !strings.HasSuffix(user.SourceEventID, ":user") {
				t.Errorf("user SourceEventID = %q, want a ':user' suffix", user.SourceEventID)
			}
			if user.SourceEventID != assistant.SourceEventID+":user" {
				t.Errorf("user SourceEventID = %q, want assistant id + :user (%q)", user.SourceEventID, assistant.SourceEventID+":user")
			}
		})
	}
}

// TestBuildToolEventsUsageOnlySingle pins that at usage_only there is exactly
// ONE event (the metadata-only assistant row) — a user row with no text is
// noise, so no user_prompt event is emitted.
func TestBuildToolEventsUsageOnlySingle(t *testing.T) {
	turn := chatGPTTurn()
	turn.Granularity = string(GranularityUsageOnly)
	body, _ := json.Marshal(turn)
	toolEvents, _, err := Normalize(body, scrub.New())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(toolEvents) != 1 {
		t.Fatalf("toolEvents = %d, want 1 (assistant only)", len(toolEvents))
	}
	if toolEvents[0].ActionType != models.ActionAssistantMessage {
		t.Errorf("ActionType = %q, want assistant_message", toolEvents[0].ActionType)
	}
	if toolEvents[0].RawToolInput != "" || toolEvents[0].ToolOutput != "" {
		t.Errorf("usage-only must carry no content: input=%q output=%q", toolEvents[0].RawToolInput, toolEvents[0].ToolOutput)
	}
	if _, ok := findEvent(toolEvents, models.ActionUserPrompt); ok {
		t.Errorf("usage-only must NOT emit a user_prompt event")
	}
	// Metadata is still present — it is non-content API-call detail.
	if toolEvents[0].Metadata == nil {
		t.Fatalf("usage-only assistant Metadata is nil; want API-call detail")
	}
}

// TestAssistantMetadata pins the API-call detail carried on the assistant
// event's Metadata column, and that it round-trips through JSON (the shape the
// store marshals into actions.metadata).
func TestAssistantMetadata(t *testing.T) {
	turn := chatGPTTurn()
	ev, ok, err := BuildToolEvent(turn, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
	}
	m := ev.Metadata
	if m == nil {
		t.Fatalf("Metadata is nil")
	}
	if m.RequestURL != "https://chatgpt.com/backend-api/conversation" {
		t.Errorf("RequestURL = %q", m.RequestURL)
	}
	if m.IDSource != "request" {
		t.Errorf("IDSource = %q, want request", m.IDSource)
	}
	if m.Granularity != string(GranularityFull) {
		t.Errorf("Granularity = %q, want full", m.Granularity)
	}
	if m.PromptTokensEst != 8 {
		t.Errorf("PromptTokensEst = %d, want 8", m.PromptTokensEst)
	}
	if m.ResponseTokensEst != 7 {
		t.Errorf("ResponseTokensEst = %d, want 7", m.ResponseTokensEst)
	}
	// Metadata must never carry prompt/response content.
	blob, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	if strings.Contains(string(blob), "capital of France") || strings.Contains(string(blob), "Paris") {
		t.Errorf("metadata leaked content: %s", blob)
	}
	// Round-trip.
	var back models.ActionMetadata
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if back != *m {
		t.Errorf("metadata did not round-trip: %+v vs %+v", back, *m)
	}
}

// TestMetadataReflectsEffectiveGranularity pins that the metadata records the
// EFFECTIVE (post-daemon-ceiling clamp) granularity, not what the client sent:
// a "full" turn clamped to usage_only reports usage_only.
func TestMetadataReflectsEffectiveGranularity(t *testing.T) {
	turn := chatGPTTurn()
	turn.Granularity = string(GranularityFull)
	body, _ := json.Marshal(turn)
	toolEvents, _, err := NormalizeWith(body, Options{
		Scrubber:           scrub.New(),
		GranularityCeiling: GranularityUsageOnly,
	})
	if err != nil {
		t.Fatalf("NormalizeWith: %v", err)
	}
	assistant, ok := findEvent(toolEvents, models.ActionAssistantMessage)
	if !ok || assistant.Metadata == nil {
		t.Fatalf("no assistant event with metadata (ok=%v)", ok)
	}
	if assistant.Metadata.Granularity != string(GranularityUsageOnly) {
		t.Errorf("Granularity = %q, want usage_only (effective, post-clamp)", assistant.Metadata.Granularity)
	}
}

// TestSourceEventIDDistinctness pins that the three rows of ONE turn — the
// user_prompt action, the assistant action, and the token event — all carry
// DISTINCT SourceEventIDs, so store.Ingest's (source_file, source_event_id)
// idempotency key can never dedup-swallow one against another.
func TestSourceEventIDDistinctness(t *testing.T) {
	turn := chatGPTTurn()
	body, _ := json.Marshal(turn)
	toolEvents, tokenEvents, err := Normalize(body, scrub.New())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(toolEvents) != 2 || len(tokenEvents) != 1 {
		t.Fatalf("events = (%d tool, %d token), want (2, 1)", len(toolEvents), len(tokenEvents))
	}
	seen := map[string]int{}
	for _, e := range toolEvents {
		seen[e.SourceEventID]++
	}
	seen[tokenEvents[0].SourceEventID]++
	for id, n := range seen {
		if n != 1 {
			t.Errorf("SourceEventID %q used %d times, want 1 (collision)", id, n)
		}
	}
	if len(seen) != 3 {
		t.Errorf("distinct SourceEventIDs = %d, want 3", len(seen))
	}
}

// TestServerScrubBackstopFullGranularity pins the §6.2 server backstop: the
// client-side redaction pipeline is the PRIMARY control, but any content that
// crosses the wire at full (or redacted) granularity MUST still be scrubbed
// by the ingest-time scrub.Scrubber before it is stored. With the two-event
// reshape the prompt lands on the user_prompt event and the response on the
// assistant event, so the backstop is asserted per event.
func TestServerScrubBackstopFullGranularity(t *testing.T) {
	// Secret-shaped fixtures assembled by concatenation so no contiguous
	// credential literal appears in the source file.
	ghToken := "gh" + "p_" + strings.Repeat("A", 32)
	awsKey := "AKIA" + "ABCDEFGHIJKLMNOP"

	turn := chatGPTTurn()
	turn.Granularity = string(GranularityFull)
	turn.PromptText = "here is my token " + ghToken + " please use it"
	turn.ResponseText = "your aws key " + awsKey + " is noted"

	// The full NormalizeWith seam ingestBrowserTurn actually calls, at full
	// granularity with no daemon ceiling — the backstop must hold on BOTH the
	// user_prompt event (prompt) and the assistant event (response).
	body, _ := json.Marshal(turn)
	toolEvents, _, err := NormalizeWith(body, Options{Scrubber: scrub.New()})
	if err != nil {
		t.Fatalf("NormalizeWith: %v", err)
	}
	if len(toolEvents) != 2 {
		t.Fatalf("toolEvents = %d, want 2", len(toolEvents))
	}
	user, ok := findEvent(toolEvents, models.ActionUserPrompt)
	if !ok {
		t.Fatalf("no user_prompt event")
	}
	assistant, ok := findEvent(toolEvents, models.ActionAssistantMessage)
	if !ok {
		t.Fatalf("no assistant event")
	}
	if strings.Contains(user.RawToolInput, ghToken) {
		t.Errorf("user RawToolInput leaked the raw token: %q", user.RawToolInput)
	}
	if !strings.Contains(user.RawToolInput, scrub.Redacted) {
		t.Errorf("user RawToolInput not scrubbed (no %s sentinel): %q", scrub.Redacted, user.RawToolInput)
	}
	if strings.Contains(assistant.ToolOutput, awsKey) {
		t.Errorf("assistant ToolOutput leaked the raw AWS key: %q", assistant.ToolOutput)
	}
	if !strings.Contains(assistant.ToolOutput, scrub.Redacted) {
		t.Errorf("assistant ToolOutput not scrubbed (no %s sentinel): %q", scrub.Redacted, assistant.ToolOutput)
	}
}

func TestBuildToolEventDefaultModelWhenAbsent(t *testing.T) {
	turn := chatGPTTurn()
	turn.Model = ""
	ev, ok, err := BuildToolEvent(turn, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
	}
	if ev.Model != "gpt-4o" {
		t.Errorf("Model = %q, want gpt-4o (default)", ev.Model)
	}
}

// TestBuildToolEventTargetUsesHarvestedModel pins Target to the model the
// capture actually harvested, not the site's fallback default — at usage_only
// there is no response preview to overwrite Target, so the fallback used to
// leak through as the visible action target (e.g. "claude-sonnet-4-5" shown
// for a claude-opus-4-8 turn).
func TestBuildToolEventTargetUsesHarvestedModel(t *testing.T) {
	turn := chatGPTTurn()
	turn.Model = "gpt-5-6-thinking" // NOT the chatgpt-web default (gpt-4o)
	turn.Granularity = string(GranularityUsageOnly)
	ev, ok, err := BuildToolEvent(turn, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
	}
	if ev.Target != "gpt-5-6-thinking" {
		t.Errorf("Target = %q, want gpt-5-6-thinking (harvested model, not site default)", ev.Target)
	}
	if ev.Model != "gpt-5-6-thinking" {
		t.Errorf("Model = %q, want gpt-5-6-thinking", ev.Model)
	}
}

// TestBuildToolEventNilScrubber checks that with a nil scrubber content is
// still stored (no scrubbing). The prompt lives on the user_prompt event and
// the response on the assistant event.
func TestBuildToolEventNilScrubber(t *testing.T) {
	turn := chatGPTTurn()
	body, _ := json.Marshal(turn)
	toolEvents, _, err := NormalizeWith(body, Options{Scrubber: nil})
	if err != nil {
		t.Fatalf("NormalizeWith: %v", err)
	}
	user, ok := findEvent(toolEvents, models.ActionUserPrompt)
	if !ok || user.RawToolInput == "" {
		t.Errorf("expected prompt content on the user event with a nil scrubber (no scrubbing, still stored)")
	}
	assistant, ok := findEvent(toolEvents, models.ActionAssistantMessage)
	if !ok || assistant.ToolOutput == "" {
		t.Errorf("expected response content on the assistant event with a nil scrubber")
	}
}

func TestBuildTokenEvent(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*CapturedTurn)
		wantOK     bool
		wantErr    bool
		wantInput  int64
		wantOutput int64
	}{
		{
			name:       "client estimate preferred",
			mutate:     func(c *CapturedTurn) {},
			wantOK:     true,
			wantInput:  8,
			wantOutput: 7,
		},
		{
			name: "server chars/4 fallback when client estimate absent",
			mutate: func(c *CapturedTurn) {
				c.PromptTokensEst = 0
				c.ResponseTokensEst = 0
			},
			wantOK: true,
			// "what is the capital of France?" = 30 runes → (30+3)/4 = 8
			wantInput: 8,
			// "The capital of France is Paris." = 31 runes → (31+3)/4 = 8
			wantOutput: 8,
		},
		{
			name: "usage-only with no content and no estimate yields nothing",
			mutate: func(c *CapturedTurn) {
				c.PromptTokensEst = 0
				c.ResponseTokensEst = 0
				c.PromptText = ""
				c.ResponseText = ""
			},
			wantOK: false,
		},
		{
			name:    "unknown site is an error",
			mutate:  func(c *CapturedTurn) { c.Site = "definitely-not-a-site" },
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := chatGPTTurn()
			tc.mutate(&turn)
			tok, ok, err := BuildTokenEvent(turn)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if tok.Source != models.TokenSourceEstimated {
				t.Errorf("Source = %q, want %q", tok.Source, models.TokenSourceEstimated)
			}
			if tok.Reliability != models.ReliabilityUnreliable {
				t.Errorf("Reliability = %q, want %q", tok.Reliability, models.ReliabilityUnreliable)
			}
			if tok.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d", tok.InputTokens, tc.wantInput)
			}
			if tok.OutputTokens != tc.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", tok.OutputTokens, tc.wantOutput)
			}
			if tok.Tool != models.ToolChatGPTWeb {
				t.Errorf("Tool = %q, want %q", tok.Tool, models.ToolChatGPTWeb)
			}
			if tok.SessionID != "conv-123" {
				t.Errorf("SessionID = %q, want conv-123", tok.SessionID)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	turn := chatGPTTurn()
	body, _ := json.Marshal(turn)
	toolEvents, tokenEvents, err := Normalize(body, scrub.New())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// Default fixture is full granularity → user_prompt + assistant.
	if len(toolEvents) != 2 {
		t.Fatalf("toolEvents = %d, want 2", len(toolEvents))
	}
	if len(tokenEvents) != 1 {
		t.Fatalf("tokenEvents = %d, want 1", len(tokenEvents))
	}
	// Every SourceEventID (both tool events + the token event) must differ.
	ids := map[string]struct{}{
		toolEvents[0].SourceEventID:  {},
		toolEvents[1].SourceEventID:  {},
		tokenEvents[0].SourceEventID: {},
	}
	if len(ids) != 3 {
		t.Errorf("SourceEventIDs collide: %v", ids)
	}
}

func TestNormalizeUsageOnlyStillEmitsToolEvent(t *testing.T) {
	turn := chatGPTTurn()
	turn.Granularity = string(GranularityUsageOnly)
	body, _ := json.Marshal(turn)
	toolEvents, tokenEvents, err := Normalize(body, scrub.New())
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(toolEvents) != 1 {
		t.Fatalf("toolEvents = %d, want 1 (the turn still records, minus content)", len(toolEvents))
	}
	if toolEvents[0].RawToolInput != "" || toolEvents[0].ToolOutput != "" {
		t.Errorf("usage-only must carry no content: input=%q output=%q", toolEvents[0].RawToolInput, toolEvents[0].ToolOutput)
	}
	// Client estimates are still present in usage-only, so a token row lands.
	if len(tokenEvents) != 1 {
		t.Fatalf("tokenEvents = %d, want 1 (client estimate present)", len(tokenEvents))
	}
}

func TestNormalizeBadJSON(t *testing.T) {
	if _, _, err := Normalize([]byte("{not json"), scrub.New()); err == nil {
		t.Fatalf("expected error on malformed JSON")
	}
}

func TestNormalizeUnknownSite(t *testing.T) {
	turn := chatGPTTurn()
	turn.Site = "definitely-not-a-site"
	body, _ := json.Marshal(turn)
	if _, _, err := Normalize(body, scrub.New()); err == nil {
		t.Fatalf("expected error on unknown site")
	}
}

// TestParseFlexibleContentFields pins the defense-in-depth decoder: the
// free-text content fields (prompt_text / response_text / title) must tolerate
// their JSON value arriving as a plain string OR an array of strings /
// {text}-objects, joining real text parts with "\n" and degrading any other
// shape (number / null / bare object) to "" — WITHOUT ever erroring, so the
// rest of the turn is still captured. Roots the live copilot-web drop
// (`json: cannot unmarshal array into ...prompt_text of type string`).
func TestParseFlexibleContentFields(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantPrompt   string
		wantResponse string
		wantTitle    string
	}{
		{
			name:       "prompt_text plain string",
			body:       `{"site":"chatgpt-web","prompt_text":"hello world"}`,
			wantPrompt: "hello world",
		},
		{
			name:       "prompt_text array of strings joined",
			body:       `{"site":"chatgpt-web","prompt_text":["a","b","c"]}`,
			wantPrompt: "a\nb\nc",
		},
		{
			name:       "prompt_text array of {text} objects joined",
			body:       `{"site":"chatgpt-web","prompt_text":[{"type":"text","text":"one"},{"type":"text","text":"two"}]}`,
			wantPrompt: "one\ntwo",
		},
		{
			name:       "prompt_text mixed array drops non-text parts",
			body:       `{"site":"chatgpt-web","prompt_text":[{"type":"text","text":"before"},{"type":"image","url":"blob:x"},"after"]}`,
			wantPrompt: "before\nafter",
		},
		{
			name:       "prompt_text number degrades to empty",
			body:       `{"site":"chatgpt-web","prompt_text":42}`,
			wantPrompt: "",
		},
		{
			name:       "prompt_text null degrades to empty",
			body:       `{"site":"chatgpt-web","prompt_text":null}`,
			wantPrompt: "",
		},
		{
			name:       "prompt_text bare object degrades to empty",
			body:       `{"site":"chatgpt-web","prompt_text":{"foo":"bar"}}`,
			wantPrompt: "",
		},
		{
			name:         "response_text array of {text} joined",
			body:         `{"site":"chatgpt-web","response_text":[{"text":"r1"},{"text":"r2"}]}`,
			wantResponse: "r1\nr2",
		},
		{
			name:         "response_text garbage array degrades to empty",
			body:         `{"site":"chatgpt-web","response_text":[1,true,{"foo":"bar"}]}`,
			wantResponse: "",
		},
		{
			name:      "title array joined",
			body:      `{"site":"chatgpt-web","title":["t1","t2"]}`,
			wantTitle: "t1\nt2",
		},
		// --- shared shape contract cross-cases (must mirror the JS side EXACTLY;
		// see the coerceCases cross-cases in parsers.test.js).
		{
			name:       "prompt_text top-level {text} object",
			body:       `{"site":"chatgpt-web","prompt_text":{"text":"real"}}`,
			wantPrompt: "real",
		},
		{
			name:       "prompt_text array of {content:[{text}]} elements",
			body:       `{"site":"chatgpt-web","prompt_text":[{"content":[{"text":"real"}]}]}`,
			wantPrompt: "real",
		},
		{
			name:       "prompt_text array of {parts:[...]} elements",
			body:       `{"site":"chatgpt-web","prompt_text":[{"parts":["real"]}]}`,
			wantPrompt: "real",
		},
		{
			name:       "prompt_text nested bare arrays flattened",
			body:       `{"site":"chatgpt-web","prompt_text":["a",["b","c"],[["d"]]]}`,
			wantPrompt: "a\nb\nc\nd",
		},
		{
			name:       "prompt_text object .text as array of parts",
			body:       `{"site":"chatgpt-web","prompt_text":{"text":[{"text":"x"},"y"]}}`,
			wantPrompt: "x\ny",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.body))
			if err != nil {
				t.Fatalf("Parse must degrade, not error: %v", err)
			}
			if got.PromptText != tt.wantPrompt {
				t.Errorf("PromptText = %q, want %q", got.PromptText, tt.wantPrompt)
			}
			if got.ResponseText != tt.wantResponse {
				t.Errorf("ResponseText = %q, want %q", got.ResponseText, tt.wantResponse)
			}
			if got.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", got.Title, tt.wantTitle)
			}
		})
	}
}

// TestCapturedTurnUnmarshalReusesReceiver pins encoding/json receiver-reuse
// semantics (finding 4): decoding JSON that OMITS prompt_text/response_text/
// title into a CapturedTurn that already carries values must PRESERVE those
// values, not erase them to "" — the shadow flexString fields are seeded from
// the receiver before decode, and json only calls flexString.UnmarshalJSON for
// keys actually present.
func TestCapturedTurnUnmarshalReusesReceiver(t *testing.T) {
	turn := CapturedTurn{
		PromptText:   "kept-prompt",
		ResponseText: "kept-response",
		Title:        "kept-title",
		Model:        "kept-model",
	}
	// This JSON omits all three flex fields (and model) entirely.
	if err := json.Unmarshal([]byte(`{"site":"chatgpt-web","conversation_id":"c1"}`), &turn); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if turn.PromptText != "kept-prompt" {
		t.Errorf("PromptText erased on omitted key: got %q, want %q", turn.PromptText, "kept-prompt")
	}
	if turn.ResponseText != "kept-response" {
		t.Errorf("ResponseText erased on omitted key: got %q, want %q", turn.ResponseText, "kept-response")
	}
	if turn.Title != "kept-title" {
		t.Errorf("Title erased on omitted key: got %q, want %q", turn.Title, "kept-title")
	}
	// Present keys still overwrite (a null present key degrades to "").
	if err := json.Unmarshal([]byte(`{"prompt_text":"new-prompt","title":null}`), &turn); err != nil {
		t.Fatalf("Unmarshal 2: %v", err)
	}
	if turn.PromptText != "new-prompt" {
		t.Errorf("present prompt_text not applied: got %q", turn.PromptText)
	}
	if turn.Title != "" {
		t.Errorf("present null title should degrade to empty: got %q", turn.Title)
	}
	if turn.ResponseText != "kept-response" {
		t.Errorf("ResponseText erased by second decode: got %q", turn.ResponseText)
	}
}

// TestCoerceFlexValueBounds pins the traversal bounds (finding 1): a
// pathologically deep or huge content value must degrade WITHOUT unbounded
// recursion/allocation, keeping what it accumulated rather than erroring.
func TestCoerceFlexValueBounds(t *testing.T) {
	// ~5,000-deep nested array around a leaf: past the depth bound the leaf is
	// dropped, but Parse must NOT error and the field degrades to a string.
	deep := `"buried"`
	for i := 0; i < 5000; i++ {
		deep = "[" + deep + "]"
	}
	got, err := Parse([]byte(`{"site":"chatgpt-web","prompt_text":` + deep + `}`))
	if err != nil {
		t.Fatalf("deep nesting must degrade, not error: %v", err)
	}
	if got.PromptText != "" {
		t.Errorf("deep-nested leaf past depth bound should drop: got %q", got.PromptText)
	}

	// Huge flat array of {text} parts: node-bounded to maxCoerceParts, kept
	// prefix non-empty, never all joined.
	var b strings.Builder
	b.WriteString(`{"site":"chatgpt-web","prompt_text":[`)
	for i := 0; i < 100000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"text":"p"}`)
	}
	b.WriteString(`]}`)
	got, err = Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("huge array must degrade, not error: %v", err)
	}
	if got.PromptText == "" {
		t.Errorf("huge array should keep a non-empty prefix")
	}
	if n := strings.Count(got.PromptText, "\n") + 1; n > maxCoerceParts {
		t.Errorf("huge array not node-bounded: joined %d parts, want <= %d", n, maxCoerceParts)
	}

	// A single leaf larger than the byte budget is clamped, not dropped.
	huge := strings.Repeat("z", maxCoerceBytes+4096)
	body, _ := json.Marshal(map[string]string{"site": "chatgpt-web", "prompt_text": huge})
	got, err = Parse(body)
	if err != nil {
		t.Fatalf("over-budget leaf must degrade, not error: %v", err)
	}
	if len(got.PromptText) != maxCoerceBytes {
		t.Errorf("over-budget leaf should clamp to %d bytes, got %d", maxCoerceBytes, len(got.PromptText))
	}
}

// TestCoerceFlexValueByteBudgetUTF8 pins finding 3's Go half: the byte budget
// is genuinely UTF-8 BYTES (len() on a Go string is bytes — no rune-count
// creep), mirroring the JS side, and the clamp never splits a multibyte rune.
// A CJK leaf (3 bytes/rune) far over the budget clamps to <= maxCoerceBytes
// BYTES and stays valid UTF-8.
func TestCoerceFlexValueByteBudgetUTF8(t *testing.T) {
	cjk := strings.Repeat("字", maxCoerceBytes) // ~3x the byte budget in runes
	body, _ := json.Marshal(map[string]string{"site": "chatgpt-web", "prompt_text": cjk})
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("over-budget CJK leaf must degrade, not error: %v", err)
	}
	if len(got.PromptText) > maxCoerceBytes {
		t.Fatalf("CJK leaf not byte-bounded: %d bytes, want <= %d (rune-count creep?)", len(got.PromptText), maxCoerceBytes)
	}
	if len(got.PromptText) == 0 {
		t.Fatalf("expected a non-empty prefix (truncate, not nuke)")
	}
	if !utf8.ValidString(got.PromptText) {
		t.Fatalf("byte clamp split a multibyte rune (emitted invalid UTF-8)")
	}
	// The clamp lands within one rune-width of the budget (backed off only a
	// partial trailing rune), proving it is a byte budget, not a rune budget.
	if maxCoerceBytes-len(got.PromptText) >= utf8.UTFMax {
		t.Fatalf("clamp %d bytes is more than one rune below the %d-byte budget", len(got.PromptText), maxCoerceBytes)
	}
}

// TestCoerceFlexValueAllocationBounded pins finding 1's core property: the walk
// allocates proportional to the ACCUMULATED BUDGET (~256 nodes), not to the
// input size. The old json.Unmarshal-into-[]json.RawMessage path materialized
// EVERY element of a huge flat array (and decoded each into a struct) before the
// 256-node cap could apply — ~10x the input. The streaming token walk stops
// reading the instant the node cap is hit, so a multi-MiB array is never fully
// consumed, let alone materialized.
func TestCoerceFlexValueAllocationBounded(t *testing.T) {
	// ~9.5 MiB of raw JSON: 500k message parts. Full materialization of this
	// would allocate tens of MiB; the streaming walk touches only ~256 nodes.
	const parts = 500000
	var b strings.Builder
	b.WriteString(`[`)
	for i := 0; i < parts; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"text","text":"xxxxxxxx"}`)
	}
	b.WriteByte(']')
	data := []byte(b.String())

	var (
		before, after runtime.MemStats
		f             flexString
	)
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := f.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON must degrade, not error: %v", err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	// Output is node-bounded (never all 500k parts joined).
	if n := strings.Count(string(f), "\n") + 1; n > maxCoerceParts {
		t.Fatalf("output not node-bounded: joined %d parts, want <= %d", n, maxCoerceParts)
	}
	if len(f) == 0 {
		t.Fatalf("expected a non-empty prefix (truncate, not nuke)")
	}
	// The decisive regression guard: allocation must be a small fraction of the
	// input. The old full-materialization path allocated MORE than the input;
	// the streaming walk allocates only what the ~256-node budget requires
	// (well under 1 MiB here). A generous cap of maxCoerceBytes keeps this
	// robust against decoder-buffer/GC noise while still failing loudly on a
	// regression to input-proportional materialization.
	if allocated > maxCoerceBytes {
		t.Fatalf("allocated %d bytes for a %d-byte input; want <= %d (budget-bounded, not input-proportional)",
			allocated, len(data), maxCoerceBytes)
	}

	// Nested-wrapper case (finding 1 re-review): the SAME huge array WRAPPED in
	// an object candidate — {"text":[…]}. The old json.RawMessage capture
	// decoded the whole wrapped array into raw bytes before any cap applied
	// (input-proportional again, plus another full subtree copy per nesting
	// level); the in-place slot walk must keep allocation bounded here too.
	var wb strings.Builder
	wb.WriteString(`{"text":[`)
	for i := 0; i < parts; i++ {
		if i > 0 {
			wb.WriteByte(',')
		}
		wb.WriteString(`{"type":"text","text":"xxxxxxxx"}`)
	}
	wb.WriteString(`]}`)
	wrapped := []byte(wb.String())

	var wf flexString
	runtime.GC()
	runtime.ReadMemStats(&before)
	if err := wf.UnmarshalJSON(wrapped); err != nil {
		t.Fatalf("wrapped UnmarshalJSON must degrade, not error: %v", err)
	}
	runtime.ReadMemStats(&after)
	wrappedAlloc := after.TotalAlloc - before.TotalAlloc

	if len(wf) == 0 {
		t.Fatalf("wrapped: expected a non-empty prefix (truncate, not nuke)")
	}
	if n := strings.Count(string(wf), "\n") + 1; n > maxCoerceParts {
		t.Fatalf("wrapped output not node-bounded: joined %d parts, want <= %d", n, maxCoerceParts)
	}
	if wrappedAlloc > maxCoerceBytes {
		t.Fatalf("object-wrapped array allocated %d bytes for a %d-byte input; want <= %d (the wrapped candidate must not be materialized)",
			wrappedAlloc, len(wrapped), maxCoerceBytes)
	}
}

// TestCoerceGlobalBudgetAcrossNesting pins finding 1's round-4 re-review
// property: the node + wire-byte budget is GLOBAL across the whole walk, NOT
// reset per object. The old design gave each object's text/content/parts slots
// a FRESH counter, so a nested fan-out promoted ~leavesPerLevel fragments AT
// EVERY level — total fragments grew without a global bound, and once the final
// strings.Join inserted a "\n" between each, the output exceeded the claimed
// wire-byte budget. This builds exactly that hostile shape (each object carries
// a big `content` array of leaf {text} parts PLUS one nested object of the same
// shape) and asserts the joined output stays GLOBALLY node-bounded and within
// the budget plus a small (newline-sized) constant.
func TestCoerceGlobalBudgetAcrossNesting(t *testing.T) {
	// D levels of {content:[<N leaves>, <nested>]}. A per-slot-reset budget
	// promotes ~N fragments per level → ~N*(D+1) fragments (well over
	// maxCoerceParts); the shared global budget caps total nodes — hence
	// fragments — at maxCoerceParts no matter how deep the fan-out goes.
	const (
		leavesPerLevel = 200
		levels         = 4
	)
	var build func(depth int) string
	build = func(depth int) string {
		var b strings.Builder
		b.WriteString(`{"content":[`)
		for i := 0; i < leavesPerLevel; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"type":"text","text":"x"}`)
		}
		if depth > 0 {
			b.WriteByte(',')
			b.WriteString(build(depth - 1))
		}
		b.WriteString(`]}`)
		return b.String()
	}
	body := []byte(`{"site":"chatgpt-web","prompt_text":` + build(levels) + `}`)

	got, err := Parse(body)
	if err != nil {
		t.Fatalf("nested fan-out must degrade, not error: %v", err)
	}
	if got.PromptText == "" {
		t.Fatalf("expected a non-empty prefix (truncate, not nuke)")
	}
	// GLOBALLY node-bounded: total joined fragments never exceed maxCoerceParts,
	// no matter how the fan-out nests (the old per-object reset produced
	// ~leavesPerLevel*(levels+1) here — far above the cap).
	if n := strings.Count(got.PromptText, "\n") + 1; n > maxCoerceParts {
		t.Fatalf("output not GLOBALLY node-bounded: joined %d fragments, want <= %d (per-object budget-reset regression?)", n, maxCoerceParts)
	}
	// Within the byte budget PLUS at most a maxCoerceParts-sized newline
	// constant: the join delimiters can never push a hostile nesting past the
	// claimed wire-byte budget (finding 1's assert — total accumulated wire
	// bytes stay <= budget + small constant across any hostile nesting).
	if len(got.PromptText) > maxCoerceBytes+maxCoerceParts {
		t.Fatalf("output %d bytes exceeds budget %d + small constant %d (unbudgeted-join regression?)", len(got.PromptText), maxCoerceBytes, maxCoerceParts)
	}
}

// TestCoerceWireByteBudget pins finding 2's Go half: the coercion budget counts
// JSON-WIRE bytes (the re-marshalled serialized size), not raw len(), mirroring
// the JS jsonWireByteLen budget. A backslash-heavy field serializes to ~2x its
// raw length, so a raw-byte budget would keep twice what fits the wire.
func TestCoerceWireByteBudget(t *testing.T) {
	// Each backslash costs 2 JSON-wire bytes. Feed far more than the budget.
	backslashes := strings.Repeat("\\", maxCoerceBytes)
	body, err := json.Marshal(map[string]string{"site": "chatgpt-web", "prompt_text": backslashes})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	got, err := Parse(body)
	if err != nil {
		t.Fatalf("backslash-heavy field must degrade, not error: %v", err)
	}
	if got.PromptText == "" {
		t.Fatalf("expected a non-empty prefix (truncate, not nuke)")
	}
	if wire := jsonWireCost(got.PromptText); wire > maxCoerceBytes {
		t.Fatalf("coerced field wire cost %d exceeds budget %d (raw-byte budget regression?)", wire, maxCoerceBytes)
	}
	// The clamp is genuinely by WIRE cost: the kept RAW length is well under the
	// budget (each kept backslash spends 2 wire bytes), proving we did not keep
	// a raw-byte-budget's worth of a 2x-escaping field.
	if len(got.PromptText) >= maxCoerceBytes {
		t.Fatalf("kept %d raw bytes; a wire-cost clamp should keep ~half the budget for a 2x-escaping field", len(got.PromptText))
	}
	// Round-trip safety: the clamp never split a rune (trivially true for ASCII
	// here, but the guard mirrors the UTF-8 test).
	if !utf8.ValidString(got.PromptText) {
		t.Fatalf("wire-cost clamp emitted invalid UTF-8")
	}
}

// TestCoercePrecedenceKeyOrderIndependent pins finding 5's fix: text>content>
// parts must NOT depend on JSON KEY ORDER. The round-4 design SHARED one budget
// across the three precedence slots, so a lower-precedence key walked FIRST
// (many `parts`, then `content`) could exhaust the budget before the streaming
// decoder ever reached `text` — the object loop broke on the shared stopped flag
// and precedence fell back to the partial lower-precedence slot, returning
// joined `content` where a materialized reader (the JS side, and round-3 Go)
// would return the `text` winner. Independent per-slot budgets restore key-order
// independence: each slot is under its OWN maxCoerceParts budget, so all three
// are collected and `text` wins regardless of source order.
func TestCoercePrecedenceKeyOrderIndependent(t *testing.T) {
	// Each of parts/content is UNDER its own node budget, but their COMBINED node
	// count exceeds a single shared budget — the exact shape that made the winner
	// depend on key order under the round-4 shared budget.
	const perSlot = 200 // < maxCoerceParts each; parts+content = 400 > maxCoerceParts
	if perSlot >= maxCoerceParts || 2*perSlot <= maxCoerceParts {
		t.Fatalf("test fixture invariants broken: perSlot=%d maxCoerceParts=%d", perSlot, maxCoerceParts)
	}
	arr := func(s string) string {
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < perSlot; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('"')
			b.WriteString(s)
			b.WriteByte('"')
		}
		b.WriteByte(']')
		return b.String()
	}
	partsArr, contentArr := arr("p"), arr("c")

	// Forward order: parts, then content, then text LAST — the regression case.
	// The shared-budget round-4 walk exhausts before `text` and returns partial
	// `content`; the fix reaches `text` and returns the winner.
	forward := `{"site":"chatgpt-web","prompt_text":{"parts":` + partsArr +
		`,"content":` + contentArr + `,"text":"winner"}}`
	got, err := Parse([]byte(forward))
	if err != nil {
		t.Fatalf("forward: Parse must degrade, not error: %v", err)
	}
	if got.PromptText != "winner" {
		trunc := got.PromptText
		if len(trunc) > 40 {
			trunc = trunc[:40] + "..."
		}
		t.Fatalf("forward (parts,content,text): PromptText = %q, want %q (key-order-dependent shared-budget regression?)", trunc, "winner")
	}

	// Reverse order: text FIRST — always worked (text is walked before the budget
	// could be spent), so this asserts UNCHANGED behavior, still "winner".
	reverse := `{"site":"chatgpt-web","prompt_text":{"text":"winner","content":` + contentArr +
		`,"parts":` + partsArr + `}}`
	got, err = Parse([]byte(reverse))
	if err != nil {
		t.Fatalf("reverse: Parse must degrade, not error: %v", err)
	}
	if got.PromptText != "winner" {
		t.Fatalf("reverse (text,content,parts): PromptText = %q, want %q (unchanged behavior)", got.PromptText, "winner")
	}
}

// TestNormalizeArrayPromptTextStillCaptures reproduces the exact live drop —
// a copilot-web turn whose prompt_text arrived as an array of message parts —
// and asserts the whole Normalize pipeline now captures it (a joined-string
// user_prompt event) rather than failing the turn.
func TestNormalizeArrayPromptTextStillCaptures(t *testing.T) {
	body := []byte(`{
		"site":"copilot-web",
		"conversation_id":"conv-1",
		"message_id":"msg-1",
		"granularity":"full",
		"prompt_text":[{"type":"text","text":"What is the"},{"type":"text","text":"capital of Japan?"}],
		"response_text":"Tokyo."
	}`)
	evs, toks, err := Normalize(body, scrub.New())
	if err != nil {
		t.Fatalf("Normalize must capture the turn, got error: %v", err)
	}
	up, ok := findEvent(evs, models.ActionUserPrompt)
	if !ok {
		t.Fatalf("expected a user_prompt event")
	}
	if up.RawToolInput != "What is the\ncapital of Japan?" {
		t.Errorf("user_prompt RawToolInput = %q, want the joined string", up.RawToolInput)
	}
	if len(toks) == 0 {
		t.Errorf("expected a token event for a content-bearing turn")
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"12345678", 2},
	}
	for _, tc := range tests {
		if got := estimateTokens(tc.in); got != tc.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResolveTimestamp(t *testing.T) {
	rfc := resolveTimestamp("2026-07-10T12:00:00Z")
	if rfc.Year() != 2026 || rfc.Month() != time.July || rfc.Day() != 10 {
		t.Errorf("RFC3339 parse wrong: %v", rfc)
	}
	unix := resolveTimestamp("1780000000")
	if unix.Unix() != 1780000000 {
		t.Errorf("unix parse wrong: %v", unix)
	}
	if resolveTimestamp("").IsZero() {
		t.Errorf("empty should fall back to now, not zero")
	}
	if resolveTimestamp("garbage").IsZero() {
		t.Errorf("garbage should fall back to now, not zero")
	}
}

func TestMessageIDSynthesizedWhenAbsent(t *testing.T) {
	// No message id, but a capture_id present → the message id derives from
	// the capture_id ("cap:" prefix) so tool + token events agree.
	turn := chatGPTTurn()
	turn.MessageID = ""
	if got := messageID(turn); got != "cap:"+turn.CaptureID {
		t.Errorf("messageID = %q, want %q", got, "cap:"+turn.CaptureID)
	}
	// No message id AND no capture_id (old bridge) → the captured_at
	// last-resort stamp, prefixed with 't'.
	turn.CaptureID = ""
	if got := messageID(turn); got == "" {
		t.Errorf("expected a synthesized message id")
	} else if got[0] != 't' {
		t.Errorf("synthesized id should start with 't', got %q", got)
	}
}

// TestSessionKeyFor pins the fallback ladder: a real conversation id wins; a
// bare message id is the next fallback; then the opaque capture_id
// ("<site>:cap:<id>"); and finally, for an old bridge with none of those, the
// content-free "<site>:<captured_at_ms>" last-resort (never dropped).
func TestSessionKeyFor(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CapturedTurn)
		want   string
	}{
		{
			name:   "conversation id wins",
			mutate: func(c *CapturedTurn) {},
			want:   "conv-123",
		},
		{
			name:   "message id is the fallback when conversation id absent",
			mutate: func(c *CapturedTurn) { c.ConversationID = "" },
			want:   "msg-abc",
		},
		{
			name: "capture_id is the fallback when conversation + message ids absent",
			mutate: func(c *CapturedTurn) {
				c.ConversationID = ""
				c.MessageID = ""
			},
			want: models.ToolChatGPTWeb + ":cap:cap-xyz",
		},
		{
			name: "captured_at last-resort when even capture_id is absent (old bridge)",
			mutate: func(c *CapturedTurn) {
				c.ConversationID = ""
				c.MessageID = ""
				c.CaptureID = ""
			},
			// captured_at 2026-07-10T12:00:00Z anchors the content-free
			// last-resort key.
			want: func() string {
				c := chatGPTTurn()
				c.ConversationID = ""
				c.MessageID = ""
				c.CaptureID = ""
				return c.Site + ":" + lastResortStamp(c)
			}(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := chatGPTTurn()
			tc.mutate(&turn)
			if got := sessionKeyFor(turn); got != tc.want {
				t.Errorf("sessionKeyFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIdlessTurnSharesSessionKey is the critical B1 invariant: an id-less
// turn is INGESTED (never dropped), and its tool + token events land in the
// SAME synthesized session — sessionKeyFor is the one owner both derive from.
func TestIdlessTurnSharesSessionKey(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CapturedTurn)
		wantKey func(CapturedTurn) string
	}{
		{
			name: "no conversation id, message id present",
			mutate: func(c *CapturedTurn) {
				c.ConversationID = ""
			},
			wantKey: func(c CapturedTurn) string { return c.MessageID },
		},
		{
			name: "no conversation id, no message id, capture_id groups the turn",
			mutate: func(c *CapturedTurn) {
				c.ConversationID = ""
				c.MessageID = ""
			},
			wantKey: func(c CapturedTurn) string {
				return c.Site + ":cap:" + c.CaptureID
			},
		},
		{
			name: "no conversation id, no message id, no capture_id (old bridge, last-resort)",
			mutate: func(c *CapturedTurn) {
				c.ConversationID = ""
				c.MessageID = ""
				c.CaptureID = ""
			},
			wantKey: func(c CapturedTurn) string {
				return c.Site + ":" + lastResortStamp(c)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := chatGPTTurn()
			tc.mutate(&turn)
			want := tc.wantKey(turn)

			ev, ok, err := BuildToolEvent(turn, scrub.New())
			if err != nil || !ok {
				t.Fatalf("BuildToolEvent: ok=%v err=%v (id-less turn must be ingested, not dropped)", ok, err)
			}
			tok, ok, err := BuildTokenEvent(turn)
			if err != nil || !ok {
				t.Fatalf("BuildTokenEvent: ok=%v err=%v", ok, err)
			}
			if ev.SessionID != want {
				t.Errorf("tool SessionID = %q, want %q", ev.SessionID, want)
			}
			if tok.SessionID != want {
				t.Errorf("token SessionID = %q, want %q", tok.SessionID, want)
			}
			if ev.SessionID != tok.SessionID {
				t.Fatalf("CRITICAL: tool/token session keys diverge: %q vs %q", ev.SessionID, tok.SessionID)
			}
			// The dedup keys must still differ (token appends ":tok").
			if ev.SourceEventID == tok.SourceEventID {
				t.Errorf("tool + token SourceEventID collide: %q", ev.SourceEventID)
			}
		})
	}
}

// TestSyntheticKeyDistinctForDifferentCaptureID is the MED-1 core in its real
// wire shape: two id-less turns captured in the SAME millisecond with IDENTICAL
// content but DIFFERENT capture_id get DIFFERENT synthesized keys (and dedup
// ids), so neither is silently deduped away as a MAX-upgrade duplicate of the
// other. This is the case the old content fingerprint got wrong — at the
// default usage_only granularity the prompt/response are stripped, so a
// content hash was constant per-site and distinct id-less turns collided
// (MED-1). capture_id is opaque and per-turn, so it survives that stripping.
func TestSyntheticKeyDistinctForDifferentCaptureID(t *testing.T) {
	// Same content, same capture millisecond — only capture_id differs.
	a := chatGPTTurn()
	a.ConversationID, a.MessageID = "", ""
	a.CaptureID = "cap-A"

	b := chatGPTTurn()
	b.ConversationID, b.MessageID = "", ""
	b.CapturedAt = a.CapturedAt // identical capture millisecond
	b.CaptureID = "cap-B"

	if sessionKeyFor(a) == sessionKeyFor(b) {
		t.Errorf("distinct-capture_id same-ms turns collided on session key: %q", sessionKeyFor(a))
	}
	if sourceEventID(a) == sourceEventID(b) {
		t.Errorf("distinct-capture_id same-ms turns collided on dedup id: %q", sourceEventID(a))
	}
	// The same turn (same capture_id) is stable: an idempotent re-fire dedups
	// against itself.
	aAgain := a
	if sessionKeyFor(a) != sessionKeyFor(aAgain) || sourceEventID(a) != sourceEventID(aAgain) {
		t.Errorf("identical turn produced unstable keys")
	}
}

// TestLastResortKeyContentFree pins that the old-bridge last-resort key
// (neither conversation/message id NOR capture_id) is derived ONLY from
// captured_at — it embeds NO prompt/response content, so nothing
// content-derived can cross the org-push privacy boundary (MED-3). Two turns
// with the same captured_at and no ids collide (accepted legacy degradation),
// and the key never contains the prompt or response text.
func TestLastResortKeyContentFree(t *testing.T) {
	turn := chatGPTTurn()
	turn.ConversationID, turn.MessageID, turn.CaptureID = "", "", ""
	turn.PromptText = "a secret-ish prompt only this turn has"
	turn.ResponseText = "a distinctive response body"

	key := sessionKeyFor(turn)
	if strings.Contains(key, "secret-ish") || strings.Contains(key, "distinctive") {
		t.Errorf("last-resort session key leaked content: %q", key)
	}
	if strings.Contains(sourceEventID(turn), "secret-ish") || strings.Contains(sourceEventID(turn), "distinctive") {
		t.Errorf("last-resort dedup id leaked content: %q", sourceEventID(turn))
	}
	// captured_at alone anchors it; a second turn with different content but
	// the same captured_at shares the key (the accepted legacy collision).
	other := chatGPTTurn()
	other.ConversationID, other.MessageID, other.CaptureID = "", "", ""
	other.PromptText, other.ResponseText = "totally different", "also different"
	if sessionKeyFor(other) != key {
		t.Errorf("same-captured_at last-resort keys should match: %q vs %q", sessionKeyFor(other), key)
	}
}

// TestSyntheticKeyDeterministicWithoutCapturedAt is the other half of MED-1:
// with NO captured_at, every call for one turn — sessionKeyFor / messageID /
// sourceEventID, and the token event's copies of them — agrees, because the
// key derives from the opaque per-turn capture_id (or, for an old bridge, a
// captured_at stamp that reads 0), NOT a per-call time.Now() that could cross
// a millisecond boundary between the tool event and the token event.
func TestSyntheticKeyDeterministicWithoutCapturedAt(t *testing.T) {
	turn := chatGPTTurn()
	turn.ConversationID, turn.MessageID, turn.CapturedAt = "", "", ""

	// Repeated derivations agree (no wall-clock in the key).
	k1, k2 := sessionKeyFor(turn), sessionKeyFor(turn)
	if k1 != k2 {
		t.Fatalf("session key not deterministic without captured_at: %q vs %q", k1, k2)
	}

	// The tool and token events (the two rows of ONE turn) MUST share the key.
	ev, ok, err := BuildToolEvent(turn, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
	}
	tok, ok, err := BuildTokenEvent(turn)
	if err != nil || !ok {
		t.Fatalf("BuildTokenEvent: ok=%v err=%v", ok, err)
	}
	if ev.SessionID != tok.SessionID {
		t.Errorf("CRITICAL: tool/token session keys diverge without captured_at: %q vs %q", ev.SessionID, tok.SessionID)
	}
	if ev.SourceEventID == tok.SourceEventID {
		t.Errorf("tool + token dedup ids must differ (the :tok suffix): %q", ev.SourceEventID)
	}
	if ev.SourceEventID+":tok" != tok.SourceEventID {
		t.Errorf("token dedup id = %q, want tool id + :tok (%q)", tok.SourceEventID, ev.SourceEventID+":tok")
	}
}

// TestIsSyntheticSessionKey pins the LOW-1 tier rule the capture-telemetry
// counter reads: synthetic ONLY when neither id is present.
func TestIsSyntheticSessionKey(t *testing.T) {
	tests := []struct {
		conv, msg string
		want      bool
	}{
		{"conv-1", "msg-1", false},
		{"conv-1", "", false},
		{"", "msg-1", false}, // grouped by a real message id → NOT synthetic
		{"", "", true},       // fully id-less → synthetic
	}
	for _, tc := range tests {
		if got := IsSyntheticSessionKey(tc.conv, tc.msg); got != tc.want {
			t.Errorf("IsSyntheticSessionKey(%q,%q) = %v, want %v", tc.conv, tc.msg, got, tc.want)
		}
	}
}

// TestNormalizeIdlessTurnIngests proves the id-less turn survives the full
// Normalize seam ingestBrowserTurn calls — the tool events + token event all
// keyed to the same synthesized session. The default fixture is full
// granularity, so a user_prompt + assistant event are emitted.
func TestNormalizeIdlessTurnIngests(t *testing.T) {
	turn := chatGPTTurn()
	turn.ConversationID = ""
	turn.MessageID = ""
	body, _ := json.Marshal(turn)
	toolEvents, tokenEvents, err := Normalize(body, scrub.New())
	if err != nil {
		t.Fatalf("Normalize: %v (id-less turn must not error)", err)
	}
	if len(toolEvents) != 2 || len(tokenEvents) != 1 {
		t.Fatalf("events = (%d tool, %d token), want (2, 1)", len(toolEvents), len(tokenEvents))
	}
	want := tokenEvents[0].SessionID
	if want == "" {
		t.Errorf("synthesized session id must be non-empty")
	}
	for _, e := range toolEvents {
		if e.SessionID != want {
			t.Errorf("tool/token session keys diverge: %q vs %q", e.SessionID, want)
		}
	}
}

// TestMultiSiteNormalize pins that every registered *-web site normalizes
// through the SAME site-agnostic path — the site is a DATA discriminator,
// not a code branch (CLAUDE.md #3).
func TestMultiSiteNormalize(t *testing.T) {
	tests := []struct {
		site        string
		wantTool    string
		wantProject string
	}{
		{models.ToolChatGPTWeb, "chatgpt-web", "browser://chatgpt.com"},
		{models.ToolClaudeWeb, "claude-web", "browser://claude.ai"},
		{models.ToolPerplexityWeb, "perplexity-web", "browser://perplexity.ai"},
		{models.ToolGeminiWeb, "gemini-web", "browser://gemini.google.com"},
		{models.ToolCopilotWeb, "copilot-web", "browser://copilot.microsoft.com"},
	}
	for _, tc := range tests {
		t.Run(tc.site, func(t *testing.T) {
			turn := chatGPTTurn()
			turn.Site = tc.site
			turn.Model = "" // force the per-site default
			body, _ := json.Marshal(turn)
			toolEvents, tokenEvents, err := Normalize(body, scrub.New())
			if err != nil {
				t.Fatalf("Normalize(%s): %v", tc.site, err)
			}
			// Full-granularity fixture → user_prompt + assistant.
			if len(toolEvents) != 2 {
				t.Fatalf("toolEvents = %d, want 2", len(toolEvents))
			}
			for _, e := range toolEvents {
				if e.Tool != tc.wantTool {
					t.Errorf("Tool = %q, want %q", e.Tool, tc.wantTool)
				}
				if e.ProjectRoot != tc.wantProject {
					t.Errorf("ProjectRoot = %q, want %q", e.ProjectRoot, tc.wantProject)
				}
				if e.Model == "" {
					t.Errorf("expected a per-site default model, got empty")
				}
			}
			if len(tokenEvents) != 1 {
				t.Errorf("tokenEvents = %d, want 1", len(tokenEvents))
			}
		})
	}
}

// TestGranularityCeilingClamp pins the §5.1 daemon-ceiling: the effective
// granularity is min(client, ceiling), so a "full" turn is downgraded when
// the daemon caps at usage_only. At content granularity the turn yields two
// events (user_prompt + assistant); clamped to usage_only it yields one
// content-free assistant event.
//
// The full client × ceiling matrix, with the HIGH honesty fix: a client that
// sent "full" but is capped at a "redacted" ceiling must NOT store its raw
// content under a "redacted" label — the daemon has no redactor, so the label
// would be a lie. That one cell downgrades to usage_only (content dropped,
// usage kept) instead of the old mislabel-as-redacted behaviour.
func TestGranularityCeilingClamp(t *testing.T) {
	tests := []struct {
		name          string
		clientGran    string
		ceiling       Granularity
		wantContent   bool
		wantEvents    int
		wantEffective Granularity
	}{
		{"no ceiling keeps full", string(GranularityFull), "", true, 2, GranularityFull},
		{"no ceiling keeps redacted", string(GranularityRedacted), "", true, 2, GranularityRedacted},
		{"no ceiling keeps usage_only", string(GranularityUsageOnly), "", false, 1, GranularityUsageOnly},

		{"ceiling usage_only downgrades full", string(GranularityFull), GranularityUsageOnly, false, 1, GranularityUsageOnly},
		{"ceiling usage_only downgrades redacted", string(GranularityRedacted), GranularityUsageOnly, false, 1, GranularityUsageOnly},
		{"ceiling usage_only keeps usage_only", string(GranularityUsageOnly), GranularityUsageOnly, false, 1, GranularityUsageOnly},

		// HIGH fix: full clamped to a redacted ceiling drops content (was:
		// stored raw content mislabeled "redacted").
		{"ceiling redacted DROPS full to usage_only (no fabricated redaction)", string(GranularityFull), GranularityRedacted, false, 1, GranularityUsageOnly},
		{"ceiling redacted keeps redacted (client actually redacted)", string(GranularityRedacted), GranularityRedacted, true, 2, GranularityRedacted},
		{"ceiling redacted keeps usage_only", string(GranularityUsageOnly), GranularityRedacted, false, 1, GranularityUsageOnly},

		{"ceiling full keeps full", string(GranularityFull), GranularityFull, true, 2, GranularityFull},
		{"ceiling full keeps redacted", string(GranularityRedacted), GranularityFull, true, 2, GranularityRedacted},
		{"client usage_only stays usage_only under full ceiling", string(GranularityUsageOnly), GranularityFull, false, 1, GranularityUsageOnly},

		{"unknown ceiling fails safe to usage_only", string(GranularityFull), Granularity("bogus"), false, 1, GranularityUsageOnly},

		// Empty/unknown client resolves to the usage_only floor upstream, so
		// it never reaches the redacted-mislabel path — the conservative path
		// holds even under a permissive ceiling.
		{"empty client under full ceiling floors to usage_only", "", GranularityFull, false, 1, GranularityUsageOnly},
		{"empty client under redacted ceiling floors to usage_only", "", GranularityRedacted, false, 1, GranularityUsageOnly},
		{"unknown client under full ceiling floors to usage_only", "bogus", GranularityFull, false, 1, GranularityUsageOnly},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			turn := chatGPTTurn()
			turn.Granularity = tc.clientGran
			body, _ := json.Marshal(turn)
			toolEvents, _, err := NormalizeWith(body, Options{
				Scrubber:           scrub.New(),
				GranularityCeiling: tc.ceiling,
			})
			if err != nil {
				t.Fatalf("NormalizeWith: %v", err)
			}
			if len(toolEvents) != tc.wantEvents {
				t.Fatalf("toolEvents = %d, want %d", len(toolEvents), tc.wantEvents)
			}
			hasContent := false
			for _, e := range toolEvents {
				if e.RawToolInput != "" || e.ToolOutput != "" {
					hasContent = true
				}
			}
			if hasContent != tc.wantContent {
				t.Errorf("hasContent = %v, want %v", hasContent, tc.wantContent)
			}
			// The assistant event's metadata must record the EFFECTIVE
			// (post-clamp, post-honesty-downgrade) granularity — the label a
			// downstream reader trusts to mean "this is how the content was
			// handled".
			assistant, ok := findEvent(toolEvents, models.ActionAssistantMessage)
			if !ok || assistant.Metadata == nil {
				t.Fatalf("no assistant event with metadata (ok=%v)", ok)
			}
			if assistant.Metadata.Granularity != string(tc.wantEffective) {
				t.Errorf("effective granularity = %q, want %q", assistant.Metadata.Granularity, tc.wantEffective)
			}
		})
	}
}

func TestNewForRegistersSite(t *testing.T) {
	a := NewFor(models.ToolClaudeWeb)
	if a.Name() != models.ToolClaudeWeb {
		t.Errorf("Name = %q, want %q", a.Name(), models.ToolClaudeWeb)
	}
	if a.WatchPaths() != nil {
		t.Errorf("WatchPaths must be nil (hook-only)")
	}
}

func TestSourceFileFor(t *testing.T) {
	if got := SourceFileFor(models.ToolChatGPTWeb); got != "chatgpt-web:extension" {
		t.Errorf("SourceFileFor = %q, want chatgpt-web:extension", got)
	}
}

func TestAdapter(t *testing.T) {
	a := New()
	if a.Name() != models.ToolChatGPTWeb {
		t.Errorf("Name = %q, want %q", a.Name(), models.ToolChatGPTWeb)
	}
	if a.WatchPaths() != nil {
		t.Errorf("WatchPaths should be nil (hook-only)")
	}
	if a.IsSessionFile("/tmp/anything.json") {
		t.Errorf("IsSessionFile should always be false")
	}
	res, err := a.ParseSessionFile(context.Background(), "/tmp/x", 42)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if res.NewOffset != 42 {
		t.Errorf("NewOffset = %d, want 42 (unchanged)", res.NewOffset)
	}
}

// TestParseIDSource proves the additive id_source field (JS LOW fix) is parsed
// into CapturedTurn rather than silently dropped by encoding/json.
func TestParseIDSource(t *testing.T) {
	body := []byte(`{"schema_version":1,"site":"chatgpt-web","conversation_id":"c1","id_source":"resume"}`)
	turn, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if turn.IDSource != "resume" {
		t.Errorf("IDSource = %q, want resume", turn.IDSource)
	}
	// A payload omitting the field leaves the zero value (backward-compat).
	turn2, err := Parse([]byte(`{"schema_version":1,"site":"chatgpt-web","conversation_id":"c1"}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if turn2.IDSource != "" {
		t.Errorf("IDSource = %q, want empty for an omitting payload", turn2.IDSource)
	}
}

// TestNormalizeIDSource pins the FINDING 2 fix directly against the pure
// helper: every value the extension actually emits today (see
// browser-extension/src/parsers.js::resolveIdSource and
// content-main.js::emitTurn's idSource override, "resume") round-trips
// unchanged, while empty, garbage, and oversized values all collapse to
// "none" — the value browser-health telemetry already recognizes as "no real
// conversation-id provenance" (cmd/observer/browser.go peekBrowserTurn).
func TestNormalizeIDSource(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"none passes through", "none", "none"},
		{"request passes through", "request", "request"},
		{"stream passes through", "stream", "stream"},
		{"resume passes through", "resume", "resume"},
		{"url passes through", "url", "url"},
		{"chain passes through", "chain", "chain"},
		{"empty maps to none", "", "none"},
		{"unknown word maps to none", "bogus", "none"},
		{"case-sensitive mismatch maps to none", "Request", "none"},
		{"whitespace-padded valid value maps to none (not trimmed/fuzzy-matched)", " request", "none"},
		{"malformed/injected text maps to none", "<script>alert(1)</script>", "none"},
		{"very long garbage maps to none", strings.Repeat("x", 5000), "none"},
		{"exactly at the length cap but not in the enum maps to none", strings.Repeat("a", idSourceMaxLen), "none"},
		{"json-ish garbage maps to none", `{"foo":"bar"}`, "none"},
		{"null-byte-bearing value maps to none", "req\x00uest", "none"},
		{"numeric string maps to none", "123", "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeIDSource(tc.in); got != tc.want {
				t.Errorf("normalizeIDSource(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildMetadataNormalizesGarbageIDSource proves the normalization is
// actually wired into storage: a CapturedTurn carrying a malformed/stale
// id_source (as a corrupted bridge might send) is stored as "none" in the
// action metadata, at every granularity including usage_only — id_source is
// metadata, not content, so it must never be dropped by a low granularity
// ceiling, and it must never carry the raw garbage through to the DB.
func TestBuildMetadataNormalizesGarbageIDSource(t *testing.T) {
	for _, gran := range []Granularity{GranularityUsageOnly, GranularityRedacted, GranularityFull} {
		t.Run(string(gran), func(t *testing.T) {
			turn := chatGPTTurn()
			turn.IDSource = "totally-unrecognized-garbage-value"
			turn.Granularity = string(gran)
			ev, ok, err := BuildToolEvent(turn, scrub.New())
			if err != nil || !ok {
				t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
			}
			if ev.Metadata == nil {
				t.Fatalf("Metadata is nil at granularity %s", gran)
			}
			if ev.Metadata.IDSource != "none" {
				t.Errorf("IDSource = %q, want normalized to %q at granularity %s", ev.Metadata.IDSource, "none", gran)
			}
		})
	}
}
