package browserchat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	}
}

func TestBuildToolEvent(t *testing.T) {
	sc := scrub.New()
	tests := []struct {
		name          string
		mutate        func(*CapturedTurn)
		wantOK        bool
		wantErr       bool
		wantInput     string // "" ⇒ expect empty RawToolInput
		wantOutputSet bool   // expect a non-empty ToolOutput
	}{
		{
			name:          "full granularity carries content",
			mutate:        func(c *CapturedTurn) { c.Granularity = string(GranularityFull) },
			wantOK:        true,
			wantInput:     "what is the capital of France?",
			wantOutputSet: true,
		},
		{
			name:          "redacted granularity carries content",
			mutate:        func(c *CapturedTurn) { c.Granularity = string(GranularityRedacted) },
			wantOK:        true,
			wantInput:     "what is the capital of France?",
			wantOutputSet: true,
		},
		{
			name:          "usage-only constructs no content",
			mutate:        func(c *CapturedTurn) { c.Granularity = string(GranularityUsageOnly) },
			wantOK:        true,
			wantInput:     "",
			wantOutputSet: false,
		},
		{
			name:          "empty granularity defaults to usage-only",
			mutate:        func(c *CapturedTurn) { c.Granularity = "" },
			wantOK:        true,
			wantInput:     "",
			wantOutputSet: false,
		},
		{
			name:          "unknown granularity defaults to usage-only",
			mutate:        func(c *CapturedTurn) { c.Granularity = "bogus" },
			wantOK:        true,
			wantInput:     "",
			wantOutputSet: false,
		},
		{
			name:    "unknown site is an error",
			mutate:  func(c *CapturedTurn) { c.Site = "definitely-not-a-site" },
			wantErr: true,
		},
		{
			name:    "missing conversation id is an error",
			mutate:  func(c *CapturedTurn) { c.ConversationID = "" },
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
			if ev.RawToolInput != tc.wantInput {
				t.Errorf("RawToolInput = %q, want %q", ev.RawToolInput, tc.wantInput)
			}
			if tc.wantOutputSet && ev.ToolOutput == "" {
				t.Errorf("expected non-empty ToolOutput")
			}
			if !tc.wantOutputSet && ev.ToolOutput != "" {
				t.Errorf("expected empty ToolOutput, got %q", ev.ToolOutput)
			}
		})
	}
}

