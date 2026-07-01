package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/platform/crossmount"
)

func TestParseSessionFile_SQLiteCapturesRichActions(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 3 {
		t.Fatalf("expected 3 events, got %d", got)
	}

	if got := res.ToolEvents[0].ActionType; got != models.ActionUserPrompt {
		t.Fatalf("first event action_type = %q, want %q", got, models.ActionUserPrompt)
	}
	if got := res.ToolEvents[0].Target; got != "Build the app" {
		t.Fatalf("prompt target = %q", got)
	}

	if got := res.ToolEvents[1].ActionType; got != models.ActionRunCommand {
		t.Fatalf("second event action_type = %q, want %q", got, models.ActionRunCommand)
	}
	if res.ToolEvents[1].Success {
		t.Fatalf("expected bash event to be unsuccessful")
	}
	if got := res.ToolEvents[1].Target; got != "npm start" {
		t.Fatalf("command target = %q", got)
	}

	if got := res.ToolEvents[2].ActionType; got != models.ActionTaskComplete {
		t.Fatalf("third event action_type = %q, want %q", got, models.ActionTaskComplete)
	}
}

// TestParseSessionFile_SQLitePopulatesMessageIDAndToolOutputAndDuration
// pins the per-adapter parity with claudecode that landed in v1.4.19:
// every OpenCode event now carries a MessageID grouping (msg_xxx for
// assistant turns, "user:<id>" for prompts), tool events carry the
// scrubbed body of the tool result via ToolOutput, and DurationMs is
// derived from the part's own start/end timestamps. Pre-fix the audit
// flagged all three as silently zero — the source data was right there
// but the adapter discarded it.
func TestParseSessionFile_SQLitePopulatesMessageIDAndToolOutputAndDuration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := res.ToolEvents[0].MessageID; got != "user:msg_user" {
		t.Errorf("user prompt MessageID = %q, want user:msg_user", got)
	}
	tool := res.ToolEvents[1]
	if tool.MessageID != "msg_tool" {
		t.Errorf("tool MessageID = %q, want msg_tool (parent message id)", tool.MessageID)
	}
	if tool.ToolOutput != "boom" {
		t.Errorf("tool ToolOutput = %q, want %q (scrubbed State.Output)", tool.ToolOutput, "boom")
	}
	if tool.DurationMs != 300 {
		t.Errorf("tool DurationMs = %d, want 300 (end-start in fixture)", tool.DurationMs)
	}
	if got := res.ToolEvents[2].MessageID; got != "msg_done" {
		t.Errorf("completion MessageID = %q, want msg_done", got)
	}
}

// TestParseSessionFile_SQLiteEmitsTokenEventsForAssistantMessages
// pins OpenCode token-extraction behaviour. Confirmed against
// OpenCode's InfoData zod schema in
// packages/opencode/src/session/message.ts:
//
//	tokens: { input, output, reasoning, cache: { read, write } }
//	cost:   number (USD)
//
// Pre-fix the adapter only extracted role/model/time from the data
// blob; the token + cost fields were silently ignored, which is why
// OpenCode rows landed on the dashboard with Source="jsonl" but no
// numbers attached. This test seeds an assistant message with the
// full token bundle and asserts each field flows through to the
// emitted TokenEvent — Reliability=approximate (not unreliable like
// Claude Code's JSONL, since OpenCode persists the upstream usage
// envelope verbatim).
func TestParseSessionFile_SQLiteEmitsTokenEventsForAssistantMessages(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithTokens(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.TokenEvents); got != 1 {
		t.Fatalf("expected 1 token event, got %d", got)
	}
	te := res.TokenEvents[0]
	if te.Tool != models.ToolOpenCode {
		t.Errorf("Tool = %q, want %q", te.Tool, models.ToolOpenCode)
	}
	if te.Model != "big-pickle" {
		t.Errorf("Model = %q, want big-pickle", te.Model)
	}
	if te.InputTokens != 1234 {
		t.Errorf("InputTokens = %d, want 1234", te.InputTokens)
	}
	if te.OutputTokens != 567 {
		t.Errorf("OutputTokens = %d, want 567", te.OutputTokens)
	}
	if te.CacheReadTokens != 12345 {
		t.Errorf("CacheReadTokens = %d, want 12345", te.CacheReadTokens)
	}
	if te.CacheCreationTokens != 678 {
		t.Errorf("CacheCreationTokens = %d, want 678", te.CacheCreationTokens)
	}
	if te.ReasoningTokens != 89 {
		t.Errorf("ReasoningTokens = %d, want 89", te.ReasoningTokens)
	}
	if te.EstimatedCostUSD != 0.0532 {
		t.Errorf("EstimatedCostUSD = %v, want 0.0532", te.EstimatedCostUSD)
	}
	if te.Reliability != models.ReliabilityApproximate {
		t.Errorf("Reliability = %q, want %q", te.Reliability, models.ReliabilityApproximate)
	}
	if te.Source != models.TokenSourceJSONL {
		t.Errorf("Source = %q, want %q", te.Source, models.TokenSourceJSONL)
	}
	if te.MessageID != "msg_done" {
		t.Errorf("MessageID = %q, want msg_done (the assistant message id)", te.MessageID)
	}
}

