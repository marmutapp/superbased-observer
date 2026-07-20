package conversation

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/stash"
)

// helperOpenAIResponsesBody builds a minimal Responses-API request body
// with `input` items and `tools`. Used as the fixture base for
// codex-shape tests.
func helperOpenAIResponsesBody(t *testing.T, items []map[string]any, tools []map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":            "gpt-5-codex",
		"input":            items,
		"tools":            tools,
		"prompt_cache_key": "test-session-id",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// TestResolveOpenAIResponsesToolCalls_PopulatesNameAndFilename pins the
// call_id → {toolName, filePath} resolver. Without this, per-block
// compression dispatch can't find the producing tool's name and the
// MCP-skip predicate misses MCP-prefixed bare names.
func TestResolveOpenAIResponsesToolCalls_PopulatesNameAndFilename(t *testing.T) {
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":      "function_call",
			"call_id":   "call_read_main",
			"name":      "exec_command",
			"arguments": `{"cmd":"cat main.go","file_path":"main.go"}`,
		},
		{
			"type":    "function_call_output",
			"call_id": "call_read_main",
			"output":  "package main\n\nfunc main() {}\n",
		},
		{
			"type":      "function_call",
			"call_id":   "call_search",
			"name":      "mcp__observer_capture__search_past_outputs",
			"arguments": `{"query":"foo"}`,
		},
		{
			"type":    "function_call_output",
			"call_id": "call_search",
			"output":  `{"hits":[]}`,
		},
	}, nil)

	_, ex, ok := openaiResponsesExtract(body)
	if !ok {
		t.Fatalf("openaiResponsesExtract: not ok")
	}
	if len(ex) != 4 {
		t.Fatalf("expected 4 extracted, got %d", len(ex))
	}
	// Find the function_call_output items by call_id.
	var readOut, searchOut *openaiResponsesExtractedMessage
	for i := range ex {
		if ex[i].itemType != "function_call_output" {
			continue
		}
		switch ex[i].toolCallID {
		case "call_read_main":
			readOut = &ex[i]
		case "call_search":
			searchOut = &ex[i]
		}
	}
	if readOut == nil || searchOut == nil {
		t.Fatalf("expected both function_call_output items resolved")
	}
	if readOut.toolName != "exec_command" {
		t.Errorf("readOut.toolName = %q, want exec_command", readOut.toolName)
	}
	if readOut.filename != "main.go" {
		t.Errorf("readOut.filename = %q, want main.go", readOut.filename)
	}
	if searchOut.toolName != "mcp__observer_capture__search_past_outputs" {
		t.Errorf("searchOut.toolName = %q, want mcp__observer_capture__search_past_outputs", searchOut.toolName)
	}
}

// TestCollectOpenAIMCPToolNames pins the namespace walk: nested function
// names land in the set in BOTH bare and prefixed forms so the predicate
// works regardless of which form codex emits on the wire.
func TestCollectOpenAIMCPToolNames(t *testing.T) {
	envelope := map[string]json.RawMessage{}
	tools := []any{
		map[string]any{"type": "function", "name": "exec_command"},
		map[string]any{
			"type":        "namespace",
			"name":        "mcp__observer_capture__",
			"description": "Tools in the mcp__observer_capture__ namespace.",
			"tools": []any{
				map[string]any{"type": "function", "name": "check_file_freshness"},
				map[string]any{"type": "function", "name": "retrieve_stashed"},
			},
		},
		// Non-mcp namespace — should be ignored.
		map[string]any{
			"type": "namespace",
			"name": "team_internal__",
			"tools": []any{
				map[string]any{"type": "function", "name": "internal_tool"},
			},
		},
	}
	rawTools, _ := json.Marshal(tools)
	envelope["tools"] = rawTools

	got := collectOpenAIMCPToolNames(envelope)
	wantPresent := []string{
		"check_file_freshness",
		"retrieve_stashed",
		"mcp__observer_capture__check_file_freshness",
		"mcp__observer_capture__retrieve_stashed",
	}
	for _, name := range wantPresent {
		if !got[name] {
			t.Errorf("expected mcpSet to contain %q, set=%v", name, got)
		}
	}
	wantAbsent := []string{"exec_command", "internal_tool", "team_internal__internal_tool"}
	for _, name := range wantAbsent {
		if got[name] {
			t.Errorf("did NOT expect mcpSet to contain %q (non-MCP), set=%v", name, got)
		}
	}
}

