package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// modernFixture copies a testdata file into a temp dir under a workspaceStorage
// path so projectRootFromPath has something realistic to walk over.
func modernFixture(t *testing.T, fixtureName string, modernSubpath string) string {
	t.Helper()
	src := filepath.Join("..", "..", "..", "testdata", "copilot", fixtureName)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	root := t.TempDir()
	// modernSubpath is e.g. "workspaceStorage/ws-1/chatSessions" or
	// "globalStorage/emptyWindowChatSessions".
	dir := filepath.Join(root, filepath.FromSlash(modernSubpath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, strings.TrimSuffix(fixtureName, ".jsonl")+".jsonl")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseModern_EmptySnapshot(t *testing.T) {
	path := modernFixture(t, "modern-empty.jsonl", "globalStorage/emptyWindowChatSessions")

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.ToolEvents) != 0 || len(res.TokenEvents) != 0 {
		t.Fatalf("empty snapshot should produce no events: tool=%d token=%d", len(res.ToolEvents), len(res.TokenEvents))
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	stat, _ := os.Stat(path)
	if res.NewOffset != stat.Size() {
		t.Fatalf("NewOffset: got %d want %d", res.NewOffset, stat.Size())
	}
}

func TestParseModern_SingleTurn(t *testing.T) {
	path := modernFixture(t, "modern-singleturn.jsonl", "workspaceStorage/ws-1/chatSessions")

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// Expect: 1 user_prompt + 2 tool calls (list_dir, view_image) + 1 task_complete = 4
	if len(res.ToolEvents) != 4 {
		t.Fatalf("ToolEvents: got %d want 4 (%v)", len(res.ToolEvents), eventSummary(res.ToolEvents))
	}
	if res.ToolEvents[0].ActionType != models.ActionUserPrompt {
		t.Fatalf("event[0] action: %s", res.ToolEvents[0].ActionType)
	}
	if got := res.ToolEvents[0].Target; got != "What is being shown here?" {
		t.Fatalf("user prompt target: %q", got)
	}
	if res.ToolEvents[1].ActionType != models.ActionSearchFiles || res.ToolEvents[1].RawToolName != "list_dir" {
		t.Fatalf("event[1] mismatch: %+v", res.ToolEvents[1])
	}
	if res.ToolEvents[2].ActionType != models.ActionReadFile || res.ToolEvents[2].RawToolName != "view_image" {
		t.Fatalf("event[2] mismatch: %+v", res.ToolEvents[2])
	}
	if res.ToolEvents[3].ActionType != models.ActionTaskComplete {
		t.Fatalf("event[3] action: %s", res.ToolEvents[3].ActionType)
	}
	if !strings.Contains(res.ToolEvents[3].ToolOutput, "I cannot describe images directly.") {
		t.Fatalf("task_complete output missing assistant text: %q", res.ToolEvents[3].ToolOutput)
	}
	if !res.ToolEvents[3].Success {
		t.Fatalf("task_complete should be Success (req.isComplete=true)")
	}
	// Model: prefer resolvedModel ("claude-haiku-4-5-20251001") over modelId ("copilot/auto").
	for i, ev := range res.ToolEvents {
		if ev.Model != "claude-haiku-4-5-20251001" {
			t.Fatalf("event[%d] model: got %q want claude-haiku-4-5-20251001", i, ev.Model)
		}
	}
	// Tokens: promptTokens=30401, completionTokens=137, reasoning sum = 42+11+0 = 53
	if len(res.TokenEvents) != 1 {
		t.Fatalf("TokenEvents: got %d want 1", len(res.TokenEvents))
	}
	te := res.TokenEvents[0]
	if te.InputTokens != 30401 || te.OutputTokens != 137 {
		t.Fatalf("tokens: in=%d out=%d", te.InputTokens, te.OutputTokens)
	}
	if te.ReasoningTokens != 53 {
		t.Fatalf("reasoning tokens: got %d want 53 (sum of round thinking.tokens)", te.ReasoningTokens)
	}
	if te.Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("token event model: got %q want claude-haiku-4-5-20251001", te.Model)
	}
	if te.Tool != models.ToolCopilot || te.Source != models.TokenSourceJSONL || te.Reliability != models.ReliabilityApproximate {
		t.Fatalf("token labels: %+v", te)
	}
	// Per-event SourceEventID dedupe keys
	for _, ev := range res.ToolEvents {
		if ev.SourceEventID == "" {
			t.Fatalf("missing SourceEventID: %+v", ev)
		}
	}
	// Image content must not be ingested verbatim — toolResultExcerpt
	// should produce a marker, not base64.
	for _, ev := range res.ToolEvents {
		if strings.Contains(ev.ToolOutput, "<<elided-base64-image-bytes>>") {
			t.Fatalf("event leaked elided image marker: %s", ev.ToolOutput)
		}
		if ev.RawToolName == "view_image" && !strings.HasPrefix(ev.ToolOutput, "<image:") {
			t.Fatalf("view_image output should be image/* marker, got %q", ev.ToolOutput)
		}
	}
}

func TestParseModern_MultiSnapshotDedupesViaRequestId(t *testing.T) {
	path := modernFixture(t, "modern-resnapshot.jsonl", "workspaceStorage/ws-2/chatSessions")

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// The first snapshot has requests:[]; a kind=1 patch sets requests=[req_a];
	// then a NEW kind=0 snapshot replaces it with [req_a, req_b]. The parser
	// must use the LAST snapshot — both turns appear, in order.
	requestIDs := make(map[string]int)
	for _, ev := range res.ToolEvents {
		// every event's source event id starts with the request id
		idx := strings.Index(ev.SourceEventID, ":")
		if idx < 0 {
			continue
		}
		requestIDs[ev.SourceEventID[:idx]]++
	}
	if _, ok := requestIDs["request_aaaaaaaa-1111"]; !ok {
		t.Fatalf("first turn missing from output: %v", requestIDs)
	}
	if _, ok := requestIDs["request_bbbbbbbb-2222"]; !ok {
		t.Fatalf("second turn missing from output: %v", requestIDs)
	}
	// req_a should appear EXACTLY ONCE (no double-emit from earlier snapshot
	// being merged with the later one). user_prompt + task_complete = 2 events.
	if requestIDs["request_aaaaaaaa-1111"] != 2 {
		t.Fatalf("first turn double-emitted across snapshots: events=%d", requestIDs["request_aaaaaaaa-1111"])
	}
	// req_b adds 1 read_file tool call → 3 events.
	if requestIDs["request_bbbbbbbb-2222"] != 3 {
		t.Fatalf("second turn event count: got %d want 3", requestIDs["request_bbbbbbbb-2222"])
	}
}

func TestParseModern_SkipsInputStateAttachmentPatches(t *testing.T) {
	path := modernFixture(t, "modern-skip-attachments.jsonl", "workspaceStorage/ws-3/chatSessions")

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	// The fixture's kind=1 attachments line embeds a sentinel string. The
	// parser must NEVER incorporate it into any emitted event — it should
	// skip the line entirely (k[0] != "requests").
	const sentinel = "SENTINEL_DO_NOT_INGEST_THIS_VALUE_INTO_ANY_EVENT_DDB7C2A1"
	for _, ev := range res.ToolEvents {
		fields := []string{ev.Target, ev.RawToolInput, ev.ToolOutput, ev.PrecedingReasoning}
		for _, fld := range fields {
			if strings.Contains(fld, sentinel) {
				t.Fatalf("attachment sentinel leaked into event: %+v", ev)
			}
		}
	}
	for _, te := range res.TokenEvents {
		if strings.Contains(te.SourceEventID, sentinel) {
			t.Fatalf("attachment sentinel leaked into token event: %+v", te)
		}
	}
	// Sanity: the one real turn in the fixture still produces events.
	if len(res.ToolEvents) == 0 {
		t.Fatalf("expected at least one event from the kind=0 turn")
	}
}

func TestParseModern_Kind2_AppendsNewTurn(t *testing.T) {
	path := modernFixture(t, "modern-kind2-append.jsonl", "workspaceStorage/ws-k2a/chatSessions")

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// Snapshot has 1 turn (request_first-1111); kind=2 appends a 2nd turn
	// (request_second-2222). Both must surface.
	requestIDs := make(map[string]int)
	for _, ev := range res.ToolEvents {
		if i := strings.Index(ev.SourceEventID, ":"); i > 0 {
			requestIDs[ev.SourceEventID[:i]]++
		}
	}
	if requestIDs["request_first-1111"] == 0 {
		t.Fatalf("first turn (snapshot) missing: %v", requestIDs)
	}
	if requestIDs["request_second-2222"] == 0 {
		t.Fatalf("second turn (kind=2 append) missing: %v", requestIDs)
	}
	// Models: turn 1 uses grok-code-fast-1; turn 2 uses
	// capi-noe-ptuc-h200-oswe-vscode-prime. Verify resolvedModel applies
	// per-turn rather than carrying over.
	models := map[string]string{}
	for _, ev := range res.ToolEvents {
		if i := strings.Index(ev.SourceEventID, ":"); i > 0 {
			models[ev.SourceEventID[:i]] = ev.Model
		}
	}
	if models["request_first-1111"] != "grok-code-fast-1" {
		t.Fatalf("turn 1 model: got %q want grok-code-fast-1", models["request_first-1111"])
	}
	if models["request_second-2222"] != "capi-noe-ptuc-h200-oswe-vscode-prime" {
		t.Fatalf("turn 2 model: got %q", models["request_second-2222"])
	}
	// Token rows for both turns.
	if len(res.TokenEvents) != 2 {
		t.Fatalf("TokenEvents: got %d want 2", len(res.TokenEvents))
	}
}

func TestParseModern_Kind2_InsertsResponseElements(t *testing.T) {
	path := modernFixture(t, "modern-kind2-insert.jsonl", "workspaceStorage/ws-k2i/chatSessions")

	res, err := New().ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	// Snapshot's response[] starts as ["first response chunk", "second response chunk"];
	// kind=2 inserts ["INSERTED chunk at index 1", "INSERTED chunk at index 2"] at i=1.
	// The task_complete event should contain ALL FOUR strings in order.
	var complete *models.ToolEvent
	for i := range res.ToolEvents {
		if res.ToolEvents[i].ActionType == models.ActionTaskComplete {
			complete = &res.ToolEvents[i]
			break
		}
	}
	if complete == nil {
		t.Fatalf("no task_complete event")
	}
	want := []string{
		"first response chunk",
		"INSERTED chunk at index 1",
		"INSERTED chunk at index 2",
		"second response chunk",
	}
	prev := 0
	for _, s := range want {
		idx := strings.Index(complete.ToolOutput[prev:], s)
		if idx < 0 {
			t.Fatalf("missing %q (or wrong order) in task_complete output:\n%s", s, complete.ToolOutput)
		}
		prev += idx + len(s)
	}
}

func TestApplyInsert_AppendAndIndex(t *testing.T) {
	cases := []struct {
		name string
		seed map[string]any
		k    []any
		i    int
		hasI bool
		v    []any
		want []any
	}{
		{
			name: "append to empty",
			seed: map[string]any{"requests": []any{}},
			k:    []any{"requests"},
			hasI: false,
			v:    []any{"x", "y"},
			want: []any{"x", "y"},
		},
		{
			name: "insert at index 1",
			seed: map[string]any{"requests": []any{"a", "b", "c"}},
			k:    []any{"requests"},
			i:    1,
			hasI: true,
			v:    []any{"X", "Y"},
			want: []any{"a", "X", "Y", "b", "c"},
		},
		{
			name: "insert into nested array via map+slice path",
			seed: map[string]any{"requests": []any{map[string]any{"response": []any{"a", "c"}}}},
			k:    []any{"requests", float64(0), "response"},
			i:    1,
			hasI: true,
			v:    []any{"b"},
			want: []any{"a", "b", "c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyInsert(tc.seed, tc.k, tc.i, tc.hasI, tc.v); err != nil {
				t.Fatalf("applyInsert: %v", err)
			}
			cur := any(tc.seed)
			for _, key := range tc.k[:len(tc.k)-1] {
				next, ok := descend(cur, key)
				if !ok {
					t.Fatalf("descend miss at %v", key)
				}
				cur = next
			}
			got, _ := descend(cur, tc.k[len(tc.k)-1])
			gotS, _ := got.([]any)
			if len(gotS) != len(tc.want) {
				t.Fatalf("len got %d want %d (%v)", len(gotS), len(tc.want), gotS)
			}
			for i, w := range tc.want {
				if gotS[i] != w {
					t.Fatalf("[%d] got %v want %v", i, gotS[i], w)
				}
			}
		})
	}
}