// TestServerScrubBackstopFullGranularity pins the §6.2 server backstop: the
// client-side redaction pipeline is the PRIMARY control, but any content that
// crosses the wire at full (or redacted) granularity MUST still be scrubbed
// by the ingest-time scrub.Scrubber before it is stored. A full-granularity
// payload whose prompt/response embed secret-shaped strings must land in the
// normalized ToolEvent (RawToolInput / ToolOutput — the exact bytes handed to
// store.Ingest) with those secrets replaced by the scrubber's [REDACTED]
// sentinel, never in the clear. This asserts the Phase-1 wiring
// (`scrub.New()` at ingest, Scrubber.String, NOT ScrubForward) is not
// regressed by the Phase-3 client-side redaction work.
func TestServerScrubBackstopFullGranularity(t *testing.T) {
	// Secret-shaped fixtures assembled by concatenation so no contiguous
	// credential literal appears in the source file.
	ghToken := "gh" + "p_" + strings.Repeat("A", 32)
	awsKey := "AKIA" + "ABCDEFGHIJKLMNOP"

	turn := chatGPTTurn()
	turn.Granularity = string(GranularityFull)
	turn.PromptText = "here is my token " + ghToken + " please use it"
	turn.ResponseText = "your aws key " + awsKey + " is noted"

	// Path 1: the exported BuildToolEvent (no ceiling).
	ev, ok, err := BuildToolEvent(turn, scrub.New())
	if err != nil || !ok {
		t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
	}
	if strings.Contains(ev.RawToolInput, ghToken) {
		t.Errorf("RawToolInput leaked the raw token: %q", ev.RawToolInput)
	}
	if !strings.Contains(ev.RawToolInput, scrub.Redacted) {
		t.Errorf("RawToolInput not scrubbed (no %s sentinel): %q", scrub.Redacted, ev.RawToolInput)
	}
	if strings.Contains(ev.ToolOutput, awsKey) {
		t.Errorf("ToolOutput leaked the raw AWS key: %q", ev.ToolOutput)
	}
	if !strings.Contains(ev.ToolOutput, scrub.Redacted) {
		t.Errorf("ToolOutput not scrubbed (no %s sentinel): %q", scrub.Redacted, ev.ToolOutput)
	}

	// Path 2: the full NormalizeWith seam ingestBrowserTurn actually calls,
	// at full granularity with no daemon ceiling — same backstop must hold.
	body, _ := json.Marshal(turn)
	toolEvents, _, err := NormalizeWith(body, Options{Scrubber: scrub.New()})
	if err != nil {
		t.Fatalf("NormalizeWith: %v", err)
	}
	if len(toolEvents) != 1 {
		t.Fatalf("toolEvents = %d, want 1", len(toolEvents))
	}
	if strings.Contains(toolEvents[0].RawToolInput, ghToken) ||
		strings.Contains(toolEvents[0].ToolOutput, awsKey) {
		t.Errorf("NormalizeWith leaked a raw secret: input=%q output=%q",
			toolEvents[0].RawToolInput, toolEvents[0].ToolOutput)
	}
	if !strings.Contains(toolEvents[0].RawToolInput, scrub.Redacted) ||
		!strings.Contains(toolEvents[0].ToolOutput, scrub.Redacted) {
		t.Errorf("NormalizeWith did not scrub: input=%q output=%q",
			toolEvents[0].RawToolInput, toolEvents[0].ToolOutput)
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

func TestBuildToolEventNilScrubber(t *testing.T) {
	turn := chatGPTTurn()
	ev, ok, err := BuildToolEvent(turn, nil)
	if err != nil || !ok {
		t.Fatalf("BuildToolEvent: ok=%v err=%v", ok, err)
	}
	if ev.RawToolInput == "" {
		t.Errorf("expected content with a nil scrubber (no scrubbing, still stored)")
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
		{
			name:    "missing conversation id is an error",
			mutate:  func(c *CapturedTurn) { c.ConversationID = "" },
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
	if len(toolEvents) != 1 {
		t.Fatalf("toolEvents = %d, want 1", len(toolEvents))
	}
	if len(tokenEvents) != 1 {
		t.Fatalf("tokenEvents = %d, want 1", len(tokenEvents))
	}
	// SourceEventIDs must differ (dedup keys) and share the turn key.
	if toolEvents[0].SourceEventID == tokenEvents[0].SourceEventID {
		t.Errorf("tool + token SourceEventID collide: %q", toolEvents[0].SourceEventID)
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
	turn := chatGPTTurn()
	turn.MessageID = ""
	if got := messageID(turn); got == "" {
		t.Errorf("expected a synthesized message id")
	} else if got[0] != 't' {
		t.Errorf("synthesized id should start with 't', got %q", got)
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
			if len(toolEvents) != 1 {
				t.Fatalf("toolEvents = %d, want 1", len(toolEvents))
			}
			if toolEvents[0].Tool != tc.wantTool {
				t.Errorf("Tool = %q, want %q", toolEvents[0].Tool, tc.wantTool)
			}
			if toolEvents[0].ProjectRoot != tc.wantProject {
				t.Errorf("ProjectRoot = %q, want %q", toolEvents[0].ProjectRoot, tc.wantProject)
			}
			if toolEvents[0].Model == "" {
				t.Errorf("expected a per-site default model, got empty")
			}
			if len(tokenEvents) != 1 {
				t.Errorf("tokenEvents = %d, want 1", len(tokenEvents))
			}
		})
	}
}

// TestGranularityCeilingClamp pins the §5.1 daemon-ceiling: the effective
// granularity is min(client, ceiling), so a "full" turn is downgraded when
// the daemon caps at usage_only.
func TestGranularityCeilingClamp(t *testing.T) {
	tests := []struct {
		name        string
		clientGran  string
		ceiling     Granularity
		wantContent bool
	}{
		{"no ceiling keeps full", string(GranularityFull), "", true},
		{"ceiling usage_only downgrades full", string(GranularityFull), GranularityUsageOnly, false},
		{"ceiling redacted keeps redacted", string(GranularityRedacted), GranularityRedacted, true},
		{"ceiling redacted downgrades full to redacted (still content)", string(GranularityFull), GranularityRedacted, true},
		{"ceiling full keeps full", string(GranularityFull), GranularityFull, true},
		{"unknown ceiling fails safe to usage_only", string(GranularityFull), Granularity("bogus"), false},
		{"client usage_only stays usage_only under full ceiling", string(GranularityUsageOnly), GranularityFull, false},
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
			if len(toolEvents) != 1 {
				t.Fatalf("toolEvents = %d, want 1", len(toolEvents))
			}
			hasContent := toolEvents[0].RawToolInput != "" || toolEvents[0].ToolOutput != ""
			if hasContent != tc.wantContent {
				t.Errorf("hasContent = %v, want %v (input=%q output=%q)",
					hasContent, tc.wantContent, toolEvents[0].RawToolInput, toolEvents[0].ToolOutput)
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