// TestIsOpenAIMCPCall_PrefixOrSet covers the union predicate: name in
// the set OR name has the `mcp__` prefix returns true. Either alone is
// sufficient; both alone is the common case.
func TestIsOpenAIMCPCall_PrefixOrSet(t *testing.T) {
	mcpSet := map[string]bool{
		"check_file_freshness":                        true,
		"mcp__observer_capture__check_file_freshness": true,
	}
	cases := []struct {
		name string
		want bool
	}{
		{"check_file_freshness", true},                        // bare name in set
		{"mcp__observer_capture__check_file_freshness", true}, // prefixed name in set
		{"mcp__some_other_server__some_tool", true},           // not in set, but HasPrefix matches
		{"exec_command", false},                               // not in set, no prefix
		{"", false},                                           // empty
	}
	for _, tc := range cases {
		got := isOpenAIMCPCall(tc.name, mcpSet)
		if got != tc.want {
			t.Errorf("isOpenAIMCPCall(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCompressOpenAIResponsesToolResults_PerTypeFires verifies that a
// JSON-shaped function_call_output body gets compressed via the JSON
// per-type compressor when its producing function_call's name is
// non-MCP. Same shape that fires on the Anthropic side.
func TestCompressOpenAIResponsesToolResults_PerTypeFires(t *testing.T) {
	bigJSON := `{"items":[`
	for i := 0; i < 50; i++ {
		if i > 0 {
			bigJSON += ","
		}
		bigJSON += `{"id":` + itoa(i) + `,"name":"item-` + itoa(i) + `"}`
	}
	bigJSON += `]}`

	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":      "function_call",
			"call_id":   "call_dump",
			"name":      "exec_command",
			"arguments": `{"cmd":"cat dump.json"}`,
		},
		{
			"type":    "function_call_output",
			"call_id": "call_dump",
			"output":  bigJSON,
		},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("expected pipeline to run, got skipped")
	}
	jsonEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "json" {
			jsonEvent = true
			break
		}
	}
	if !jsonEvent {
		t.Errorf("expected json mechanism event, got events=%+v", got.Events)
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Errorf("expected body to shrink, original=%d compressed=%d", got.OriginalBytes, got.CompressedBytes)
	}
}

// TestCompressOpenAIResponsesToolResults_SkipsMCP pins the critical
// correctness invariant: MCP tool_results MUST NEVER be compressed via
// the per-type pipeline. Mirrors TestCompressToolResults_SkipsMCPToolResults
// from the Anthropic side.
func TestCompressOpenAIResponsesToolResults_SkipsMCP(t *testing.T) {
	bigSearchResp := `{"query":"app.set","count":10,"hits":[`
	for i := 0; i < 10; i++ {
		if i > 0 {
			bigSearchResp += ","
		}
		bigSearchResp += `{"action_id":` + itoa(1000+i) +
			`,"tool_name":"Bash","target":"grep app.set","excerpt":"app.set('foo','bar')","rank":-` + itoa(i) + `.5}`
	}
	bigSearchResp += `]}`

	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":      "function_call",
			"call_id":   "call_search",
			"name":      "mcp__observer__search_past_outputs",
			"arguments": `{"query":"app.set"}`,
		},
		{
			"type":    "function_call_output",
			"call_id": "call_search",
			"output":  bigSearchResp,
		},
	}, []map[string]any{
		{
			"type":        "namespace",
			"name":        "mcp__observer__",
			"description": "Observer MCP tools.",
			"tools": []any{
				map[string]any{"type": "function", "name": "search_past_outputs"},
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	for _, ev := range got.Events {
		switch ev.Mechanism {
		case "json", "code", "logs", "stash":
			t.Errorf("MCP tool_result was compressed via %q (would corrupt query data); event=%+v", ev.Mechanism, ev)
		}
	}
	bodyStr := string(got.Body)
	if !strings.Contains(bodyStr, `1000`) || !strings.Contains(bodyStr, `1009`) {
		t.Errorf("action_id values stripped from MCP response; output=%s", bodyStr)
	}
	if strings.Contains(bodyStr, `<number>`) {
		t.Errorf("JSON compression scalars leaked into MCP response; output=%s", bodyStr)
	}
	if strings.Contains(bodyStr, `<string>`) {
		t.Errorf("JSON compression scalars leaked into MCP response; output=%s", bodyStr)
	}
}

// TestStashOpenAIResponsesLargeBodies_ReplacesWithMarker verifies that
// a function_call_output body exceeding the threshold gets replaced with
// the canonical stash marker.
func TestStashOpenAIResponsesLargeBodies_ReplacesWithMarker(t *testing.T) {
	bigBody := strings.Repeat("not very compressible content with random stuff ", 500)

	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":      "function_call",
			"call_id":   "call_dump",
			"name":      "exec_command",
			"arguments": `{"cmd":"head -c 99999 /dev/random"}`,
		},
		{
			"type":    "function_call_output",
			"call_id": "call_dump",
			"output":  bigBody,
		},
	}, nil)

	st, err := stash.New(stash.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New()).WithStash(st, 1024)

	got := p.Run("openai", body)
	stashEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "stash" {
			stashEvent = true
			break
		}
	}
	if !stashEvent {
		t.Errorf("expected stash mechanism event, got events=%+v", got.Events)
	}
	if !strings.Contains(string(got.Body), "mcp__observer__retrieve_stashed") {
		t.Errorf("expected stash marker with retrieve_stashed directive in body; output=%s", string(got.Body))
	}
}

// TestStashOpenAIResponsesLargeBodies_SkipsMCP verifies the stash pass
// also honors the MCP-skip predicate. A retrieve_stashed-style MCP
// tool_result must NEVER be re-stashed (would loop the model on a hash
// that matches its own retrieved bytes).
func TestStashOpenAIResponsesLargeBodies_SkipsMCP(t *testing.T) {
	bigBody := strings.Repeat("retrieved content from earlier stash ", 500)

	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":      "function_call",
			"call_id":   "call_retrieve",
			"name":      "mcp__observer__retrieve_stashed",
			"arguments": `{"sha":"abc123"}`,
		},
		{
			"type":    "function_call_output",
			"call_id": "call_retrieve",
			"output":  bigBody,
		},
	}, []map[string]any{
		{
			"type": "namespace",
			"name": "mcp__observer__",
			"tools": []any{
				map[string]any{"type": "function", "name": "retrieve_stashed"},
			},
		},
	})

	st, err := stash.New(stash.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New()).WithStash(st, 1024)

	got := p.Run("openai", body)
	for _, ev := range got.Events {
		if ev.Mechanism == "stash" {
			t.Errorf("MCP retrieve_stashed output was re-stashed; would loop the model. event=%+v", ev)
		}
	}
}

// TestCompressOpenAIResponsesToolDefinitions_TrimsAndStripsExamples
// verifies the two transforms apply to top-level function-typed tools.
// Forbidden list: parameters / properties / required / enum / numeric
// bounds untouched.
func TestCompressOpenAIResponsesToolDefinitions_TrimsAndStripsExamples(t *testing.T) {
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		},
	}, []map[string]any{
		{
			"type":        "function",
			"name":        "exec_command",
			"description": "Para 1.\n\nPara 2.\n\nPara 3 should be trimmed.\n\nPara 4 also trimmed.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{
						"type":        "string",
						"description": "Shell command",
						"examples":    []string{"ls", "pwd"},
						"minLength":   1,
					},
				},
				"required": []string{"cmd"},
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		// V7-21: tool-defs trim is opt-in via the "tools" sentinel.
		CompressTypes: []string{"json", "logs", "code", "tools"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	toolsEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "tools" {
			toolsEvent = true
			break
		}
	}
	if !toolsEvent {
		t.Errorf("expected tools mechanism event, got events=%+v", got.Events)
	}
	bodyStr := string(got.Body)
	if strings.Contains(bodyStr, "Para 3 should be trimmed") {
		t.Errorf("description tail not trimmed; output contains Para 3")
	}
	if strings.Contains(bodyStr, `"examples"`) {
		t.Errorf("examples not stripped from parameters; output=%s", bodyStr)
	}
	// Forbidden list — these MUST survive.
	for _, want := range []string{`"properties"`, `"required"`, `"type"`, `"minLength"`, `"cmd"`} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("forbidden-list field %q stripped from parameters; output=%s", want, bodyStr)
		}
	}
}