// TestParseSessionFile_SQLiteSkipsZeroTokenAssistantRows pins the
// no-tokens guard — in-progress turns and assistant rows where the
// token bundle is empty across the board don't emit a TokenEvent
// (would otherwise pollute the cost engine with zero rows).
func TestParseSessionFile_SQLiteSkipsZeroTokenAssistantRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path) // seeds assistant rows WITHOUT tokens

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.TokenEvents); got != 0 {
		t.Errorf("expected 0 token events from token-less rows, got %d: %+v", got, res.TokenEvents)
	}
}

// TestParseSessionFile_SubtaskPartEmitsSpawnSubagent pins the
// subtask-part wiring added in v1.4.9. OpenCode's parent message
// emits a `subtask` part to invoke a subagent (Build/Plan/Explore/
// custom). We tag those as ActionSpawnSubagent with target=agent name
// + the subagent's model when set.
func TestParseSessionFile_SubtaskPartEmitsSpawnSubagent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithSubtask(t, path)
	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var spawn *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionSpawnSubagent {
			spawn = &res.ToolEvents[i]
			break
		}
	}
	if spawn == nil {
		t.Fatalf("expected a spawn_subagent event from subtask part, got %+v", res.ToolEvents)
	}
	if spawn.Target != "Explore" {
		t.Errorf("Target = %q, want Explore (subagent name)", spawn.Target)
	}
	if spawn.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5 (subagent's model)", spawn.Model)
	}
	if spawn.RawToolName != "subtask" {
		t.Errorf("RawToolName = %q, want subtask", spawn.RawToolName)
	}
	if spawn.MessageID != "msg_a" {
		t.Errorf("MessageID = %q, want msg_a (parent message id)", spawn.MessageID)
	}
}

// TestParseSessionFile_TodoTableEmitsTodoUpdate pins the todo-table
// wiring. Each row → one ActionTodoUpdate event with target=status.
func TestParseSessionFile_TodoTableEmitsTodoUpdate(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithTodos(t, path)
	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var todoCount int
	for _, e := range res.ToolEvents {
		if e.ActionType == models.ActionTodoUpdate {
			todoCount++
		}
	}
	if todoCount != 2 {
		t.Errorf("expected 2 todo events, got %d (events=%+v)", todoCount, res.ToolEvents)
	}
}

// TestParseSessionFile_NewToolNamesMappedCorrectly pins the
// mapTool() extension covering webfetch, websearch, task, todoread,
// todowrite, multiedit, and OpenCode's underscore variant `apply_patch`.
// Pre-fix these all fell through to mcp regex or stayed as ActionUnknown.
func TestParseSessionFile_NewToolNamesMappedCorrectly(t *testing.T) {
	cases := []struct {
		tool, want string
	}{
		{"webfetch", models.ActionWebFetch},
		{"websearch", models.ActionWebSearch},
		{"task", models.ActionSpawnSubagent},
		{"agent", models.ActionSpawnSubagent},
		{"todowrite", models.ActionTodoUpdate},
		{"todoread", models.ActionTodoUpdate},
		{"multiedit", models.ActionEditFile},
		{"apply_patch", models.ActionEditFile},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			part := toolPartData{Tool: tc.tool}
			at, _, _, _ := mapTool(part)
			if at != tc.want {
				t.Errorf("mapTool(%q): got %q, want %q", tc.tool, at, tc.want)
			}
		})
	}
}

