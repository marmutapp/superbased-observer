package freebuff

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestOffLimitsFilesNeverDispatchedOrIngested is the security guard for the
// freebuff store's secret + PII siblings. Two properties are pinned:
//
//	(1) IsSessionFile rejects credentials.json, message-history.json, and
//	    log.jsonl even when they sit right next to a real chat-messages.json,
//	    so the watcher never dispatches them to the parser; and
//	(2) parsing a chat dir that ALSO contains a log.jsonl full of PII
//	    (hostname / userId / userEmail) never surfaces any of those values in
//	    the emitted rows — the only sibling this adapter opens is
//	    run-state.json.
//
// A future refactor that widened the file glob or slurped a whole chat dir
// would break this test rather than silently ingesting the developer's PII.
func TestOffLimitsFilesNeverDispatchedOrIngested(t *testing.T) {
	// Build an isolated store: copy the real chat fixture into a temp
	// projects/<slug>/chats/<ts>/ dir, then plant the off-limits siblings.
	root := filepath.Join(t.TempDir(), "manicode", "projects")
	chatDir := filepath.Join(root, "guardslug", "chats", "2026-08-11T07-07-38.552Z")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, name := range []string{"chat-messages.json", "run-state.json", "chat-meta.json"} {
		src := filepath.Join(fixtureRoot(t), "needlehaystack", "chats",
			"2026-08-11T07-07-38.552Z", name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(chatDir, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	// PII / secret sentinels that must never reach an emitted row. The fake
	// key is built at runtime so no secret-shaped literal exists in source.
	piiHost := "SECRET-HOSTNAME-a1b2c3"
	piiUser := "SECRET-USERID-d4e5f6"
	piiEmail := "leaked.dev@secret-example.invalid"
	fakeKey := "guard-fake-" + t.Name()
	offLimits := map[string]string{
		"log.jsonl":            `{"hostname":"` + piiHost + `","userId":"` + piiUser + `","userEmail":"` + piiEmail + `"}` + "\n",
		"credentials.json":     `{"apiKey":"` + fakeKey + `"}`,
		"message-history.json": `["` + piiEmail + `"]`,
	}
	for name, body := range offLimits {
		if err := os.WriteFile(filepath.Join(chatDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write off-limits %s: %v", name, err)
		}
	}

	a := NewWithOptions(nil, root)

	// (1) None of the off-limits files may be dispatched as a session file.
	for name := range offLimits {
		if a.IsSessionFile(filepath.Join(chatDir, name)) {
			t.Errorf("IsSessionFile(%s) = true; off-limits secret/PII file must never be dispatched", name)
		}
	}

	// (2) Parsing the real chat-messages.json must not surface any sentinel.
	res, err := a.ParseSessionFile(context.Background(),
		filepath.Join(chatDir, "chat-messages.json"), 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, sentinel := range []string{piiHost, piiUser, piiEmail, fakeKey} {
		if strings.Contains(string(blob), sentinel) {
			t.Errorf("emitted rows contain off-limits sentinel %q — PII/secret leaked from a sibling file", sentinel)
		}
	}
}

// fixtureRoot is the manicode/projects watch root inside the top-level
// testdata/freebuff fixture tree (repo-wide convention: every other adapter
// keeps its fixtures at testdata/<adapter>/, not in-package).
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", "testdata", "freebuff", "manicode", "projects"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return root
}

func fixtureMessagesPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(fixtureRoot(t), "needlehaystack", "chats",
		"2026-08-11T07-07-38.552Z", "chat-messages.json")
}

func TestIsSessionFile(t *testing.T) {
	a := NewWithOptions(nil, fixtureRoot(t))
	msgs := fixtureMessagesPath(t)
	if !a.IsSessionFile(msgs) {
		t.Errorf("chat-messages.json under a watch root must be a session file: %s", msgs)
	}
	// run-state.json is a sibling, not a tracked session file.
	runState := filepath.Join(filepath.Dir(msgs), "run-state.json")
	if a.IsSessionFile(runState) {
		t.Errorf("run-state.json must NOT be a session file")
	}
	// A chat-messages.json outside the manicode/projects/.../chats shape.
	if a.IsSessionFile("/tmp/other/chat-messages.json") {
		t.Errorf("a path outside the manicode/projects/chats shape must not match")
	}
}

func TestParseSessionFile(t *testing.T) {
	a := NewWithOptions(nil, fixtureRoot(t))
	msgs := fixtureMessagesPath(t)

	res, err := a.ParseSessionFile(context.Background(), msgs, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// Cursor is a MESSAGE COUNT: 6 messages in the fixture.
	if res.NewOffset != 6 {
		t.Errorf("NewOffset = %d, want 6 (message count)", res.NewOffset)
	}
	// Freebuff records no billable usage.
	if len(res.TokenEvents) != 0 {
		t.Errorf("TokenEvents = %d, want 0 (freebuff has no per-turn usage)", len(res.TokenEvents))
	}

	// 14 tool events: 2 user prompts + 2 assistant messages + read_files +
	// write_file + 2 run_terminal_command (one top-level, one inside the
	// nested agent transcript) + 1 agent (subagent) spawn + 1 nested
	// set_output (task_complete) + glob (search_files) + write_todos +
	// ask_user + 1 genuinely-unmapped tool (suggest_followups). Mode-divider
	// blocks emit nothing.
	if len(res.ToolEvents) != 14 {
		t.Fatalf("ToolEvents = %d, want 14\n%+v", len(res.ToolEvents), res.ToolEvents)
	}

	// Every event carries the resolved project root (run-state projectRoot;
	// git.Resolve fails on the non-repo path so cwd passes through) + session.
	const wantRoot = "/home/dev/needlehaystack"
	seenIDs := map[string]bool{}
	for i, e := range res.ToolEvents {
		if e.Tool != models.ToolFreebuff {
			t.Errorf("event %d Tool = %q, want %q", i, e.Tool, models.ToolFreebuff)
		}
		if e.SessionID != "2026-08-11T07-07-38.552Z" {
			t.Errorf("event %d SessionID = %q", i, e.SessionID)
		}
		if e.ProjectRoot != wantRoot {
			t.Errorf("event %d ProjectRoot = %q, want %q", i, e.ProjectRoot, wantRoot)
		}
		if e.SourceEventID == "" {
			t.Errorf("event %d has empty SourceEventID (idempotency key)", i)
		}
		if seenIDs[e.SourceEventID] {
			t.Errorf("event %d has duplicate SourceEventID %q (nested-block id collision?)", i, e.SourceEventID)
		}
		seenIDs[e.SourceEventID] = true
	}

	// Assert the normalized action sequence.
	byAction := map[string]int{}
	for _, e := range res.ToolEvents {
		byAction[e.ActionType]++
	}
	wants := map[string]int{
		models.ActionUserPrompt:       2,
		models.ActionAssistantMessage: 2,
		models.ActionReadFile:         1,
		models.ActionWriteFile:        1,
		models.ActionRunCommand:       2,
		models.ActionSpawnSubagent:    1,
		models.ActionTaskComplete:     1,
		models.ActionSearchFiles:      1,
		models.ActionTodoUpdate:       1,
		models.ActionAskUser:          1,
		models.ActionUnknown:          1,
	}
	wantTotal := 0
	for at, n := range wants {
		wantTotal += n
		if byAction[at] != n {
			t.Errorf("action %q count = %d, want %d", at, byAction[at], n)
		}
	}
	if wantTotal != 14 {
		t.Fatalf("test bug: wants sums to %d, not 14", wantTotal)
	}

	// The reasoning run threads onto the following assistant message.
	var firstAssistant *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionAssistantMessage {
			firstAssistant = &res.ToolEvents[i]
			break
		}
	}
	if firstAssistant == nil || firstAssistant.PrecedingReasoning == "" {
		t.Errorf("first assistant message should carry PrecedingReasoning, got %+v", firstAssistant)
	}

	// The read_files tool target comes from input.paths[0].
	var read *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].RawToolName == "read_files" {
			read = &res.ToolEvents[i]
			break
		}
	}
	if read == nil || read.Target != "haystack/finder.go" {
		t.Errorf("read_files target = %v, want haystack/finder.go", read)
	}

	// The agent (subagent) block's RawToolName is the real agentType, and its
	// Target comes from params (initialPrompt is empty in real captures).
	var agentEvt *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionSpawnSubagent {
			agentEvt = &res.ToolEvents[i]
			break
		}
	}
	if agentEvt == nil {
		t.Fatalf("expected a spawn_subagent event")
	}
	if agentEvt.RawToolName != "basher" {
		t.Errorf("agent RawToolName = %q, want %q (agentType, not the literal \"agent\")", agentEvt.RawToolName, "basher")
	}
	if agentEvt.Target != "go test ./haystack/" {
		t.Errorf("agent Target = %q, want the params.command value", agentEvt.Target)
	}

	// The nested agent transcript's own tool call (run_terminal_command
	// inside the "basher" agent block) is captured with a distinct dotted
	// SourceEventID, not lost or collided with the top-level one.
	runCount := 0
	for _, e := range res.ToolEvents {
		if e.ActionType == models.ActionRunCommand {
			runCount++
		}
	}
	if runCount != 2 {
		t.Errorf("run_command events = %d, want 2 (one top-level, one nested in the agent block)", runCount)
	}

	// The genuinely-unmapped real tool name lands honestly on ActionUnknown.
	var unknown *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionUnknown {
			unknown = &res.ToolEvents[i]
			break
		}
	}
	if unknown == nil || unknown.RawToolName != "suggest_followups" {
		t.Errorf("unknown event = %+v, want RawToolName suggest_followups", unknown)
	}
}