// TestCompressOpenAIResponsesToolDefinitions_RecursesIntoNamespace
// verifies that namespace-typed tools have their nested function tools
// trimmed in the same way. Critical for codex MCP namespaces — without
// recursion, the 8KB+ MCP namespace stays uncompressed.
func TestCompressOpenAIResponsesToolDefinitions_RecursesIntoNamespace(t *testing.T) {
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hi"}},
		},
	}, []map[string]any{
		{
			"type":        "namespace",
			"name":        "mcp__observer_capture__",
			"description": "Tools.\n\nUsage notes.\n\nRetrieval semantics.\n\nMore.",
			"tools": []any{
				map[string]any{
					"type":        "function",
					"name":        "check_file_freshness",
					"description": "P1.\n\nP2.\n\nP3 should trim.\n\nP4 too.",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"file_path": map[string]any{
								"type":     "string",
								"examples": []string{"/foo", "/bar"},
							},
						},
					},
				},
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		// V7-21: tool-defs trim is opt-in via the "tools" sentinel.
		CompressTypes: []string{"json", "logs", "code", "tools"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	bodyStr := string(got.Body)
	if strings.Contains(bodyStr, "P3 should trim") {
		t.Errorf("nested function description tail not trimmed; output=%s", bodyStr)
	}
	if strings.Contains(bodyStr, "Retrieval semantics") {
		t.Errorf("namespace description tail not trimmed; output=%s", bodyStr)
	}
	if strings.Contains(bodyStr, `"examples"`) {
		t.Errorf("nested examples not stripped; output=%s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"check_file_freshness"`) {
		t.Errorf("nested tool name dropped; output=%s", bodyStr)
	}
}

// TestRunOpenAI_FastPathOnZeroEvents pins the critical cache-cost
// invariant: when no events fire, the request body forwarded upstream
// must be byte-identical to the input. Without this, codex's
// `prompt_cache_key` cache lookup misses on every turn even when
// compression is a no-op, and the OpenAI prefix cache can't help.
//
// Mirrors the Anthropic-side regression that ate ~38% of the cache
// savings before the fast-path was added.
func TestRunOpenAI_FastPathOnZeroEvents(t *testing.T) {
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "say hi"}},
		},
	}, []map[string]any{
		// Single function tool with description short enough to skip
		// the trim and a parameters schema with no examples.
		{
			"type":        "function",
			"name":        "exec_command",
			"description": "Run a command.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"cmd": map[string]any{"type": "string"}},
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	if len(got.Events) != 0 {
		t.Errorf("expected zero events on this trivially-clean body, got %d: %+v", len(got.Events), got.Events)
	}
	if got.DroppedCount != 0 {
		t.Errorf("expected zero drops, got %d", got.DroppedCount)
	}
	if !bytes.Equal(got.Body, body) {
		t.Errorf("fast-path violated: body was re-serialized when nothing changed.\noriginal: %s\nforwarded: %s", string(body), string(got.Body))
	}
}

// TestRunOpenAI_ChatCompletions_TrivialBodyFastPath verifies that a
// trivially-clean Chat Completions body (no tool messages, no
// compressible tools array) hits the fast-path early-return: zero
// events fire and the body forwarded upstream is byte-identical to the
// input. Critical for OpenAI's `prompt_cache_key` cache hit on Chat
// Completions traffic — same invariant the Responses-API path was
// hardened against.
func TestRunOpenAI_ChatCompletions_TrivialBodyFastPath(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "gpt-4o-mini",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())
	got := p.Run("openai", body)
	if got.Skipped {
		t.Errorf("expected Chat Completions to run, not skip")
	}
	if len(got.Events) != 0 {
		t.Errorf("expected zero events on trivially-clean body, got %d: %+v", len(got.Events), got.Events)
	}
	if got.DroppedCount != 0 {
		t.Errorf("expected zero drops, got %d", got.DroppedCount)
	}
	if !bytes.Equal(got.Body, body) {
		t.Errorf("fast-path violated on Chat Completions: body re-serialized when nothing changed.\noriginal:  %s\nforwarded: %s", string(body), string(got.Body))
	}
}