func TestParseSessionFile_SQLiteWatermarkSkipsOldRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDB(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 3000)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 0 {
		t.Fatalf("expected no events, got %d", len(res.ToolEvents))
	}
	if res.NewOffset != 3000 {
		t.Fatalf("NewOffset = %d, want 3000", res.NewOffset)
	}
}

// TestParseSessionFile_AssistantTextEmission pins the new
// opencode.assistant_text emission: assistant-role text parts in the
// `part` table produce ActionTaskComplete rows with the body in
// ToolOutput, NO token/cost fields on the ToolEvent (token data flows
// through the separate TokenEvent path), and MessageID set to the
// parent message ID for cross-event linkage.
func TestParseSessionFile_AssistantTextEmission(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithAssistantText(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	// Filter the emitted events to just the assistant_text rows for
	// stable assertions independent of other emitters' ordering.
	var asst []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.RawToolName == "opencode.assistant_text" {
			asst = append(asst, ev)
		}
	}
	if len(asst) != 2 {
		t.Fatalf("opencode.assistant_text rows: got %d want 2 (full events: %+v)", len(asst), res.ToolEvents)
	}

	for i, want := range []string{"First reasoning chunk.", "Second reasoning chunk."} {
		ev := asst[i]
		if ev.ActionType != models.ActionAssistantMessage {
			t.Errorf("asst[%d] action_type = %q, want assistant_message", i, ev.ActionType)
		}
		if ev.Target != want {
			t.Errorf("asst[%d] target = %q, want %q", i, ev.Target, want)
		}
		if ev.ToolOutput != want {
			t.Errorf("asst[%d] tool_output = %q, want %q", i, ev.ToolOutput, want)
		}
		if ev.MessageID != "msg_asst" {
			t.Errorf("asst[%d] message_id = %q, want msg_asst", i, ev.MessageID)
		}
		if ev.Tool != models.ToolOpenCode {
			t.Errorf("asst[%d] tool = %q, want %s", i, ev.Tool, models.ToolOpenCode)
		}
		if !ev.Success {
			t.Errorf("asst[%d] should be success", i)
		}
	}
	if asst[0].SourceEventID == asst[1].SourceEventID {
		t.Errorf("SourceEventIDs must differ across distinct parts: %q vs %q",
			asst[0].SourceEventID, asst[1].SourceEventID)
	}
}