// TestParseSessionFileIncremental pins the message-count cursor: a re-parse
// from the last-seen offset re-covers only the trailing message (stable
// SourceEventIDs let the store dedupe), never the whole transcript.
func TestParseSessionFileIncremental(t *testing.T) {
	a := NewWithOptions(nil, fixtureRoot(t))
	msgs := fixtureMessagesPath(t)

	// Resume from offset 6 (all seen): re-covers message index 5 only.
	res, err := a.ParseSessionFile(context.Background(), msgs, 6)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if res.NewOffset != 6 {
		t.Errorf("NewOffset = %d, want 6", res.NewOffset)
	}
	// Message 5 = 1 assistant message + 1 unknown tool = 2 events.
	if len(res.ToolEvents) != 2 {
		t.Errorf("re-cover of the last message = %d events, want 2", len(res.ToolEvents))
	}
}

// TestParseSessionFileCursorShrink pins the (previously unverified) shrink
// case: chat-messages.json is a whole-file rewrite, so nothing prevents a
// future poll from observing FEWER messages than a stale cursor recorded
// (e.g. the app rewrote the array with a trimmed/reset history). The offset
// arithmetic must degrade gracefully — no crash, no panic, no negative-index
// slice — and NewOffset must reflect the new, shorter reality.
func TestParseSessionFileCursorShrink(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "shrinkproj", "chats", "2026-07-01T00-00-00.000Z")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	msgs := []map[string]any{
		{"id": "u1", "variant": "user", "content": "hello"},
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(chatDir, "chat-messages.json")
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write: %v", err)
	}

	a := NewWithOptions(nil, dir)
	// Simulate a stale cursor recorded when the file had 5 messages; the
	// file now (after an app-side rewrite) has only 1.
	res, err := a.ParseSessionFile(context.Background(), path, 5)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if res.NewOffset != 1 {
		t.Errorf("NewOffset = %d, want 1 (the new, shorter message count)", res.NewOffset)
	}
	if len(res.ToolEvents) != 0 {
		t.Errorf("ToolEvents = %d, want 0 (start index clamped past the shrunk array, nothing re-covered)", len(res.ToolEvents))
	}
}