// TestExtractFilePathFromArgs covers the file-path heuristic on common
// argument shapes. Explicit path keys take priority; freeform `cmd`
// shapes fall back to the read-style verb scan (cat / head / tail /
// less / more / nl).
func TestExtractFilePathFromArgs(t *testing.T) {
	cases := []struct {
		args string
		want string
	}{
		{`{"file_path":"/abs/main.go"}`, "/abs/main.go"},
		{`{"path":"relative/main.go"}`, "relative/main.go"},
		{`{"filename":"out.json"}`, "out.json"},
		// Codex `exec_command` shapes — read-style verbs.
		{`{"cmd":"cat foo.go"}`, "foo.go"},
		{`{"cmd":"head -50 foo.go"}`, "foo.go"},
		{`{"cmd":"tail -n 100 foo.go"}`, "foo.go"},
		{`{"cmd":"less /path/to/x"}`, "/path/to/x"},
		{`{"cmd":"bash -lc 'cat foo.go'"}`, "foo.go"},
		{`{"command":["bash","-lc","cat foo.go"]}`, "foo.go"},
		// Codex on Windows: shell_command with a PowerShell read verb.
		{`{"command":"Get-Content foo.go"}`, "foo.go"},
		{`{"command":"Get-Content -Path internal/mcp/x.go"}`, "internal/mcp/x.go"},
		{`{"command":"Get-Content -TotalCount 200 a/b/c.py"}`, "a/b/c.py"},
		{`{"command":"type bar.ts"}`, "bar.ts"},
		{`{"command":"gc -LiteralPath 'D:\\proj\\a.go'"}`, `D:\proj\a.go`},
		// PowerShell non-read verb — no path.
		{`{"command":"Select-String -Pattern TODO x.go"}`, ""},
		// Non-read verbs — must NOT return a path.
		{`{"cmd":"grep foo bar.txt"}`, ""},
		{`{"cmd":"ls -la"}`, ""},
		{`{"cmd":"go test ./..."}`, ""},
		// Negative + edge.
		{`{"unrelated":"value"}`, ""},
		{`not json`, ""},
		{``, ""},
	}
	for _, tc := range cases {
		got := extractFilePathFromArgs(tc.args)
		if got != tc.want {
			t.Errorf("extractFilePathFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// TestParseReadCmdForPath covers the read-style verb scanner directly.
func TestParseReadCmdForPath(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"cat foo.go", "foo.go"},
		{"head -50 foo.go", "foo.go"},
		{"tail -n 100 foo.go", "foo.go"},
		{"less /etc/hosts", "/etc/hosts"},
		{"more bar", "bar"},
		{"nl src/main.go", "src/main.go"},
		{"bash -lc 'cat /a/b'", "/a/b"},
		{"sh -c \"cat /a/b\"", "/a/b"},
		// PowerShell / Windows readers codex emits on Windows.
		{"Get-Content foo.go", "foo.go"},
		{"get-content -Path src/x.ts", "src/x.ts"},
		{"Get-Content -TotalCount 200 a/b/c.py", "a/b/c.py"},
		{"Get-Content -Tail 100 file.go", "file.go"},
		{"gc internal/mcp/x.go", "internal/mcp/x.go"},
		{"type bar.rs", "bar.rs"},
		{`Get-Content -LiteralPath 'C:\a\b.go'`, `C:\a\b.go`},
		{"Select-String -Pattern TODO x.go", ""},
		{"grep -n foo bar.txt", ""},
		{"ls", ""},
		{"go test ./...", ""},
		{"", ""},
		{"cat", ""}, // verb only, no path
	}
	for _, tc := range cases {
		got := parseReadCmdForPath(tc.cmd)
		if got != tc.want {
			t.Errorf("parseReadCmdForPath(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// TestSubstituteOpenAIRedundantReads_FiresOnDuplicate pins C16: when
// the same file is read twice in one request body, the second body
// gets replaced with the read-cache marker. Mirrors the Anthropic-side
// TestPipeline_ReadCache_FiresOnDuplicateRead.
func TestSubstituteOpenAIRedundantReads_FiresOnDuplicate(t *testing.T) {
	bigFile := strings.Repeat("package main\n\nfunc main() {}\n\n", 100)
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{"type": "function_call", "call_id": "call_a", "name": "exec_command", "arguments": `{"cmd":"cat foo.go"}`},
		{"type": "function_call_output", "call_id": "call_a", "output": bigFile},
		{"type": "function_call", "call_id": "call_b", "name": "exec_command", "arguments": `{"cmd":"cat foo.go"}`},
		{"type": "function_call_output", "call_id": "call_b", "output": bigFile},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.RunInSession("openai", body, "test-session-id")
	readCacheEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "read_cache" {
			readCacheEvent = true
			break
		}
	}
	if !readCacheEvent {
		t.Errorf("expected read_cache event, got %+v", got.Events)
	}
	if !strings.Contains(string(got.Body), "[file foo.go unchanged since earlier in this turn") {
		t.Errorf("expected read_cache marker in body; output=%s", string(got.Body))
	}
}

// TestSubstituteOpenAIRedundantReads_NoSession verifies the pass
// no-ops when sessionID is empty (parity with the Anthropic side).
func TestSubstituteOpenAIRedundantReads_NoSession(t *testing.T) {
	bigFile := strings.Repeat("package main\n\n", 100)
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{"type": "function_call", "call_id": "call_a", "name": "exec_command", "arguments": `{"cmd":"cat foo.go"}`},
		{"type": "function_call_output", "call_id": "call_a", "output": bigFile},
		{"type": "function_call", "call_id": "call_b", "name": "exec_command", "arguments": `{"cmd":"cat foo.go"}`},
		{"type": "function_call_output", "call_id": "call_b", "output": bigFile},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	// Run() (legacy entry) passes "" for sessionID → C16 should no-op.
	got := p.Run("openai", body)
	for _, ev := range got.Events {
		if ev.Mechanism == "read_cache" {
			t.Errorf("read_cache must not fire without sessionID; got event=%+v", ev)
		}
	}
}

// TestSubstituteOpenAIRedundantReads_SkipsMCP pins the MCP-skip
// invariant — MCP tool_results MUST NEVER be replaced with a read-cache
// marker, even when their (filename, hash) tuple is duplicated.
func TestSubstituteOpenAIRedundantReads_SkipsMCP(t *testing.T) {
	bigOutput := strings.Repeat(`{"hits":[{"action_id":1}]}`, 50)
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{
			"type": "function_call", "call_id": "call_a",
			"name":      "mcp__observer__search_past_outputs",
			"arguments": `{"query":"x","file_path":"/synthetic/hits"}`,
		},
		{"type": "function_call_output", "call_id": "call_a", "output": bigOutput},
		{
			"type": "function_call", "call_id": "call_b",
			"name":      "mcp__observer__search_past_outputs",
			"arguments": `{"query":"x","file_path":"/synthetic/hits"}`,
		},
		{"type": "function_call_output", "call_id": "call_b", "output": bigOutput},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.RunInSession("openai", body, "test-session-id")
	for _, ev := range got.Events {
		if ev.Mechanism == "read_cache" {
			t.Errorf("MCP output got read_cache substituted; would corrupt query data. event=%+v", ev)
		}
	}
}

// helperOpenAIChatBody builds a minimal Chat Completions request body
// with a `messages` array and a `tools` array. Used as the fixture base
// for the Chat-Completions parity tests below.
func helperOpenAIChatBody(t *testing.T, messages []map[string]any, tools []map[string]any) []byte {
	t.Helper()
	envelope := map[string]any{
		"model":            "gpt-4o-mini",
		"messages":         messages,
		"prompt_cache_key": "test-session-id",
	}
	if tools != nil {
		envelope["tools"] = tools
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// helperOpenAIChatToolCall builds a single tool_calls[*] entry for an
// assistant message. Args is the JSON-encoded arguments string.
func helperOpenAIChatToolCall(id, fnName, args string) map[string]any {
	return map[string]any{
		"id":   id,
		"type": "function",
		"function": map[string]any{
			"name":      fnName,
			"arguments": args,
		},
	}
}

// TestResolveOpenAIChatToolCalls_PopulatesNameAndFilename pins the
// per-call resolver: tool messages get their producing function's
// name + file_path back-filled via tool_call_id lookup against earlier
// assistant.tool_calls entries.
func TestResolveOpenAIChatToolCalls_PopulatesNameAndFilename(t *testing.T) {
	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "read main.go and search past outputs"},
		{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{
				helperOpenAIChatToolCall("call_read_main", "read_file", `{"file_path":"main.go"}`),
				helperOpenAIChatToolCall("call_search", "mcp__observer__search_past_outputs", `{"query":"foo"}`),
			},
		},
		{"role": "tool", "tool_call_id": "call_read_main", "content": "package main\n"},
		{"role": "tool", "tool_call_id": "call_search", "content": `{"hits":[]}`},
	}, nil)

	_, ex, ok := openaiExtract(body)
	if !ok {
		t.Fatalf("openaiExtract: not ok")
	}
	if len(ex) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(ex))
	}
	var readOut, searchOut *openaiExtractedMessage
	for i := range ex {
		if !ex[i].isToolResult {
			continue
		}
		switch ex[i].toolCallID {
		case "call_read_main":
			readOut = &ex[i]
		case "call_search":
			searchOut = &ex[i]
		}
	}
	if readOut == nil || searchOut == nil {
		t.Fatalf("expected both tool messages resolved")
	}
	if readOut.toolName != "read_file" {
		t.Errorf("readOut.toolName = %q, want read_file", readOut.toolName)
	}
	if readOut.filename != "main.go" {
		t.Errorf("readOut.filename = %q, want main.go", readOut.filename)
	}
	if searchOut.toolName != "mcp__observer__search_past_outputs" {
		t.Errorf("searchOut.toolName = %q, want mcp__observer__search_past_outputs", searchOut.toolName)
	}
}

// TestCompressOpenAIChatToolResults_PerTypeFires verifies a JSON-shaped
// tool message body gets compressed via the JSON per-type compressor.
// Mirrors the Responses-API test on the same shape.
func TestCompressOpenAIChatToolResults_PerTypeFires(t *testing.T) {
	bigJSON := `{"items":[`
	for i := 0; i < 50; i++ {
		if i > 0 {
			bigJSON += ","
		}
		bigJSON += `{"id":` + itoa(i) + `,"name":"item-` + itoa(i) + `"}`
	}
	bigJSON += `]}`

	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "dump the items"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_dump", "dump_items", `{"path":"dump.json"}`)},
		},
		{"role": "tool", "tool_call_id": "call_dump", "content": bigJSON},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("expected pipeline to run, got skipped")
	}
	jsonEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "json" {
			jsonEvent = true
			break
		}
	}
	if !jsonEvent {
		t.Errorf("expected json mechanism event, got events=%+v", got.Events)
	}
	if got.CompressedBytes >= got.OriginalBytes {
		t.Errorf("expected body to shrink, original=%d compressed=%d", got.OriginalBytes, got.CompressedBytes)
	}
}