// TestParseSessionFile_VariantStampsEffortLevelOnAssistantRows pins
// F1 from the 2026-05-21 opencode audit. OpenCode CLI sets
// `message.data.variant` to the per-(provider,model) effort selection
// ("low"/"medium"/"high") from ~/.local/state/opencode/model.json on
// every assistant message — verified against a live WSL CLI session
// where the assistant message JSON carried `"variant":"high"`. Pre-fix
// the adapter's messageData struct had no Variant field, so the
// effort was silently dropped from every CLI-origin row. This test
// seeds an assistant message with variant=high and asserts the
// effort lands on Metadata.EffortLevel of every assistant-side
// ToolEvent (completion, tool, assistant_text, subtask, reasoning,
// step_finish) — and stays nil on the user_prompt row which has no
// variant in OpenCode's emit shape.
func TestParseSessionFile_VariantStampsEffortLevelOnAssistantRows(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithVariant(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	type want struct {
		raw    string
		effort string // "" means Metadata should be nil
		stop   string // expected Metadata.StopReason ("" = not asserted)
	}
	wantFor := map[string]want{
		// user prompts carry no variant in OpenCode's emit shape.
		"chat.message":            {raw: "chat.message", effort: ""},
		"opencode.assistant_text": {raw: "opencode.assistant_text", effort: "high"},
		// The terminus row stamps the source finish reason into the
		// canonical Metadata.StopReason column alongside the effort.
		"assistant.stop":       {raw: "assistant.stop", effort: "high", stop: "stop"},
		"bash":                 {raw: "bash", effort: "high"},
		"opencode.reasoning":   {raw: "opencode.reasoning", effort: "high"},
		"opencode.step_finish": {raw: "opencode.step_finish", effort: "high"},
	}
	seen := map[string]bool{}
	for _, ev := range res.ToolEvents {
		w, ok := wantFor[ev.RawToolName]
		if !ok {
			continue
		}
		seen[ev.RawToolName] = true
		if w.effort == "" {
			if ev.Metadata != nil {
				t.Errorf("%s row Metadata = %+v, want nil (no variant on user prompts)", ev.RawToolName, ev.Metadata)
			}
			continue
		}
		if ev.Metadata == nil {
			t.Errorf("%s row Metadata = nil, want EffortLevel=%q", ev.RawToolName, w.effort)
			continue
		}
		if ev.Metadata.EffortLevel != w.effort {
			t.Errorf("%s row EffortLevel = %q, want %q", ev.RawToolName, ev.Metadata.EffortLevel, w.effort)
		}
		if w.stop != "" && ev.Metadata.StopReason != w.stop {
			t.Errorf("%s row StopReason = %q, want %q", ev.RawToolName, ev.Metadata.StopReason, w.stop)
		}
	}
	for raw := range wantFor {
		if !seen[raw] {
			t.Errorf("expected an emitted row with RawToolName=%q; got events=%+v", raw, res.ToolEvents)
		}
	}
}

// TestEffortMetadata_TrimAndEmpty pins the helper's contract — only
// non-empty trimmed input produces a non-nil ActionMetadata. Whitespace-
// only variants (defensive — OpenCode shouldn't emit them) return nil
// so action rows don't get a "{}" metadata column for nothing.
func TestEffortMetadata_TrimAndEmpty(t *testing.T) {
	if effortMetadata("") != nil {
		t.Error("empty variant must return nil Metadata")
	}
	if effortMetadata("   ") != nil {
		t.Error("whitespace variant must return nil Metadata")
	}
	if m := effortMetadata("high"); m == nil || m.EffortLevel != "high" {
		t.Errorf("variant=high must return EffortLevel=high; got %+v", m)
	}
}

// TestParseSessionFile_ResolveProjectRootTranslatesForeignCwd pins
// F3 from the 2026-05-21 audit. OpenCode Desktop on Windows records
// path.cwd in Windows convention (e.g. "C:\programsx\..."); pre-fix
// resolveProjectRoot called git.Resolve directly on that string, and
// because filepath.Abs treats it as relative on Linux, the result
// CWD-prefixed onto observer's own tree (memory
// feedback_foreign_path_git_resolve). This test asserts the cwd is
// translated through crossmount.TranslateForeignPath before
// git.Resolve runs — so the projectRoot is "/mnt/c/programsx/..."
// (the WSL-mount equivalent) rather than observer's repo root.
func TestParseSessionFile_ResolveProjectRootTranslatesForeignCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("foreign-cwd translation to /mnt/c is a Linux/WSL-daemon-reading-Windows-mount behavior; on a Windows host a C:\\ path is native and correctly stays untranslated")
	}
	a := NewWithOptions(nil, []string{t.TempDir()})
	cache := map[string]string{}
	got := a.resolveProjectRoot(`C:\programsx\open-code-test`, cache)
	// git.Resolve will fail to find a .git dir under /mnt/c on this
	// host; the fallback path stores the (translated) cwd directly.
	if !strings.HasPrefix(got, "/mnt/") {
		t.Errorf("resolveProjectRoot did not translate foreign path; got %q (want prefix /mnt/)", got)
	}
}