func TestApplyPatch_PathDepths(t *testing.T) {
	cases := []struct {
		name    string
		seed    map[string]any
		k       []any
		v       any
		wantVal any
		path    []any
	}{
		{
			name:    "top-level replace",
			seed:    map[string]any{"requests": []any{}},
			k:       []any{"requests"},
			v:       []any{"new"},
			wantVal: []any{"new"},
			path:    []any{"requests"},
		},
		{
			name:    "leaf scalar in nested map",
			seed:    map[string]any{"requests": []any{map[string]any{"completionTokens": float64(0)}}},
			k:       []any{"requests", float64(0), "completionTokens"},
			v:       float64(75),
			wantVal: float64(75),
			path:    []any{"requests", float64(0), "completionTokens"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyPatch(tc.seed, tc.k, tc.v); err != nil {
				t.Fatalf("applyPatch: %v", err)
			}
			cur := any(tc.seed)
			for _, key := range tc.path[:len(tc.path)-1] {
				next, ok := descend(cur, key)
				if !ok {
					t.Fatalf("descend miss at %v", key)
				}
				cur = next
			}
			got, _ := descend(cur, tc.path[len(tc.path)-1])
			// Compare via go's deep-equal-ish: shallow equality is enough for these scalars/slices.
			if gotS, ok := got.([]any); ok {
				wantS, _ := tc.wantVal.([]any)
				if len(gotS) != len(wantS) || (len(gotS) > 0 && gotS[0] != wantS[0]) {
					t.Fatalf("got %v want %v", got, tc.wantVal)
				}
				return
			}
			if got != tc.wantVal {
				t.Fatalf("got %v want %v", got, tc.wantVal)
			}
		})
	}
}