// TestCompressOpenAIChatToolResults_SkipsMCP pins the critical
// correctness invariant: MCP tool messages MUST NEVER be compressed
// via the per-type pipeline. Mirrors the Anthropic and Responses-API
// tests of the same shape.
func TestCompressOpenAIChatToolResults_SkipsMCP(t *testing.T) {
	bigSearchResp := `{"query":"app.set","count":10,"hits":[`
	for i := 0; i < 10; i++ {
		if i > 0 {
			bigSearchResp += ","
		}
		bigSearchResp += `{"action_id":` + itoa(1000+i) +
			`,"tool_name":"Bash","target":"grep app.set","excerpt":"app.set('foo','bar')","rank":-` + itoa(i) + `.5}`
	}
	bigSearchResp += `]}`

	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "search past outputs"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_search", "mcp__observer__search_past_outputs", `{"query":"app.set"}`)},
		},
		{"role": "tool", "tool_call_id": "call_search", "content": bigSearchResp},
	}, []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "mcp__observer__search_past_outputs",
				"description": "Search past tool outputs",
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	for _, ev := range got.Events {
		switch ev.Mechanism {
		case "json", "code", "logs", "stash":
			t.Errorf("MCP tool message was compressed via %q (would corrupt query data); event=%+v", ev.Mechanism, ev)
		}
	}
	bodyStr := string(got.Body)
	if !strings.Contains(bodyStr, "1000") || !strings.Contains(bodyStr, "1009") {
		t.Errorf("action_id values stripped from MCP response; output=%s", bodyStr)
	}
	if strings.Contains(bodyStr, `<number>`) || strings.Contains(bodyStr, `<string>`) {
		t.Errorf("JSON compression scalars leaked into MCP response; output=%s", bodyStr)
	}
}

// TestStashOpenAIChatLargeBodies_ReplacesWithMarker verifies a tool
// message body exceeding the threshold gets replaced with the canonical
// stash marker.
func TestStashOpenAIChatLargeBodies_ReplacesWithMarker(t *testing.T) {
	bigBody := strings.Repeat("not very compressible content with random stuff ", 500)

	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "dump"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_dump", "exec", `{"cmd":"head -c 99999 /dev/random"}`)},
		},
		{"role": "tool", "tool_call_id": "call_dump", "content": bigBody},
	}, nil)

	st, err := stash.New(stash.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New()).WithStash(st, 1024)

	got := p.Run("openai", body)
	stashEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "stash" {
			stashEvent = true
			break
		}
	}
	if !stashEvent {
		t.Errorf("expected stash event, got events=%+v", got.Events)
	}
	if !strings.Contains(string(got.Body), "mcp__observer__retrieve_stashed") {
		t.Errorf("expected stash marker with retrieve_stashed directive in body; output=%s", string(got.Body))
	}
}

// TestStashOpenAIChatLargeBodies_SkipsMCP verifies the stash pass also
// honors the MCP-skip predicate. Re-stashing an MCP retrieve_stashed
// output would loop the model on a hash that matches its own retrieved
// bytes.
func TestStashOpenAIChatLargeBodies_SkipsMCP(t *testing.T) {
	bigBody := strings.Repeat("retrieved content from earlier stash ", 500)

	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "retrieve"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_retrieve", "mcp__observer__retrieve_stashed", `{"sha":"abc123"}`)},
		},
		{"role": "tool", "tool_call_id": "call_retrieve", "content": bigBody},
	}, nil)

	st, err := stash.New(stash.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("stash.New: %v", err)
	}
	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New()).WithStash(st, 1024)

	got := p.Run("openai", body)
	for _, ev := range got.Events {
		if ev.Mechanism == "stash" {
			t.Errorf("MCP retrieve_stashed output was re-stashed; would loop the model. event=%+v", ev)
		}
	}
}

// TestCompressOpenAIChatToolDefinitions_TrimsAndStripsExamples
// verifies the two transforms apply to function-typed tools, descending
// through the `function` wrapper. Forbidden list — parameters /
// properties / required / enum / numeric bounds — must survive.
func TestCompressOpenAIChatToolDefinitions_TrimsAndStripsExamples(t *testing.T) {
	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "hi"},
	}, []map[string]any{
		{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "Para 1.\n\nPara 2.\n\nPara 3 should be trimmed.\n\nPara 4 also trimmed.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "File path",
							"examples":    []string{"/foo", "/bar"},
							"minLength":   1,
						},
					},
					"required": []string{"path"},
				},
			},
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		// V7-21: tool-defs trim is opt-in via the "tools" sentinel.
		CompressTypes: []string{"json", "logs", "code", "tools"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	toolsEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "tools" {
			toolsEvent = true
			break
		}
	}
	if !toolsEvent {
		t.Errorf("expected tools event, got events=%+v", got.Events)
	}
	bodyStr := string(got.Body)
	if strings.Contains(bodyStr, "Para 3 should be trimmed") {
		t.Errorf("description tail not trimmed; output contains Para 3")
	}
	if strings.Contains(bodyStr, `"examples"`) {
		t.Errorf("examples not stripped from parameters; output=%s", bodyStr)
	}
	for _, want := range []string{`"properties"`, `"required"`, `"type"`, `"minLength"`, `"path"`} {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("forbidden-list field %q stripped from parameters; output=%s", want, bodyStr)
		}
	}
}

// TestCompressOpenAIChatToolDefinitions_SkipsNonFunctionTypes pins the
// conservative behaviour: web_search, mcp, and other non-function tool
// types pass through unchanged so future tool types don't get
// accidentally mutated.
func TestCompressOpenAIChatToolDefinitions_SkipsNonFunctionTypes(t *testing.T) {
	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "hi"},
	}, []map[string]any{
		{
			"type":        "web_search",
			"description": "Para 1.\n\nPara 2.\n\nPara 3 should not be touched.",
		},
	})

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	for _, ev := range got.Events {
		if ev.Mechanism == "tools" {
			t.Errorf("non-function tool was mutated; event=%+v", ev)
		}
	}
	if !strings.Contains(string(got.Body), "Para 3 should not be touched") {
		t.Errorf("non-function tool description was trimmed; output=%s", string(got.Body))
	}
}

// TestSubstituteOpenAIChatRedundantReads_FiresOnDuplicate pins C16:
// when the same file is read twice in one Chat Completions request body,
// the second tool message gets replaced with the read-cache marker.
func TestSubstituteOpenAIChatRedundantReads_FiresOnDuplicate(t *testing.T) {
	bigFile := strings.Repeat("package main\n\nfunc main() {}\n\n", 100)
	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "read it twice"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_a", "read_file", `{"file_path":"foo.go"}`)},
		},
		{"role": "tool", "tool_call_id": "call_a", "content": bigFile},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_b", "read_file", `{"file_path":"foo.go"}`)},
		},
		{"role": "tool", "tool_call_id": "call_b", "content": bigFile},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.RunInSession("openai", body, "test-session-id")
	readCacheEvent := false
	for _, ev := range got.Events {
		if ev.Mechanism == "read_cache" {
			readCacheEvent = true
			break
		}
	}
	if !readCacheEvent {
		t.Errorf("expected read_cache event, got %+v", got.Events)
	}
	if !strings.Contains(string(got.Body), "[file foo.go unchanged since earlier in this turn") {
		t.Errorf("expected read_cache marker in body; output=%s", string(got.Body))
	}
}