// TestResolveProjectRootMissingRunState pins the fallback when a chat dir has
// no sibling run-state.json (e.g. a chat directory created but no message
// ever sent, matching a real observed log.jsonl-only dir).
func TestResolveProjectRootMissingRunState(t *testing.T) {
	a := NewWithOptions(nil, fixtureRoot(t))
	msgs := filepath.Join(fixtureRoot(t), "otherslug", "chats",
		"2026-07-09T00-12-09.857Z", "chat-messages.json")

	res, err := a.ParseSessionFile(context.Background(), msgs, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("ToolEvents = %d, want 1", len(res.ToolEvents))
	}
	if got := res.ToolEvents[0].ProjectRoot; got != "[freebuff]" {
		t.Errorf("ProjectRoot = %q, want the no-run-state fallback %q", got, "[freebuff]")
	}
}

// TestResolveProjectRootIgnoresSlug pins that project-root resolution reads
// run-state.json's real cwd, never the manicode project-directory slug (the
// fixture's slug "mismatched-slug" deliberately does not match the cwd).
func TestResolveProjectRootIgnoresSlug(t *testing.T) {
	a := NewWithOptions(nil, fixtureRoot(t))
	msgs := filepath.Join(fixtureRoot(t), "mismatched-slug", "chats",
		"2026-07-10T00-00-00.000Z", "chat-messages.json")

	res, err := a.ParseSessionFile(context.Background(), msgs, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 1 {
		t.Fatalf("ToolEvents = %d, want 1", len(res.ToolEvents))
	}
	const want = "/home/dev/actual-project-name"
	if got := res.ToolEvents[0].ProjectRoot; got != want {
		t.Errorf("ProjectRoot = %q, want %q (from run-state.json, not the %q slug)", got, want, "mismatched-slug")
	}
}

func TestMapFreebuffTool(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"read_files", models.ActionReadFile},
		{"write_file", models.ActionWriteFile},
		{"str_replace", models.ActionEditFile},
		{"run_terminal_command", models.ActionRunCommand},
		{"code_search", models.ActionSearchText},
		{"find_files", models.ActionSearchFiles},
		{"glob", models.ActionSearchFiles},
		{"list_directory", models.ActionSearchFiles},
		{"web_search", models.ActionWebSearch},
		{"read_url", models.ActionWebFetch},
		{"spawn_agents", models.ActionSpawnSubagent},
		{"write_todos", models.ActionTodoUpdate},
		{"ask_user", models.ActionAskUser},
		{"skill", models.ActionSkillInvoke},
		{"browser_use", models.ActionBrowserAction},
		{"set_output", models.ActionTaskComplete},
		// Present in the app's own toolNames capability list, but never
		// observed as a real invocation: left honestly unmapped.
		{"tmux_cli", models.ActionUnknown},
		{"read_subtree", models.ActionUnknown},
		{"render_ui", models.ActionUnknown},
		{"gravity_index", models.ActionUnknown},
		{"file_picker", models.ActionUnknown},
		{"context_pruner", models.ActionUnknown},
		{"something_never_seen", models.ActionUnknown},
	}
	for _, tc := range cases {
		got, _ := mapFreebuffTool(tc.name, nil)
		if got != tc.want {
			t.Errorf("mapFreebuffTool(%q) action = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSessionIDFromPath(t *testing.T) {
	got := sessionIDFromPath("/x/manicode/projects/p/chats/2026-08-11T07-07-38.552Z/chat-messages.json")
	if got != "2026-08-11T07-07-38.552Z" {
		t.Errorf("sessionIDFromPath = %q", got)
	}
}

func TestParseChatDirTime(t *testing.T) {
	ts := parseChatDirTime("2026-08-11T07-07-38.552Z")
	if ts.IsZero() {
		t.Fatalf("parseChatDirTime returned zero for a valid dir name")
	}
	if got := ts.UTC().Format("2006-01-02T15:04:05"); got != "2026-08-11T07:07:38" {
		t.Errorf("parsed time = %s, want 2026-08-11T07:07:38", got)
	}
}

// TestAgentNestDepthCap pins the maxAgentNestDepth recursion boundary the
// 2026-08-25 adversarial review flagged as verified-safe-but-untested: a
// pathological chat whose agent blocks nest far deeper than the cap must
// parse without panic/hang and emit spawn events for exactly the capped
// depth (root level = depth 0, so maxAgentNestDepth+1 spawn events total),
// silently ignoring everything deeper.
func TestAgentNestDepthCap(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "deepproj", "chats", "2026-07-01T00-00-00.000Z")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Build 51 nested agent blocks, innermost first.
	block := map[string]any{"type": "agent", "agentType": "leaf", "params": map[string]any{}}
	for i := 0; i < 50; i++ {
		block = map[string]any{
			"type": "agent", "agentType": "nest", "params": map[string]any{},
			"blocks": []any{block},
		}
	}
	msgs := []map[string]any{
		{"id": "a1", "variant": "assistant", "blocks": []any{block}},
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(chatDir, "chat-messages.json")
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write: %v", err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	want := maxAgentNestDepth + 1
	if len(res.ToolEvents) != want {
		t.Errorf("ToolEvents = %d, want exactly %d (root + %d nested levels, deeper ignored)",
			len(res.ToolEvents), want, maxAgentNestDepth)
	}
	if res.NewOffset != 1 {
		t.Errorf("NewOffset = %d, want 1", res.NewOffset)
	}
}

// TestNestedAgentBlocksStampedSidechain pins the freebuff half of the
// WS-SUBAGENT/EFFORT work-stream (adapter-parity-audit-2026-08-25.md
// §2.4): the recursive nested-agent-blocks walk captured child-agent
// actions but never flagged them as sidechain. A top-level message
// carries: a top-level text block (not sidechain), a top-level "agent"
// block that SPAWNS a subagent (the spawn action itself is not a
// sidechain — it happens in the parent's own context), and that
// subagent's own nested transcript with a text block, a tool block, and
// a further nested "agent" block (all of which ARE sidechain, since
// they execute inside the subagent's context).
func TestNestedAgentBlocksStampedSidechain(t *testing.T) {
	dir := t.TempDir()
	chatDir := filepath.Join(dir, "sideproj", "chats", "2026-07-01T00-00-00.000Z")
	if err := os.MkdirAll(chatDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	msgs := []map[string]any{
		{
			"id": "a1", "variant": "assistant",
			"blocks": []any{
				map[string]any{"type": "text", "content": "top-level reply"},
				map[string]any{
					"type": "agent", "agentType": "researcher", "agentName": "researcher",
					"params": map[string]any{"command": "investigate"},
					"blocks": []any{
						map[string]any{"type": "text", "content": "subagent's own reply"},
						map[string]any{
							"type": "tool", "toolName": "read_file", "toolCallId": "tc1",
							"input": map[string]any{"path": "README.md"},
						},
						map[string]any{
							"type": "agent", "agentType": "nested-helper", "agentName": "nested-helper",
							"params": map[string]any{"command": "double-nested"},
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(chatDir, "chat-messages.json")
	if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write: %v", err)
	}

	a := NewWithOptions(nil, dir)
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// 5 rows: top text, spawn agent, nested text, nested tool, nested agent.
	if len(res.ToolEvents) != 5 {
		t.Fatalf("ToolEvents = %d, want 5", len(res.ToolEvents))
	}
	byRaw := map[string]models.ToolEvent{}
	for _, te := range res.ToolEvents {
		byRaw[te.SourceEventID] = te
	}
	sessID := sessionIDFromPath(path)

	topText, ok := byRaw[idFor(sessID, 0, "0", "text")]
	if !ok {
		t.Fatalf("top-level text row not found; got IDs %v", keysOf(byRaw))
	}
	if topText.IsSidechain {
		t.Errorf("top-level text IsSidechain = true, want false")
	}
	spawn, ok := byRaw[idFor(sessID, 0, "1", "agent")]
	if !ok {
		t.Fatalf("spawning agent row not found; got IDs %v", keysOf(byRaw))
	}
	if spawn.IsSidechain {
		t.Errorf("spawning agent block IsSidechain = true, want false (spawn itself is not a sidechain)")
	}
	if spawn.ActionType != models.ActionSpawnSubagent {
		t.Fatalf("spawn ActionType = %q, want %q", spawn.ActionType, models.ActionSpawnSubagent)
	}
	nestedText, ok := byRaw[idFor(sessID, 0, "1.0", "text")]
	if !ok {
		t.Fatalf("nested text row not found; got IDs %v", keysOf(byRaw))
	}
	if !nestedText.IsSidechain {
		t.Errorf("nested text IsSidechain = false, want true")
	}
	nestedTool, ok := byRaw[idFor(sessID, 0, "1.1", "tc1")]
	if !ok {
		t.Fatalf("nested tool row not found; got IDs %v", keysOf(byRaw))
	}
	if !nestedTool.IsSidechain {
		t.Errorf("nested tool IsSidechain = false, want true")
	}
	nestedAgent, ok := byRaw[idFor(sessID, 0, "1.2", "agent")]
	if !ok {
		t.Fatalf("nested (agent-in-agent) row not found; got IDs %v", keysOf(byRaw))
	}
	if !nestedAgent.IsSidechain {
		t.Errorf("nested (agent-in-agent) spawn IsSidechain = false, want true (already inside subagent context)")
	}
}

func keysOf(m map[string]models.ToolEvent) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
