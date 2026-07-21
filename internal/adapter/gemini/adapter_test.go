package gemini

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestClassifyPath_ShapeFilter pins the path-shape classifier
// independently of the v1.4.51 under-WatchPaths constraint. Foreign-
// OS separators are accepted so tests + WSL2 /mnt/c paths still
// match. The integrated public API is covered by TestIsSessionFile
// below.
func TestClassifyPath_ShapeFilter(t *testing.T) {
	cases := []struct {
		path string
		want bool
		desc string
	}{
		{"/home/u/.gemini/tmp/abc/chats/session-2026-04-01T10-00-id1.json", true, "legacy json"},
		{"/home/u/.gemini/tmp/abc/chats/session-2026-04-01T10-00-id1.jsonl", true, "jsonl"},
		{`C:\Users\u\.gemini\tmp\abc\chats\session-2026-04-01T10-00-id1.json`, true, "windows path"},
		{"/mnt/c/Users/u/.gemini/tmp/abc/chats/session-X.json", true, "wsl cross-mount"},
		{"/home/u/.gemini/tmp/abc/checkpoints/cp-1.json", false, "checkpoints rejected"},
		{"/home/u/.gemini/tmp/abc/logs.json", false, "logs.json rejected"},
		{"/home/u/.gemini/antigravity/conversations/x.pb", false, "antigravity dir rejected"},
		{"/home/u/.gemini/tmp/abc/chats/parent-id/session-X.json", true, "subagent depth: classifier accepts (Parse rejects via classifySubagent — see TestSubagentRejectedExplicitly)"},
		{"/home/u/.gemini/tmp/abc/chats/foo.txt", false, "non-session basename"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			got := classifyPath(c.path) != classifyOther
			if got != c.want {
				t.Fatalf("classifyPath(%q) != other = %v, want %v", c.path, got, c.want)
			}
		})
	}
}

// TestIsSessionFile pins the integrated public API: shape AND
// under-WatchPaths. Uses host-native paths so filepath.Abs behaves.
func TestIsSessionFile(t *testing.T) {
	root := t.TempDir()
	a := NewWithOptions(nil, root)
	good := filepath.Join(root, ".gemini", "tmp", "abc", "chats", "session-1.jsonl")
	if !a.IsSessionFile(good) {
		t.Errorf("matching path under watch root should match: %s", good)
	}
	// Shape match but outside watch root (v1.4.51 invariant).
	if a.IsSessionFile("/tmp/foreign/.gemini/tmp/abc/chats/session-1.jsonl") {
		t.Error("shape-match outside watch root must NOT match")
	}
}

func TestParseLegacySingleTurn(t *testing.T) {
	a := New()
	dst := writeFixture(t, "abc/chats/session-2026-04-01T10-00-id1.json", "../../../testdata/gemini/session-legacy-singleturn.json")
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// 3 events: user_prompt + a standalone gemini.reasoning row (the fixture
	// carries a `thought` part) + the list_files tool call.
	if got := len(res.ToolEvents); got != 3 {
		t.Fatalf("ToolEvents=%d, want 3 (1 user + 1 reasoning + 1 list_files), got %#v", got, summary(res.ToolEvents))
	}
	if res.ToolEvents[0].ActionType != models.ActionUserPrompt {
		t.Fatalf("first event = %s, want user_prompt", res.ToolEvents[0].ActionType)
	}
	if res.ToolEvents[1].RawToolName != "gemini.reasoning" {
		t.Fatalf("second event raw = %s, want gemini.reasoning", res.ToolEvents[1].RawToolName)
	}
	if res.ToolEvents[2].ActionType != models.ActionSearchFiles {
		t.Fatalf("third event = %s, want search_files (list_files mapping)", res.ToolEvents[2].ActionType)
	}
	if res.ToolEvents[2].ToolOutput == "" {
		t.Fatalf("third event ToolOutput empty — functionResponse join failed")
	}
	if !strings.Contains(res.ToolEvents[2].ToolOutput, "main.go") {
		t.Fatalf("ToolOutput missing expected content: %q", res.ToolEvents[2].ToolOutput)
	}
	if res.ToolEvents[1].PrecedingReasoning == "" {
		t.Fatalf("expected reasoning carried from `thought` part")
	}
	if got := len(res.TokenEvents); got != 1 {
		t.Fatalf("TokenEvents=%d, want 1", got)
	}
	tok := res.TokenEvents[0]
	if tok.InputTokens != 1234 || tok.OutputTokens != 89 || tok.ReasoningTokens != 12 {
		t.Fatalf("token counts mismatch: %+v", tok)
	}
	if tok.Model != "gemini-2.5-pro" {
		t.Fatalf("token model = %q, want gemini-2.5-pro", tok.Model)
	}
	if res.ToolEvents[0].SessionID != "00000000-1111-2222-3333-444444444444" {
		t.Fatalf("user event session id = %q, want from JSON sessionId", res.ToolEvents[0].SessionID)
	}
}