// TestSubstituteOpenAIChatRedundantReads_NoSession verifies the pass
// no-ops when sessionID is empty (parity with Anthropic + Responses-API).
func TestSubstituteOpenAIChatRedundantReads_NoSession(t *testing.T) {
	bigFile := strings.Repeat("package main\n\n", 100)
	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "read"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_a", "read_file", `{"file_path":"foo.go"}`)},
		},
		{"role": "tool", "tool_call_id": "call_a", "content": bigFile},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_b", "read_file", `{"file_path":"foo.go"}`)},
		},
		{"role": "tool", "tool_call_id": "call_b", "content": bigFile},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	for _, ev := range got.Events {
		if ev.Mechanism == "read_cache" {
			t.Errorf("read_cache must not fire without sessionID; got event=%+v", ev)
		}
	}
}

// TestSubstituteOpenAIChatRedundantReads_SkipsMCP pins the MCP-skip
// invariant — MCP tool messages must NEVER be replaced with a
// read-cache marker, even when their (filename, hash) tuple is
// duplicated.
func TestSubstituteOpenAIChatRedundantReads_SkipsMCP(t *testing.T) {
	bigOutput := strings.Repeat(`{"hits":[{"action_id":1}]}`, 50)
	body := helperOpenAIChatBody(t, []map[string]any{
		{"role": "user", "content": "search twice"},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_a", "mcp__observer__search_past_outputs", `{"query":"x","file_path":"/synthetic/hits"}`)},
		},
		{"role": "tool", "tool_call_id": "call_a", "content": bigOutput},
		{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": []any{helperOpenAIChatToolCall("call_b", "mcp__observer__search_past_outputs", `{"query":"x","file_path":"/synthetic/hits"}`)},
		},
		{"role": "tool", "tool_call_id": "call_b", "content": bigOutput},
	}, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.95,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.RunInSession("openai", body, "test-session-id")
	for _, ev := range got.Events {
		if ev.Mechanism == "read_cache" {
			t.Errorf("MCP output got read_cache substituted; would corrupt query data. event=%+v", ev)
		}
	}
}

// helperAdditionalToolsItem builds a Responses-API `additional_tools`
// input item modeled on GPT-5.6 Codex's on-wire shape: tool definitions
// arrive as the FIRST `input` item (role:"developer") carrying a `tools`
// array, NOT a top-level `tools` array. The array holds a custom `exec`
// tool plus filler definitions so the item is a large, budget-dominating
// share of the request (forcing the enforcer to drop other items while
// this one must survive).
func helperAdditionalToolsItem() map[string]any {
	toolsArr := []any{
		map[string]any{
			"type":        "custom",
			"name":        "exec",
			"description": strings.Repeat("Run a shell command in the workspace and return combined stdout and stderr. ", 40),
		},
	}
	for i := 0; i < 10; i++ {
		toolsArr = append(toolsArr, map[string]any{
			"type":        "function",
			"name":        "helper_" + itoa(i),
			"description": strings.Repeat("Auxiliary developer tool number "+itoa(i)+" description filler. ", 30),
		})
	}
	return map[string]any{
		"type":  "additional_tools",
		"role":  "developer",
		"tools": toolsArr,
	}
}

// TestOpenAIResponsesExtract_ClassifiesItemTypes pins the tool-result
// classifier at the boundary: an `additional_tools` DEFINITION item is
// NOT a tool result and keeps its real `developer` role; genuine *_output
// results and role:"tool" items still classify as results; call and
// definition items do not. Regression guard for the GPT-5.6 Codex
// tool-loss bug (the over-broad `Contains(itemType,"tool")` clause used
// to misclassify `additional_tools` as a result).
func TestOpenAIResponsesExtract_ClassifiesItemTypes(t *testing.T) {
	body := helperOpenAIResponsesBody(t, []map[string]any{
		{"type": "additional_tools", "role": "developer", "tools": []any{
			map[string]any{"type": "custom", "name": "exec"},
		}},
		{"type": "function_call", "call_id": "c1", "name": "exec", "arguments": "{}"},
		{"type": "function_call_output", "call_id": "c1", "output": "ok"},
		{"type": "custom_tool_call_output", "call_id": "c2", "output": "ok"},
		{"type": "message", "role": "user", "content": "hi"},
		{"type": "custom_tool_call", "call_id": "c2", "name": "exec", "input": "ls -la"},
		{"type": "local_shell_call", "call_id": "c3", "action": map[string]any{"type": "exec", "command": []any{"ls"}}},
	}, nil)

	_, ex, ok := openaiResponsesExtract(body)
	if !ok || len(ex) != 7 {
		t.Fatalf("openaiResponsesExtract: ok=%v len=%d", ok, len(ex))
	}
	cases := []struct {
		idx         int
		wantType    string
		wantResult  bool
		wantRawRole string
	}{
		{0, "additional_tools", false, "developer"},
		{1, "function_call", false, ""},
		{2, "function_call_output", true, ""},
		{3, "custom_tool_call_output", true, ""},
		{4, "message", false, "user"},
		{5, "custom_tool_call", false, ""},
		{6, "local_shell_call", false, ""},
	}
	for _, tc := range cases {
		em := ex[tc.idx]
		if em.itemType != tc.wantType {
			t.Errorf("idx %d: itemType=%q want %q", tc.idx, em.itemType, tc.wantType)
		}
		if em.isToolResult != tc.wantResult {
			t.Errorf("idx %d (%s): isToolResult=%v want %v", tc.idx, tc.wantType, em.isToolResult, tc.wantResult)
		}
		if em.role != tc.wantRawRole {
			t.Errorf("idx %d (%s): role=%q want %q", tc.idx, tc.wantType, em.role, tc.wantRawRole)
		}
	}

	// The definition item must be pinned and keep a non-tool role when
	// projected to a conversation.Message so scoring always preserves it.
	msgs := openaiResponsesToConversationMessages(ex, nil)
	if !msgs[0].Pinned {
		t.Errorf("additional_tools message: Pinned=false, want true")
	}
	if msgs[0].Role == RoleTool {
		t.Errorf("additional_tools message: Role=RoleTool, want the preserved developer role")
	}
	// Tool-call PRODUCERS (function_call / custom_tool_call /
	// local_shell_call) must be pinned AND contribute their call_id to
	// ToolUseIDs so tool-pair-integrity protects the matching *_output.
	producers := []struct {
		idx    int
		wantID string
	}{
		{1, "c1"}, // function_call
		{5, "c2"}, // custom_tool_call
		{6, "c3"}, // local_shell_call
	}
	for _, pr := range producers {
		if !msgs[pr.idx].Pinned {
			t.Errorf("producer idx %d (%s): Pinned=false, want true", pr.idx, ex[pr.idx].itemType)
		}
		if len(msgs[pr.idx].ToolUseIDs) != 1 || msgs[pr.idx].ToolUseIDs[0] != pr.wantID {
			t.Errorf("producer idx %d (%s): ToolUseIDs=%v, want [%s]",
				pr.idx, ex[pr.idx].itemType, msgs[pr.idx].ToolUseIDs, pr.wantID)
		}
	}
}