// TestParseSessionFile_ReasoningPartEmitsRow pins F4 from the audit.
// OpenCode Desktop and the CLI both emit `reasoning`-typed parts
// carrying the model's chain-of-thought body (verified 2026-05-21 on a
// live Desktop session: 11 reasoning parts across 11 messages).
// Pre-fix the adapter dropped them silently. This test seeds one
// reasoning part and asserts a single ActionTaskComplete row lands
// with RawToolName=opencode.reasoning and the body in ToolOutput.
func TestParseSessionFile_ReasoningPartEmitsRow(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithReasoning(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}

	var reasoning []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.RawToolName == "opencode.reasoning" {
			reasoning = append(reasoning, ev)
		}
	}
	if len(reasoning) != 1 {
		t.Fatalf("opencode.reasoning rows: got %d want 1 (events=%+v)", len(reasoning), res.ToolEvents)
	}
	ev := reasoning[0]
	if ev.ActionType != models.ActionTaskComplete {
		t.Errorf("ActionType = %q, want task_complete", ev.ActionType)
	}
	if ev.ToolOutput != "Considering the next step..." {
		t.Errorf("ToolOutput = %q, want %q", ev.ToolOutput, "Considering the next step...")
	}
	if ev.DurationMs != 4500 {
		t.Errorf("DurationMs = %d, want 4500 (end-start)", ev.DurationMs)
	}
	if ev.MessageID != "msg_asst" {
		t.Errorf("MessageID = %q, want msg_asst", ev.MessageID)
	}
	if ev.SourceEventID != "reasoning:prt_reasoning" {
		t.Errorf("SourceEventID = %q, want reasoning:prt_reasoning", ev.SourceEventID)
	}
}

// TestParseSessionFile_StepFinishPartEmitsToolEventOnly pins F5 from
// the audit. OpenCode emits `step-finish` parts per-step within an
// assistant message carrying that step's token + cost slice; summed
// across all step-finishes within a message, the totals equal the
// message-level token bundle. Emitting TokenEvents from step-finish
// would double-count against loadTokenEvents — this test asserts a
// step-finish part produces a ToolEvent but ZERO additional
// TokenEvents (the message-level token bundle remains the single
// source of token truth).
func TestParseSessionFile_StepFinishPartEmitsToolEventOnly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "opencode.db")
	setupOpenCodeDBWithStepFinish(t, path)

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	var steps []models.ToolEvent
	for _, ev := range res.ToolEvents {
		if ev.RawToolName == "opencode.step_finish" {
			steps = append(steps, ev)
		}
	}
	if len(steps) != 1 {
		t.Fatalf("opencode.step_finish rows: got %d want 1 (events=%+v)", len(steps), res.ToolEvents)
	}
	ev := steps[0]
	if ev.Target != "tool-calls" {
		t.Errorf("Target = %q, want tool-calls (finish reason)", ev.Target)
	}
	if !strings.Contains(ev.RawToolInput, `"tokens"`) {
		t.Errorf("RawToolInput must contain the step-finish JSON tokens block; got %q", ev.RawToolInput)
	}
	// Crucial: the step-finish path MUST NOT emit any TokenEvent —
	// message-level loadTokenEvents owns token attribution.
	if len(res.TokenEvents) != 0 {
		t.Errorf("step-finish must not emit TokenEvents (would double-count); got %+v", res.TokenEvents)
	}
}

// TestIsForeignMountPath_OnlyForeignHomes pins the F2 helper's
// contract — only paths under cross-mount-detected non-native homes
// match. Tests inject a fake AllHomes returning one native + one
// foreign home so the assertion runs identically on any host.
func TestIsForeignMountPath_OnlyForeignHomes(t *testing.T) {
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: "/home/me", OS: crossmount.OSLinux, Origin: "native"},
			{Path: "/mnt/c/Users/auzy_", OS: crossmount.OSWindows, Origin: "wsl-mnt:auzy_"},
		}
	}
	cases := []struct {
		path string
		want bool
	}{
		{"/home/me/.local/share/opencode/opencode.db", false},
		{"/mnt/c/Users/auzy_/.local/share/opencode/opencode.db", true},
		{"/tmp/something", false},
		{"/mnt/c/Users/other/.local/share/opencode/opencode.db", false},
	}
	for _, tc := range cases {
		if got := isForeignMountPath(tc.path); got != tc.want {
			t.Errorf("isForeignMountPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestStageMirrorIfForeign_NativePassThrough pins that native paths
// hit the no-op fast path — no copy, no cache dir created. Critical
// because every parse call goes through this helper; a mistaken
// always-copy would burn 2.3+MB I/O on every native poll.
func TestStageMirrorIfForeign_NativePassThrough(t *testing.T) {
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: "/home/me", OS: crossmount.OSLinux, Origin: "native"},
		}
	}
	got, err := stageMirrorIfForeign("/home/me/.local/share/opencode/opencode.db")
	if err != nil {
		t.Fatalf("stageMirrorIfForeign: %v", err)
	}
	if got != "/home/me/.local/share/opencode/opencode.db" {
		t.Errorf("native path got remapped to %q (want passthrough)", got)
	}
}