func TestParseLegacyIdempotent(t *testing.T) {
	a := New()
	dst := writeFixture(t, "abc/chats/session-Y.json", "../../../testdata/gemini/session-legacy-multiturn.json")
	res1, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("first parse: %v", err)
	}
	res2, err := a.ParseSessionFile(context.Background(), dst, res1.NewOffset)
	if err != nil {
		t.Fatalf("second parse: %v", err)
	}
	// Re-parse with cursor at file size = no events (file unchanged).
	if len(res2.ToolEvents) != 0 || len(res2.TokenEvents) != 0 {
		t.Fatalf("re-parse with current cursor produced events: %+v", res2)
	}
	// Re-parse from scratch to confirm SourceEventIDs are deterministic
	// (caller-side dedup will collapse them).
	res3, _ := a.ParseSessionFile(context.Background(), dst, 0)
	if len(res3.ToolEvents) != len(res1.ToolEvents) || len(res3.TokenEvents) != len(res1.TokenEvents) {
		t.Fatalf("non-deterministic event count: first=%d/%d third=%d/%d",
			len(res1.ToolEvents), len(res1.TokenEvents),
			len(res3.ToolEvents), len(res3.TokenEvents))
	}
	for i := range res1.ToolEvents {
		if res1.ToolEvents[i].SourceEventID != res3.ToolEvents[i].SourceEventID {
			t.Fatalf("event %d SourceEventID drift: %q != %q", i, res1.ToolEvents[i].SourceEventID, res3.ToolEvents[i].SourceEventID)
		}
	}
}

func TestParseJSONL(t *testing.T) {
	a := New()
	dst := writeFixture(t, "abc/chats/session-jsonl-1.jsonl", "../../../testdata/gemini/session-jsonl-singleturn.jsonl")
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	want := map[string]int{
		models.ActionUserPrompt: 1,
		models.ActionRunCommand: 1,
	}
	got := map[string]int{}
	for _, ev := range res.ToolEvents {
		got[ev.ActionType]++
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("action_type %s count = %d, want %d (full counts: %v)", k, got[k], v, got)
		}
	}
	// runShellCommand event should have its tool output joined.
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionRunCommand {
			if !strings.Contains(ev.ToolOutput, "hi") {
				t.Fatalf("run_command ToolOutput = %q, want it to contain 'hi'", ev.ToolOutput)
			}
		}
	}
	// Two TokenEvents: original gemini message + message_update.
	if len(res.TokenEvents) != 2 {
		t.Fatalf("TokenEvents = %d, want 2 (initial + update)", len(res.TokenEvents))
	}
	// Both should share the same MessageID; the update has refreshed counts.
	first, second := res.TokenEvents[0], res.TokenEvents[1]
	if first.MessageID != second.MessageID {
		t.Fatalf("token MessageIDs diverge: %q vs %q", first.MessageID, second.MessageID)
	}
	if second.OutputTokens != 11 || second.ReasoningTokens != 3 {
		t.Fatalf("update tokens not picked up: %+v", second)
	}
}