// TestRunOpenAI_PreservesAdditionalToolsUnderCompression is the
// end-to-end regression test for the GPT-5.6 Codex tool-loss bug. With a
// budget-dominating `additional_tools` definition at input[0] followed by
// several compressible, droppable tool outputs, the enforcer is forced to
// BOTH per-type compress AND drop — yet the definition item must survive
// verbatim at its original position. Before the fix it was misclassified
// as a tool result, remapped to RoleTool, and dropped, leaving the model
// with no tools.
func TestRunOpenAI_PreservesAdditionalToolsUnderCompression(t *testing.T) {
	items := []map[string]any{helperAdditionalToolsItem()}
	// Compressible, droppable tool outputs: orphan call_ids (no producing
	// function_call) → not part of a live tool pair → eligible for drops.
	bigJSON := func(seed int) string {
		s := `{"items":[`
		for i := 0; i < 60; i++ {
			if i > 0 {
				s += ","
			}
			s += `{"id":` + itoa(seed*100+i) + `,"name":"item-` + itoa(i) + `","tag":"x"}`
		}
		return s + `]}`
	}
	for k := 0; k < 6; k++ {
		items = append(items, map[string]any{
			"type":    "function_call_output",
			"call_id": "orphan_" + itoa(k),
			"output":  bigJSON(k),
		})
	}
	body := helperOpenAIResponsesBody(t, items, nil)

	// Capture the original input[0] for the verbatim comparison.
	var origEnv map[string]json.RawMessage
	if err := json.Unmarshal(body, &origEnv); err != nil {
		t.Fatalf("unmarshal original body: %v", err)
	}
	var origInput []json.RawMessage
	if err := json.Unmarshal(origEnv["input"], &origInput); err != nil {
		t.Fatalf("unmarshal original input: %v", err)
	}
	var wantItem any
	if err := json.Unmarshal(origInput[0], &wantItem); err != nil {
		t.Fatalf("unmarshal original input[0]: %v", err)
	}

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.3,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("expected pipeline to run, got skipped")
	}

	// Guard against a vacuous pass: per-type compression must have fired
	// (json) AND the enforcer must have dropped at least one message.
	jsonFired := false
	for _, ev := range got.Events {
		if ev.Mechanism == "json" {
			jsonFired = true
			break
		}
	}
	if !jsonFired {
		t.Errorf("expected json per-type compression to fire; events=%+v", got.Events)
	}
	if got.DroppedCount == 0 {
		t.Fatalf("expected the budget enforcer to drop at least one message; dropped=%d compressed=%d events=%+v",
			got.DroppedCount, got.CompressedCount, got.Events)
	}

	// The additional_tools item must still be present, verbatim, at input[0].
	var gotEnv map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &gotEnv); err != nil {
		t.Fatalf("unmarshal compressed body: %v", err)
	}
	var gotInput []json.RawMessage
	if err := json.Unmarshal(gotEnv["input"], &gotInput); err != nil {
		t.Fatalf("unmarshal compressed input: %v", err)
	}
	if len(gotInput) == 0 {
		t.Fatalf("compressed input is empty")
	}
	var gotItem any
	if err := json.Unmarshal(gotInput[0], &gotItem); err != nil {
		t.Fatalf("unmarshal compressed input[0]: %v", err)
	}
	gotObj, ok := gotItem.(map[string]any)
	if !ok || gotObj["type"] != "additional_tools" {
		t.Fatalf("input[0] is no longer the additional_tools definition: %s", gotInput[0])
	}
	if !reflect.DeepEqual(gotItem, wantItem) {
		t.Errorf("additional_tools definition was mutated by compression.\n got: %s\nwant: %s", gotInput[0], origInput[0])
	}
	// Byte-for-byte, too (prefix-cache safety): the raw bytes forward unchanged.
	if !bytes.Equal(gotInput[0], origInput[0]) {
		t.Errorf("additional_tools definition not byte-identical.\n got: %s\nwant: %s", gotInput[0], origInput[0])
	}
}

// TestRunOpenAI_PreservesCustomToolCallPairUnderDrops pins tool-pair
// integrity for GPT-5.6 Codex's `exec` tool shape: a `custom_tool_call`
// ↔ `custom_tool_call_output` pair sitting in the droppable middle of
// the conversation must survive an aggressive drop pass PAIRED — the
// producer via Pinned, the output via the live-pair rule wired through
// ToolUseIDs/ReferencedIDs. Before the fix the producer was never
// pinned and never contributed ToolUseIDs, so both halves were droppable
// and the enforcer could orphan (or fully evict) the pair.
func TestRunOpenAI_PreservesCustomToolCallPairUnderDrops(t *testing.T) {
	bigJSON := func(seed int) string {
		s := `{"items":[`
		for i := 0; i < 60; i++ {
			if i > 0 {
				s += ","
			}
			s += `{"id":` + itoa(seed*100+i) + `,"name":"filler-` + itoa(i) + `","tag":"x"}`
		}
		return s + `]}`
	}
	filler := func(k int) map[string]any {
		return map[string]any{
			"type":    "function_call_output",
			"call_id": "orphan_" + itoa(k),
			"output":  bigJSON(k),
		}
	}
	// Pair placed after two fillers, followed by four more — squarely in
	// the droppable zone (not first, not last, PreserveLastN=0).
	items := []map[string]any{
		{"type": "message", "role": "user", "content": "please inspect the repository"},
		filler(0),
		filler(1),
		{"type": "custom_tool_call", "call_id": "call_exec_1", "name": "exec", "input": "ls -la"},
		{"type": "custom_tool_call_output", "call_id": "call_exec_1", "output": "total 42\nsrc\nREADME.md"},
		filler(2),
		filler(3),
		filler(4),
		filler(5),
	}
	body := helperOpenAIResponsesBody(t, items, nil)

	p := NewPipeline(PipelineConfig{
		Enabled:       true,
		TargetRatio:   0.2,
		PreserveLastN: 0,
		CompressTypes: []string{"json", "logs", "code"},
	}, DefaultRegistry(), scrub.New())

	got := p.Run("openai", body)
	if got.Skipped {
		t.Fatalf("expected pipeline to run, got skipped")
	}
	// Vacuous-pass guard: the enforcer must actually have dropped
	// filler items (per-type compression alone can't hit 0.2).
	if got.DroppedCount == 0 {
		t.Fatalf("expected drops to fire; dropped=%d events=%+v", got.DroppedCount, got.Events)
	}

	var gotEnv map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &gotEnv); err != nil {
		t.Fatalf("unmarshal compressed body: %v", err)
	}
	var gotInput []map[string]json.RawMessage
	if err := json.Unmarshal(gotEnv["input"], &gotInput); err != nil {
		t.Fatalf("unmarshal compressed input: %v", err)
	}
	var haveCall, haveOutput, haveMarker bool
	fillerCount := 0
	for _, item := range gotInput {
		itemType := rawString(item["type"])
		callID := rawString(item["call_id"])
		switch {
		case itemType == "custom_tool_call" && callID == "call_exec_1":
			haveCall = true
		case itemType == "custom_tool_call_output" && callID == "call_exec_1":
			haveOutput = true
		case itemType == "function_call_output":
			fillerCount++
		case itemType == "message":
			if strings.Contains(string(item["content"]), "messages compressed") {
				haveMarker = true
			}
		}
	}
	if !haveCall {
		t.Errorf("custom_tool_call producer was dropped; input=%s", gotEnv["input"])
	}
	if !haveOutput {
		t.Errorf("custom_tool_call_output was dropped (orphaning the pair); input=%s", gotEnv["input"])
	}
	if haveCall != haveOutput {
		t.Errorf("pair survived UNPAIRED: call=%v output=%v", haveCall, haveOutput)
	}
	// Filler items must have actually been evicted (collapsed into a
	// marker) so the pair assertions can't pass via a no-op drop pass.
	if fillerCount >= 6 {
		t.Errorf("expected filler outputs to be dropped, all %d still present", fillerCount)
	}
	if !haveMarker {
		t.Errorf("expected a compression marker replacing dropped fillers; input=%s", gotEnv["input"])
	}
}