// TestStageMirrorIfForeign_CopiesTrioAndReusesOnRepeat pins F2's
// happy path — a foreign-mount source triggers a trio copy to a per-
// source cache dir; a second call with no source mtime change skips
// the copy. Uses a fake AllHomes pointing at a tempdir so the test
// runs without depending on /mnt/c.
func TestStageMirrorIfForeign_CopiesTrioAndReusesOnRepeat(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stageMirrorIfForeign uses os.UserCacheDir, which honors XDG_CACHE_HOME only on Unix; on Windows it returns %LocalAppData% with no env redirect, so the mirror cannot be staged under the test's temp cache root without prod changes")
	}
	srcRoot := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: "/home/observer", OS: crossmount.OSLinux, Origin: "native"},
			{Path: srcRoot, OS: crossmount.OSWindows, Origin: "wsl-mnt:fake"},
		}
	}
	srcDB := filepath.Join(srcRoot, "opencode.db")
	if err := os.WriteFile(srcDB, []byte("DBv1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcDB+"-wal", []byte("WALv1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcDB+"-shm", []byte("SHMv1"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatalf("first mirror: %v", err)
	}
	if first == srcDB {
		t.Fatalf("foreign source returned passthrough; want mirror path")
	}
	if !strings.HasPrefix(first, cacheRoot) {
		t.Errorf("mirror path %q must be under cache root %q", first, cacheRoot)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		got, err := os.ReadFile(first + suffix)
		if err != nil {
			t.Fatalf("read mirror sibling %s: %v", suffix, err)
		}
		want := map[string]string{"": "DBv1", "-wal": "WALv1", "-shm": "SHMv1"}[suffix]
		if string(got) != want {
			t.Errorf("mirror %s body = %q, want %q", suffix, got, want)
		}
	}

	// Backdate every source sibling so the mirror is clearly fresher
	// than the source — that's the condition mirrorUpToDate checks.
	// Snapshot the mirror's mtime BEFORE the second call so we can
	// detect a re-copy by observing the mtime move forward.
	past := time.Now().Add(-time.Hour)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chtimes(srcDB+suffix, past, past); err != nil {
			t.Fatal(err)
		}
	}
	beforeSecond, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}

	second, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	if second != first {
		t.Errorf("repeat call returned %q, want %q (same per-source mirror)", second, first)
	}
	afterSecond, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !afterSecond.ModTime().Equal(beforeSecond.ModTime()) {
		t.Errorf("mirror mtime changed (%v -> %v); repeat call must skip the copy when source is unchanged",
			beforeSecond.ModTime(), afterSecond.ModTime())
	}
}