// TestParseJSONLStringAssistantContent is the regression guard for the
// live-capture bug (2026-06-27): the real Gemini CLI writes a `gemini`
// (assistant) message's `content` as a BARE STRING ("hi there"), while user
// messages use an ARRAY of parts. Before the contentParts unmarshaler the
// string shape failed to decode and the whole assistant line was dropped, so
// the dashboard showed only the user prompt. This pins that the assistant
// message + its token row are now captured. Mirrors the exact on-disk shape
// (incl. the `$set` mutation lines the CLI interleaves).
func TestParseJSONLStringAssistantContent(t *testing.T) {
	a := New()
	dir := t.TempDir()
	dst := filepath.Join(dir, ".gemini", "tmp", "abc", "chats", "session-live.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"sessionId":"live-1","projectHash":"abc","startTime":"2026-06-26T19:07:00.000Z","kind":"session"}
{"$set":{"lastUpdated":"2026-06-26T19:07:01.000Z"}}
{"id":"u1","timestamp":"2026-06-26T19:07:02.000Z","type":"user","content":[{"text":"respond with exactly: hi there"}]}
{"$set":{"lastUpdated":"2026-06-26T19:07:05.000Z"}}
{"id":"g1","timestamp":"2026-06-26T19:07:05.000Z","type":"gemini","content":"hi there","thoughts":[],"tokens":{"input":11092,"output":2,"cached":0,"thoughts":130,"tool":0,"total":11224},"model":"gemini-3.5-flash"}
`
	if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// The header line + the interleaved {"$set":…} mutation lines must NOT
	// produce warnings (they'd flood the watcher log otherwise).
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings (header/$set should be silent): %v", res.Warnings)
	}
	counts := map[string]int{}
	for _, ev := range res.ToolEvents {
		counts[ev.ActionType]++
	}
	if counts[models.ActionUserPrompt] != 1 {
		t.Errorf("user_prompt rows = %d, want 1", counts[models.ActionUserPrompt])
	}
	// The assistant message must now produce a visible row (was dropped before).
	var assistantRow bool
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionAssistantMessage && strings.Contains(ev.ToolOutput, "hi there") {
			assistantRow = true
		}
	}
	if !assistantRow {
		t.Errorf("assistant message row missing (the dropped-line bug); events: %+v", res.ToolEvents)
	}
	// And the token row must be captured (input netted, output, reasoning).
	if len(res.TokenEvents) != 1 {
		t.Fatalf("TokenEvents = %d, want 1", len(res.TokenEvents))
	}
	tok := res.TokenEvents[0]
	if tok.InputTokens != 11092 || tok.OutputTokens != 2 || tok.ReasoningTokens != 130 {
		t.Errorf("token counts = in %d/out %d/reason %d, want 11092/2/130", tok.InputTokens, tok.OutputTokens, tok.ReasoningTokens)
	}
	if tok.Model != "gemini-3.5-flash" {
		t.Errorf("token model = %q, want gemini-3.5-flash", tok.Model)
	}
	// All events MUST share the path-derived session id — the header's UUID
	// sessionId must NOT be adopted, or the session fragments into two rows
	// (user prompt under the path id, the rest under the UUID).
	wantSID := "session-live"
	for _, ev := range res.ToolEvents {
		if ev.SessionID != wantSID {
			t.Errorf("ToolEvent %s session=%q, want %q (header UUID must not override)", ev.ActionType, ev.SessionID, wantSID)
		}
	}
	if tok.SessionID != wantSID {
		t.Errorf("TokenEvent session=%q, want %q", tok.SessionID, wantSID)
	}
}

// TestParseJSONLImageOnlyUserTurn pins the multimodal image-marker:
// a user turn carrying only an `inlineData` image part (no text) is
// surfaced as a user_prompt marker row instead of being silently
// dropped. Observability-only — no image bytes are stored.
func TestParseJSONLImageOnlyUserTurn(t *testing.T) {
	a := New()
	dir := t.TempDir()
	dst := filepath.Join(dir, ".gemini", "tmp", "abc", "chats", "session-img.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := `{"type":"session_metadata","sessionId":"img-1","cwd":"/tmp/p"}
{"type":"user","id":"u1","timestamp":"2026-04-03T09:00:01.000Z","content":[{"type":"image","inlineData":{"mimeType":"image/png","data":"AAAA"}}]}
`
	if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var imgRows int
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionUserPrompt && strings.Contains(ev.Target, "image attachment") {
			imgRows++
		}
	}
	if imgRows != 1 {
		t.Fatalf("image-marker user_prompt rows = %d, want 1 (events: %+v)", imgRows, res.ToolEvents)
	}
}

func TestParseJSONLMalformedLine(t *testing.T) {
	a := New()
	dst := writeFixture(t, "abc/chats/session-malformed.jsonl", "../../../testdata/gemini/session-malformed.jsonl")
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("expected at least 1 warning for malformed line")
	}
	// Should still capture the post-malformed user line.
	var sawUser bool
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionUserPrompt {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatalf("malformed-line skip did not advance — user event after malformed missed")
	}
}

func TestParseTruncatedJSONReturnsRetry(t *testing.T) {
	a := New()
	full, err := os.ReadFile("../../../testdata/gemini/session-legacy-singleturn.json")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// Truncate mid-write.
	dir := t.TempDir()
	dst := filepath.Join(dir, "tmp", "abc", "chats", "session-X.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gemini-marker"), nil, 0o644); err != nil {
		t.Fatalf("marker: %v", err)
	}
	// Re-anchor under a synthetic .gemini/tmp tree.
	dst2 := filepath.Join(dir, ".gemini", "tmp", "abc", "chats", "session-X.json")
	if err := os.MkdirAll(filepath.Dir(dst2), 0o755); err != nil {
		t.Fatalf("mkdir2: %v", err)
	}
	if err := os.WriteFile(dst2, full[:len(full)/2], 0o644); err != nil {
		t.Fatalf("write truncated: %v", err)
	}
	res, err := a.ParseSessionFile(context.Background(), dst2, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) == 0 {
		t.Fatalf("expected mid-write warning, got none; events=%d tokens=%d", len(res.ToolEvents), len(res.TokenEvents))
	}
	if res.NewOffset != 0 {
		t.Fatalf("truncated parse advanced cursor to %d, expected 0 (retry)", res.NewOffset)
	}
}

func TestSubagentRejectedExplicitly(t *testing.T) {
	a := New()
	res, err := a.ParseSessionFile(context.Background(), "/home/u/.gemini/tmp/abc/chats/parent-id/session-X.json", 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "subagent") {
		t.Fatalf("expected subagent warning, got: %v", res.Warnings)
	}
	if len(res.ToolEvents) != 0 {
		t.Fatalf("expected zero events for rejected subagent, got %d", len(res.ToolEvents))
	}
}

func TestProjectHashFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/home/u/.gemini/tmp/abc123/chats/session-X.json", "abc123"},
		{`C:\Users\u\.gemini\tmp\HASHHASH\chats\session-Y.jsonl`, "HASHHASH"},
		{"/no/match/here", ""},
	}
	for _, c := range cases {
		got := projectHashFromPath(c.path)
		if got != c.want {
			t.Fatalf("projectHashFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestMapToolName(t *testing.T) {
	cases := map[string]string{
		"read_file":         models.ActionReadFile,
		"readFile":          models.ActionReadFile,
		"runShellCommand":   models.ActionRunCommand,
		"run_shell_command": models.ActionRunCommand,
		"google_web_search": models.ActionWebSearch,
		"googleWebSearch":   models.ActionWebSearch,
		"replace":           models.ActionEditFile,
		"glob":              models.ActionSearchFiles,
		"grep":              models.ActionSearchText,
		"web_fetch":         models.ActionWebFetch,
		"unknown_tool":      models.ActionUnknown,
		"mcp__server__do":   models.ActionMCPCall,
	}
	for in, want := range cases {
		if got := mapToolName(in); got != want {
			t.Fatalf("mapToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// writeFixture copies a fixture file under a synthetic .gemini/tmp/<rel>
// path inside t.TempDir() so cwd-less classifyPath / project_root
// resolution doesn't depend on a real ~/.gemini/ install.
func writeFixture(t *testing.T, rel, fixture string) string {
	t.Helper()
	body, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("read fixture %s: %v", fixture, err)
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, ".gemini", "tmp", rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return dst
}

func summary(events []models.ToolEvent) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.ActionType + "/" + ev.RawToolName
	}
	return out
}

// TestAssistantTextEmission pins the new gemini.assistant_text emission:
// every text part on a role=gemini message produces an ActionTaskComplete
// row with RawToolName="gemini.assistant_text", the body in ToolOutput,
// MessageID linked to the assistant message id, and NO companion
// TokenEvent (token data flows through the existing tokens emitter).
// User-role text parts continue to emit user_prompt, NOT assistant_text.
func TestAssistantTextEmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".gemini", "tmp", "chats", "session-with-text.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{
  "sessionId": "asst-text-session",
  "projectHash": "0000",
  "startTime": "2026-05-12T10:00:00.000Z",
  "messages": [
    {"id":"u1","role":"user","timestamp":"2026-05-12T10:00:01.000Z","cwd":"/tmp/g","content":[{"type":"text","text":"ping"}]},
    {"id":"m1","role":"gemini","timestamp":"2026-05-12T10:00:02.000Z","model":"gemini-2.5-pro","content":[
      {"type":"text","text":"First gemini message."},
      {"type":"text","text":"Second gemini message."},
      {"type":"text","text":"   "},
      {"type":"functionCall","functionCall":{"id":"c1","name":"list_files","args":{"path":"."}}}
    ]}
  ]
}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// Filter to just assistant_text rows for ordering-independent checks.
	var asst []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.RawToolName == "gemini.assistant_text" {
			asst = append(asst, ev)
		}
	}
	if len(asst) != 2 {
		t.Fatalf("gemini.assistant_text rows: got %d want 2 (whitespace-only suppressed); all: %v", len(asst), summary(res.ToolEvents))
	}

	for i, want := range []string{"First gemini message.", "Second gemini message."} {
		ev := asst[i]
		if ev.ActionType != models.ActionAssistantMessage {
			t.Errorf("asst[%d] action = %q, want assistant_message", i, ev.ActionType)
		}
		if ev.ToolOutput != want {
			t.Errorf("asst[%d] tool_output = %q, want %q", i, ev.ToolOutput, want)
		}
		if ev.Target != want {
			t.Errorf("asst[%d] target = %q, want %q", i, ev.Target, want)
		}
		if ev.MessageID != "m1" {
			t.Errorf("asst[%d] message_id = %q, want m1", i, ev.MessageID)
		}
		if ev.Tool != models.ToolGeminiCLI {
			t.Errorf("asst[%d] tool = %q", i, ev.Tool)
		}
	}
	if asst[0].SourceEventID == asst[1].SourceEventID {
		t.Errorf("SourceEventIDs must differ across distinct parts: %q vs %q",
			asst[0].SourceEventID, asst[1].SourceEventID)
	}

	// User-role text part still emits user_prompt, not assistant_text.
	var userPrompts int
	for _, ev := range res.ToolEvents {
		if ev.ActionType == models.ActionUserPrompt {
			userPrompts++
		}
	}
	if userPrompts != 1 {
		t.Errorf("user_prompt count: got %d want 1", userPrompts)
	}
}

// TestTokenEventForNetsCachedInput pins the OpenAI-gross fix: Gemini's
// `input` is the gross prompt count including the cached portion, so
// tokenEventFor must emit NET input (gross − cacheRead) to match the cost
// engine's TokenBundle.Input contract and avoid double-billing the cached
// tokens (feedback_openai_input_is_gross).
func TestTokenEventForNetsCachedInput(t *testing.T) {
	st := &sessionState{SessionID: "s1", Model: "gemini-2.5-pro"}
	tests := []struct {
		name          string
		in            legacyTokens
		wantInput     int64
		wantCacheRead int64
	}{
		{"cacheRead field", legacyTokens{Input: 12000, Output: 50, CacheRead: 11000}, 1000, 11000},
		{"cached field", legacyTokens{Input: 12000, Output: 50, Cached: 11000}, 1000, 11000},
		{"max of both", legacyTokens{Input: 12000, Output: 50, CacheRead: 9000, Cached: 11000}, 1000, 11000},
		{"no cache", legacyTokens{Input: 1234, Output: 89, ThoughtsTokens: 12}, 1234, 0},
		{"clamp negative", legacyTokens{Input: 500, Cached: 900}, 0, 900},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := tokenEventFor("p", "m", time.Time{}, "", st, tc.in)
			if ev.InputTokens != tc.wantInput {
				t.Errorf("InputTokens = %d, want %d (net non-cached)", ev.InputTokens, tc.wantInput)
			}
			if ev.CacheReadTokens != tc.wantCacheRead {
				t.Errorf("CacheReadTokens = %d, want %d", ev.CacheReadTokens, tc.wantCacheRead)
			}
		})
	}
}

// TestTokenEventForReasoningKeys pins that reasoning tokens are captured
// from BOTH the live "thoughts" key and the fixture/proposed
// "thoughtsTokenCount" key (max of the two). Regression guard for the
// silent-zero bug where only thoughtsTokenCount was tagged.
func TestTokenEventForReasoningKeys(t *testing.T) {
	st := &sessionState{SessionID: "s1", Model: "gemini-3.5-flash"}
	tests := []struct {
		name string
		in   legacyTokens
		want int64
	}{
		{"live thoughts key", legacyTokens{Input: 100, Thoughts: 442}, 442},
		{"legacy thoughtsTokenCount key", legacyTokens{Input: 100, ThoughtsTokens: 12}, 12},
		{"both present takes max", legacyTokens{Input: 100, Thoughts: 442, ThoughtsTokens: 9}, 442},
		{"neither", legacyTokens{Input: 100}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := tokenEventFor("p", "m", time.Time{}, "", st, tc.in)
			if ev.ReasoningTokens != tc.want {
				t.Errorf("ReasoningTokens = %d, want %d", ev.ReasoningTokens, tc.want)
			}
		})
	}
}