func TestMapToolName_ModernCamelAndSnake(t *testing.T) {
	cases := map[string]string{
		"runInTerminal":          models.ActionRunCommand,
		"run_in_terminal":        models.ActionRunCommand,
		"editFiles":              models.ActionEditFile,
		"replaceStringInFile":    models.ActionEditFile,
		"replace_string_in_file": models.ActionEditFile,
		"readFile":               models.ActionReadFile,
		"read_file":              models.ActionReadFile,
		"view_image":             models.ActionReadFile,
		"list_dir":               models.ActionSearchFiles,
		"fileSearch":             models.ActionSearchFiles,
		"findTextInFiles":        models.ActionSearchText,
		"grep_search":            models.ActionSearchText,
		"webSearch":              models.ActionWebSearch,
		"fetchWebpage":           models.ActionWebFetch,
		"manage_todo_list":       models.ActionTodoUpdate,
		"runSubagent":            models.ActionSpawnSubagent,
		"unknown_thing":          models.ActionUnknown,
	}
	for name, want := range cases {
		if got := mapToolName(name); got != want {
			t.Errorf("mapToolName(%q) = %q want %q", name, got, want)
		}
	}
}

func eventSummary(events []models.ToolEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.ActionType+":"+ev.RawToolName)
	}
	return out
}
