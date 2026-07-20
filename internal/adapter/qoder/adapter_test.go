package qoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// newTestAdapter builds an adapter whose watch roots are the two dirs so
// synthetic/fixture paths under them pass UnderAnyWatchRoot.
func newTestAdapter(roots ...string) *Adapter {
	return NewWithOptions(nil, roots...)
}

func TestName(t *testing.T) {
	if got := New().Name(); got != models.ToolQoder {
		t.Fatalf("Name() = %q, want %q", got, models.ToolQoder)
	}
	if got := New().Name(); got != "qoder" {
		t.Fatalf("Name() = %q, want qoder", got)
	}
}

func TestIsSessionFile(t *testing.T) {
	projRoot := "/home/u/.qoder/projects"
	segRoot := "/home/u/.qoder/logs/sessions"
	a := newTestAdapter(projRoot, segRoot)
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"canonical transcript", "/home/u/.qoder/projects/-home-dev-proj/1a2b.jsonl", true},
		{"windows-style separators", `/home/u/.qoder/projects/c--proj/deadbeef.jsonl`, true},
		{"segment run-log", "/home/u/.qoder/logs/sessions/-home-dev-proj/sid/segments/2026-07-09T10-24-p1.jsonl", true},
		{"encrypted state.json rejected", "/home/u/.qoder/projects/-home-dev-proj/1a2b/state.json", false},
		{"compression state.json rejected", "/home/u/.qoder/projects/-home-dev-proj/1a2b/compression-v2/state.json", false},
		{"transcript outside watch root", "/other/place/.qoder/projects/x/y.jsonl", false},
		{"segment outside watch root", "/other/.qoder/logs/sessions/x/s/segments/z.jsonl", false},
		{"non-jsonl", "/home/u/.qoder/projects/x/y.json", false},
		{"segments dir but not under sessions", "/home/u/.qoder/other/segments/z.jsonl", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.IsSessionFile(tc.path); got != tc.want {
				t.Fatalf("IsSessionFile(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// parseTranscript copies a testdata transcript fixture into a watch-rooted
// projects/<slug>/ dir, then parses it fully.
func parseTranscript(t *testing.T, fixture string) ([]models.ToolEvent, []models.TokenEvent, int64) {
	t.Helper()
	src := filepath.Join("..", "..", "..", "testdata", "qoder", fixture)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	root := t.TempDir()
	projects := filepath.Join(root, ".qoder", "projects")
	dst := filepath.Join(projects, "-home-dev-proj", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestAdapter(projects, filepath.Join(root, ".qoder", "logs", "sessions"))
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	return res.ToolEvents, res.TokenEvents, res.NewOffset
}

// parseSegment copies a testdata segment fixture into a watch-rooted
// logs/sessions/<slug>/<sid>/segments/ dir, then parses it fully.
func parseSegment(t *testing.T, fixture, sid string) ([]models.TokenEvent, int64) {
	t.Helper()
	src := filepath.Join("..", "..", "..", "testdata", "qoder", fixture)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	root := t.TempDir()
	sessions := filepath.Join(root, ".qoder", "logs", "sessions")
	dst := filepath.Join(sessions, "-home-dev-proj", sid, "segments", "seg.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestAdapter(filepath.Join(root, ".qoder", "projects"), sessions)
	if !a.IsSessionFile(dst) {
		t.Fatalf("segment fixture not recognised as session file: %s", dst)
	}
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	return res.TokenEvents, res.NewOffset
}

func TestParseToolCallSession(t *testing.T) {
	tools, tokens, off := parseTranscript(t, "tool-call-session.jsonl")
	if off <= 0 {
		t.Fatalf("offset did not advance: %d", off)
	}
	// The transcript carries NO tokens (usage is server-side only).
	if len(tokens) != 0 {
		t.Fatalf("token events = %d, want 0 (no local tokens in transcript)", len(tokens))
	}

	byAction := map[string]models.ToolEvent{}
	byRaw := map[string]models.ToolEvent{}
	for _, e := range tools {
		byAction[e.ActionType] = e
		byRaw[e.RawToolName] = e
	}
	for _, want := range []string{
		models.ActionSessionStart, models.ActionUserPrompt,
		models.ActionWriteFile, models.ActionRunCommand, models.ActionAssistantMessage,
	} {
		if _, ok := byAction[want]; !ok {
			t.Errorf("missing action type %q; got %v", want, actionTypes(tools))
		}
	}

	wf := byRaw["Write"]
	if wf.ActionType != models.ActionWriteFile {
		t.Errorf("Write mapped to %q", wf.ActionType)
	}
	if !strings.HasSuffix(wf.Target, "hello.txt") {
		t.Errorf("Write target = %q, want *hello.txt", wf.Target)
	}
	if !wf.Success {
		t.Errorf("Write should be success (from tool_result)")
	}
	if wf.ToolOutput == "" {
		t.Errorf("Write output not stamped from tool_result")
	}
	if wf.MessageID != "chatcmpl-AAA" {
		t.Errorf("Write MessageID = %q, want chatcmpl-AAA", wf.MessageID)
	}
	if wf.ContentBytes != int64(len("hello from qoder")) {
		t.Errorf("Write ContentBytes = %d, want %d", wf.ContentBytes, len("hello from qoder"))
	}
	if wf.Model != "" {
		t.Errorf("Model must stay empty (server-side only), got %q", wf.Model)
	}

	bash := byRaw["Bash"]
	if bash.ActionType != models.ActionRunCommand {
		t.Errorf("Bash action = %q", bash.ActionType)
	}
	if bash.Target != "ls" {
		t.Errorf("Bash target = %q, want ls", bash.Target)
	}

	// Project root resolves from the record cwd (a non-git dir → itself).
	if wf.ProjectRoot != "/home/dev/proj" {
		t.Errorf("project root = %q, want /home/dev/proj", wf.ProjectRoot)
	}
	// GitBranch carried from the envelope.
	if wf.GitBranch != "master" {
		t.Errorf("git branch = %q, want master", wf.GitBranch)
	}
	// exactly one session_start.
	var starts int
	for _, e := range tools {
		if e.ActionType == models.ActionSessionStart {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("session_start count = %d, want 1", starts)
	}
}

func TestSegmentTokensZeroGuarded(t *testing.T) {
	tokens, off := parseSegment(t, "segments-zero.jsonl", "5d51bc6a-0000-0000-0000-000000000001")
	if off <= 0 {
		t.Fatalf("offset did not advance: %d", off)
	}
	if len(tokens) != 0 {
		t.Fatalf("zero-token segments produced %d token events, want 0 (zero-usage guard)", len(tokens))
	}
}

func TestSegmentTokensNonZeroFlow(t *testing.T) {
	sid := "5d51bc6a-0000-0000-0000-000000000001"
	tokens, _ := parseSegment(t, "segments-tokens.jsonl", sid)
	// Two model.response.completed with non-zero usage; turn.finished is
	// NOT emitted (would double-count the aggregate).
	if len(tokens) != 2 {
		t.Fatalf("token events = %d, want 2 (per model.response.completed)", len(tokens))
	}
	first := tokens[0]
	if first.InputTokens != 1200 || first.OutputTokens != 84 || first.CacheReadTokens != 900 {
		t.Errorf("turn1 = in%d out%d cache%d, want in1200 out84 cache900",
			first.InputTokens, first.OutputTokens, first.CacheReadTokens)
	}
	if first.SessionID != sid {
		t.Errorf("session id = %q, want %q (recovered from path)", first.SessionID, sid)
	}
	if first.ProjectRoot != "/home/dev/proj" {
		t.Errorf("project root = %q, want /home/dev/proj (from config.loaded)", first.ProjectRoot)
	}
	if first.MessageID != "req-0000-0001" {
		t.Errorf("MessageID = %q, want req-0000-0001", first.MessageID)
	}
	if first.Source != models.TokenSourceJSONL || first.Reliability != models.ReliabilityApproximate {
		t.Errorf("source/reliability = %q/%q", first.Source, first.Reliability)
	}
	if first.Model != "" {
		t.Errorf("model must stay empty, got %q", first.Model)
	}
}

func TestMalformedToleranceAndOffset(t *testing.T) {
	tools, _, off := parseTranscript(t, "malformed-session.jsonl")
	src := filepath.Join("..", "..", "..", "testdata", "qoder", "malformed-session.jsonl")
	fi, _ := os.Stat(src)
	if off != fi.Size() {
		t.Errorf("offset = %d, want EOF %d", off, fi.Size())
	}
	var prompt, assistant bool
	for _, e := range tools {
		if e.ActionType == models.ActionUserPrompt && e.Target == "first prompt" {
			prompt = true
		}
		if e.ActionType == models.ActionAssistantMessage {
			assistant = true
		}
	}
	if !prompt || !assistant {
		t.Errorf("recovery failed: prompt=%v assistant=%v", prompt, assistant)
	}
}

func TestCursorResumption(t *testing.T) {
	src := filepath.Join("..", "..", "..", "testdata", "qoder", "tool-call-session.jsonl")
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	projects := filepath.Join(root, ".qoder", "projects")
	dst := filepath.Join(projects, "-home-dev-proj", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, body, 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestAdapter(projects, filepath.Join(root, ".qoder", "logs", "sessions"))

	res1, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatal(err)
	}
	full := len(res1.ToolEvents)

	lines := strings.SplitAfter(string(body), "\n")
	var midOffset int64
	for i := 0; i < 5; i++ { // through the first tool_use record
		midOffset += int64(len(lines[i]))
	}
	res2, err := a.ParseSessionFile(context.Background(), dst, midOffset)
	if err != nil {
		t.Fatal(err)
	}
	if res2.NewOffset != int64(len(body)) {
		t.Errorf("resume offset = %d, want %d", res2.NewOffset, len(body))
	}
	if len(res2.ToolEvents) >= full {
		t.Errorf("resume produced %d events, full produced %d — no resumption", len(res2.ToolEvents), full)
	}
	// A mid-file resume must NOT re-emit the session-start marker.
	for _, e := range res2.ToolEvents {
		if e.ActionType == models.ActionSessionStart {
			t.Errorf("session_start re-emitted on resume")
		}
	}
}

// TestScrubsSecrets builds a transcript with real secret-shaped strings
// (assembled from parts so the on-disk write filter doesn't redact the
// fixture) and asserts the adapter's scrubber removes them.
func TestScrubsSecrets(t *testing.T) {
	sk := "sk-" + "live" + strings.Repeat("A", 32)
	aws := "AKIA" + "IOSFODNN7EXAMPLE"
	line1 := `{"type":"user","uuid":"u-1","timestamp":"2026-07-09T05:00:00Z","message":{"role":"user","content":"my key is ` + sk + ` ok"},"parentUuid":null,"isSidechain":false,"cwd":"/home/dev/proj","sessionId":"sess-scrub","gitBranch":"main"}`
	line2 := `{"type":"assistant","uuid":"u-2","timestamp":"2026-07-09T05:00:02Z","message":{"id":"chatcmpl-S","role":"assistant","model":"","content":[{"type":"tool_use","id":"call_1","name":"Bash","input":{"command":"AWS_ACCESS_KEY_ID=` + aws + ` aws s3 ls"}}]},"parentUuid":"u-1","isSidechain":false,"cwd":"/home/dev/proj","sessionId":"sess-scrub","gitBranch":"main"}`
	body := line1 + "\n" + line2 + "\n"

	root := t.TempDir()
	projects := filepath.Join(root, ".qoder", "projects")
	dst := filepath.Join(projects, "-home-dev-proj", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestAdapter(projects, filepath.Join(root, ".qoder", "logs", "sessions"))
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.ToolEvents {
		blob := e.Target + "\x00" + e.RawToolInput + "\x00" + e.ToolOutput
		if strings.Contains(blob, sk) {
			t.Errorf("sk- secret leaked in event %q: %q", e.ActionType, blob)
		}
		if strings.Contains(blob, aws) {
			t.Errorf("AKIA secret leaked in event %q: %q", e.ActionType, blob)
		}
	}
}

func TestIsSidechainCarried(t *testing.T) {
	body := `{"type":"assistant","uuid":"u-1","timestamp":"2026-07-09T05:00:00Z","message":{"id":"m1","role":"assistant","model":"","content":[{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"/home/dev/proj/x.go"}}]},"parentUuid":null,"isSidechain":true,"cwd":"/home/dev/proj","sessionId":"s-side","gitBranch":"main"}` + "\n"
	root := t.TempDir()
	projects := filepath.Join(root, ".qoder", "projects")
	dst := filepath.Join(projects, "-home-dev-proj", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestAdapter(projects, filepath.Join(root, ".qoder", "logs", "sessions"))
	res, err := a.ParseSessionFile(context.Background(), dst, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("events = %d, want 1", len(res.ToolEvents))
	}
	if !res.ToolEvents[0].IsSidechain {
		t.Errorf("isSidechain not carried onto the event")
	}
	if res.ToolEvents[0].ActionType != models.ActionReadFile {
		t.Errorf("Read mapped to %q", res.ToolEvents[0].ActionType)
	}
}

func TestMapToolName(t *testing.T) {
	cases := map[string]string{
		"Write":            models.ActionWriteFile,
		"Read":             models.ActionReadFile,
		"Edit":             models.ActionEditFile,
		"Bash":             models.ActionRunCommand,
		"Grep":             models.ActionSearchText,
		"Glob":             models.ActionSearchFiles,
		"WebSearch":        models.ActionWebSearch,
		"WebFetch":         models.ActionWebFetch,
		"Agent":            models.ActionSpawnSubagent,
		"TodoWrite":        models.ActionTodoUpdate,
		"mcp__server__do":  models.ActionMCPCall,
		"SomethingUnknown": models.ActionUnknown,
	}
	for name, want := range cases {
		if got := mapToolName(name); got != want {
			t.Errorf("mapToolName(%q) = %q, want %q", name, got, want)
		}
	}
}

func actionTypes(evs []models.ToolEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.ActionType)
	}
	return out
}