// TestRunOpenAI_DisableDrops pins the 2026-07-16 fix: the OpenAI/codex
// lane must honour [compression.conversation].disable_drops
// (PipelineConfig.DisableDrops), turning off the budget enforcer's lossy
// Pass-2 eviction so the lane can never degenerate into drop-only
// compression when the per-type compressors no-op on codex tool outputs.
// Table covers: (a) back-compat drops still fire when the knob is off,
// (b) knob ON + incompressible body → valid, byte-identical passthrough,
// zero drops, and (c) knob ON + compressible JSON → per-type compression
// still fires, still zero drops.
func TestRunOpenAI_DisableDrops(t *testing.T) {
	// Incompressible outputs: single-line repeated bytes never dedup or
	// shrink, so per-type compression is a functional no-op — the exact
	// gpt-5.6-sol shape where only the drop pass could hit target. Orphan
	// call_ids (no producing function_call) keep the outputs droppable —
	// a paired producer would PIN them out of drop eligibility.
	incompressibleItems := func() []map[string]any {
		var items []map[string]any
		for k := 0; k < 6; k++ {
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": "orphan_" + itoa(k),
				"output":  strings.Repeat("A", 800),
			})
		}
		return items
	}

	// Compressible JSON output: proves per-type compression is untouched
	// by the no-drop knob.
	bigJSON := `{"records":[`
	for i := 0; i < 80; i++ {
		if i > 0 {
			bigJSON += ","
		}
		bigJSON += `{"id":` + itoa(i) + `,"payload":"` + strings.Repeat("x", 30) + `"}`
	}
	bigJSON += `]}`
	compressibleItems := []map[string]any{
		{"type": "function_call", "call_id": "c0", "name": "exec_command", "arguments": `{"cmd":"cat dump.json"}`},
		{"type": "function_call_output", "call_id": "c0", "output": bigJSON},
		{"type": "function_call", "call_id": "c1", "name": "exec_command", "arguments": `{"cmd":"noise"}`},
		{"type": "function_call_output", "call_id": "c1", "output": strings.Repeat("A", 800)},
	}

	tests := []struct {
		name          string
		items         []map[string]any
		disableDrops  bool
		compressTypes []string
		wantDrops     bool
		wantJSONEvent bool
		wantUnchanged bool
	}{
		{
			name:          "back-compat: drops fire when knob off",
			items:         incompressibleItems(),
			disableDrops:  false,
			compressTypes: []string{"logs"},
			wantDrops:     true,
		},
		{
			name:          "disable_drops: incompressible body passes through unchanged",
			items:         incompressibleItems(),
			disableDrops:  true,
			compressTypes: []string{"logs"},
			wantDrops:     false,
			wantUnchanged: true,
		},
		{
			name:          "disable_drops: per-type compression still fires",
			items:         compressibleItems,
			disableDrops:  true,
			compressTypes: []string{"json"},
			wantDrops:     false,
			wantJSONEvent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := helperOpenAIResponsesBody(t, tc.items, nil)
			p := NewPipeline(PipelineConfig{
				Enabled:       true,
				TargetRatio:   0.3,
				PreserveLastN: 0,
				CompressTypes: tc.compressTypes,
				DisableDrops:  tc.disableDrops,
			}, DefaultRegistry(), scrub.New())

			got := p.Run("openai", body)
			if got.Skipped {
				t.Fatalf("expected pipeline to run, got skipped")
			}
			// The forwarded body must always be valid JSON — a corrupt
			// upstream-bound body silently breaks the live session.
			if !json.Valid(got.Body) {
				t.Fatalf("forwarded body is not valid JSON: %s", trimPreview(string(got.Body)))
			}
			if tc.wantDrops && got.DroppedCount == 0 {
				t.Errorf("expected drops (knob off), got none; events=%+v", got.Events)
			}
			if !tc.wantDrops && got.DroppedCount != 0 {
				t.Errorf("expected zero drops (disable_drops), got %d; events=%+v", got.DroppedCount, got.Events)
			}
			if !tc.wantDrops {
				for _, ev := range got.Events {
					if ev.Mechanism == "drop" {
						t.Errorf("disable_drops recorded a drop event: %+v", ev)
					}
				}
			}
			if tc.wantUnchanged {
				// Per-type no-op + no drops → fast-path early-return
				// forwards the original body byte-identically (prefix-cache
				// safety). No fabricated savings.
				if !bytes.Equal(got.Body, body) {
					t.Errorf("expected byte-identical passthrough under disable_drops with no-op per-type.\n orig: %s\n  got: %s",
						trimPreview(string(body)), trimPreview(string(got.Body)))
				}
				if got.CompressedCount != 0 || len(got.Events) != 0 {
					t.Errorf("expected zero compression events on no-op body, got compressed=%d events=%+v", got.CompressedCount, got.Events)
				}
			}
			if tc.wantJSONEvent {
				jsonFired := false
				for _, ev := range got.Events {
					if ev.Mechanism == "json" {
						jsonFired = true
						break
					}
				}
				if !jsonFired {
					t.Errorf("expected json per-type compression to fire under disable_drops; events=%+v", got.Events)
				}
			}
		})
	}
}