// TestStageMirrorIfForeign_RefreshesOnSourceWALAdvance pins the WAL-
// triggered refresh — the main .db's mtime can stay stable while the
// WAL advances on every flush, so the mirror must re-copy when the
// WAL is newer than the mirror's WAL.
func TestStageMirrorIfForeign_RefreshesOnSourceWALAdvance(t *testing.T) {
	srcRoot := t.TempDir()
	cacheRoot := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	orig := allHomesFunc
	t.Cleanup(func() { allHomesFunc = orig })
	allHomesFunc = func() []crossmount.HomeRoot {
		return []crossmount.HomeRoot{
			{Path: srcRoot, OS: crossmount.OSWindows, Origin: "wsl-mnt:fake"},
		}
	}
	srcDB := filepath.Join(srcRoot, "opencode.db")
	if err := os.WriteFile(srcDB, []byte("DBv1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcDB+"-wal", []byte("WALv1"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := stageMirrorIfForeign(srcDB)
	if err != nil {
		t.Fatalf("first mirror: %v", err)
	}

	// Advance ONLY the WAL — the main .db stays unchanged. Force a
	// distinctly-newer mtime to defeat the same-second tick.
	if err := os.WriteFile(srcDB+"-wal", []byte("WALv1-advanced"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(srcDB+"-wal", future, future); err != nil {
		t.Fatal(err)
	}

	if _, err := stageMirrorIfForeign(srcDB); err != nil {
		t.Fatalf("second mirror: %v", err)
	}
	got, err := os.ReadFile(first + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "WALv1-advanced" {
		t.Errorf("mirror -wal = %q, want %q (refresh must trigger on WAL mtime advance even when main .db is stable)", got, "WALv1-advanced")
	}
}

func setupOpenCodeDBWithVariant(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 5000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_user', 'ses_1', 1000, 1001,
			 '{"role":"user","agent":"build","model":{"providerID":"openai","modelID":"gpt-5.4-nano"},"time":{"created":1000}}'),
			('msg_asst', 'ses_1', 2000, 5000,
			 '{"role":"assistant","agent":"build","variant":"high","modelID":"gpt-5.4-nano","providerID":"openai","path":{"cwd":"/tmp/oc"},"time":{"created":2000,"completed":5000},"finish":"stop"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_user_text',  'msg_user', 'ses_1', 1000, 1001, '{"type":"text","text":"Do work"}'),
			('prt_asst_text',  'msg_asst', 'ses_1', 2100, 2200, '{"type":"text","text":"Working on it."}'),
			('prt_tool',       'msg_asst', 'ses_1', 2300, 2400, '{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"ls"},"output":"ok","title":"ls","time":{"start":2300,"end":2400}}}'),
			('prt_subtask',    'msg_asst', 'ses_1', 2500, 2600, '{"type":"subtask","prompt":"plan","description":"plan","agent":"Plan","model":{"providerID":"openai","modelID":"gpt-5.4-nano"},"time":{"created":2500}}'),
			('prt_reasoning',  'msg_asst', 'ses_1', 2700, 2800, '{"type":"reasoning","text":"Thinking step.","time":{"start":2700,"end":2800}}'),
			('prt_step',       'msg_asst', 'ses_1', 2900, 3000, '{"type":"step-finish","reason":"tool-calls","tokens":{"input":10,"output":5,"reasoning":0,"total":15,"cache":{"read":0,"write":0}},"cost":0.0001}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func setupOpenCodeDBWithReasoning(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 7500)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_asst', 'ses_1', 2000, 7500,
			 '{"role":"assistant","agent":"build","modelID":"gpt-5.4-nano","providerID":"openai","path":{"cwd":"/tmp/oc"},"time":{"created":2000,"completed":7500},"finish":"stop"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_reasoning', 'msg_asst', 'ses_1', 3000, 7500, '{"type":"reasoning","text":"Considering the next step...","time":{"start":3000,"end":7500}}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func setupOpenCodeDBWithStepFinish(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_asst', 'ses_1', 2000, 3000,
			 '{"role":"assistant","agent":"build","modelID":"gpt-5.4-nano","providerID":"openai","path":{"cwd":"/tmp/oc"},"time":{"created":2000,"completed":3000},"finish":"tool-calls"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_step', 'msg_asst', 'ses_1', 2500, 3000, '{"type":"step-finish","reason":"tool-calls","tokens":{"input":7720,"output":199,"reasoning":76,"total":7995,"cache":{"read":0,"write":0}},"cost":0.00188775}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func setupOpenCodeDBWithAssistantText(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_asst', 'ses_1', 2000, 2500,
			 '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"/tmp/oc"},"time":{"created":2000,"completed":2500},"finish":"stop"}'),
			('msg_user', 'ses_1', 1000, 1001,
			 '{"role":"user","agent":"build","time":{"created":1000}}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_user_text', 'msg_user', 'ses_1', 1000, 1001, '{"type":"text","text":"Run the thing"}'),
			('prt_asst_1', 'msg_asst', 'ses_1', 2100, 2200, '{"type":"text","text":"First reasoning chunk."}'),
			('prt_asst_2', 'msg_asst', 'ses_1', 2300, 2400, '{"type":"text","text":"Second reasoning chunk."}'),
			('prt_asst_empty', 'msg_asst', 'ses_1', 2350, 2400, '{"type":"text","text":"   "}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

func setupOpenCodeDB(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stmts := []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY,
			directory TEXT NOT NULL,
			time_updated INTEGER NOT NULL
		)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY,
			message_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			data TEXT NOT NULL
		)`,
		`INSERT INTO session(id, directory, time_updated) VALUES
			('ses_1', 'D:\\programsx\\open-code-test', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_user', 'ses_1', 1000, 1001, '{"role":"user","agent":"build","model":{"providerID":"opencode","modelID":"big-pickle"},"time":{"created":1000}}'),
			('msg_tool', 'ses_1', 2000, 2500, '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"D:\\programsx\\open-code-test"},"time":{"created":2000,"completed":2500},"finish":"tool-calls"}'),
			('msg_done', 'ses_1', 2900, 3000, '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"D:\\programsx\\open-code-test"},"time":{"created":2900,"completed":3000},"finish":"stop"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_prompt', 'msg_user', 'ses_1', 1000, 1001, '{"type":"text","text":"Build the app"}'),
			('prt_tool', 'msg_tool', 'ses_1', 2200, 2500, '{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"npm start","description":"Run app"},"output":"boom","metadata":{"output":"boom","exit":1,"description":"Run app","truncated":false},"title":"Run app","time":{"start":2200,"end":2500}}}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupOpenCodeDBWithSubtask seeds a session with one subtask-typed
// part — the parent invoking an Explore subagent on a haiku model.
func setupOpenCodeDBWithSubtask(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_a', 'ses_1', 2900, 3000,
			 '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"/tmp/oc"},"time":{"created":2900,"completed":3000},"finish":"tool-calls"}')`,
		`INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES
			('prt_subtask', 'msg_a', 'ses_1', 2950, 3000,
			 '{"type":"subtask","prompt":"explore the codebase","description":"map the repo","agent":"Explore","model":{"providerID":"anthropic","modelID":"claude-haiku-4-5"},"time":{"created":2950}}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupOpenCodeDBWithTodos seeds the todo table with two entries —
// one pending, one completed — to exercise loadTodoEvents.
func setupOpenCodeDBWithTodos(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE todo (
			session_id TEXT NOT NULL,
			content TEXT NOT NULL,
			status TEXT NOT NULL,
			priority TEXT NOT NULL,
			position INTEGER NOT NULL,
			time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL,
			PRIMARY KEY (session_id, position)
		)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		`INSERT INTO todo VALUES
			('ses_1', 'Refactor module X', 'pending',     'high', 0, 1000, 1000),
			('ses_1', 'Run go test',       'completed',   'med',  1, 1100, 1500)`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}

// setupOpenCodeDBWithTokens seeds a minimal opencode.db with one
// assistant message carrying the full InfoData token bundle (input,
// output, reasoning, cache.read, cache.write, cost). Used by the
// token-event regression test to assert each field flows through.
func setupOpenCodeDBWithTokens(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session(id, directory, time_updated) VALUES ('ses_1', '/tmp/oc', 3000)`,
		// Numbers chosen deliberately so each field's drop-through can
		// be asserted independently:
		//   input=1234, output=567, reasoning=89,
		//   cache.read=12345, cache.write=678, cost=0.0532
		`INSERT INTO message(id, session_id, time_created, time_updated, data) VALUES
			('msg_done', 'ses_1', 2900, 3000,
			 '{"role":"assistant","agent":"build","modelID":"big-pickle","providerID":"opencode","path":{"cwd":"/tmp/oc"},"time":{"created":2900,"completed":3000},"finish":"stop","tokens":{"input":1234,"output":567,"reasoning":89,"cache":{"read":12345,"write":678}},"cost":0.0532}')`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
}
