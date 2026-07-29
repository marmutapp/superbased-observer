package droid

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestMapToolName(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"Read", models.ActionReadFile},
		{"Create", models.ActionWriteFile},
		{"Edit", models.ActionEditFile},
		{"ApplyPatch", models.ActionEditFile},
		{"Execute", models.ActionRunCommand},
		{"Grep", models.ActionSearchText},
		{"Glob", models.ActionSearchFiles},
		{"LS", models.ActionSearchFiles},
		{"WebSearch", models.ActionWebSearch},
		{"FetchUrl", models.ActionWebFetch},
		{"TodoWrite", models.ActionTodoUpdate},
		{"AskUser", models.ActionAskUser},
		{"ExitSpecMode", models.ActionPermissionMode},
		{"TaskOutput", models.ActionSpawnSubagent},
		{"TaskStop", models.ActionSpawnSubagent},
		{"CronCreate", models.ActionConfigChange},
		{"DeleteAutomation", models.ActionConfigChange},
		{"GenerateDroid", models.ActionWriteFile},
		// Case-insensitive fallback.
		{"read", models.ActionReadFile},
		{"todowrite", models.ActionTodoUpdate},
		// MCP namespacing.
		{"mcp__linear__list_issues", models.ActionMCPCall},
		{"mcpSomething", models.ActionMCPCall},
		// Unknown names keep the raw name and land in the catch-all.
		{"SomeFutureTool", models.ActionUnknown},
		{"", models.ActionUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := mapToolName(tc.raw); got != tc.want {
				t.Errorf("mapToolName(%q)=%q want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestTargetFromInput(t *testing.T) {
	cases := []struct {
		name  string
		input string
		fb    string
		want  string
	}{
		{"file_path", `{"file_path":"/a/b.md","offset":0}`, "Read", "/a/b.md"},
		{"directory_path", `{"directory_path":"C:\\p","ignorePatterns":[".git/**"]}`, "LS", `C:\p`},
		{"command", `{"command":"ls -la"}`, "Execute", "ls -la"},
		{"todos string", `{"todos":"1. [pending] x"}`, "TodoWrite", "1. [pending] x"},
		{"no known key", `{"weird":1}`, "Mystery", "Mystery"},
		{"malformed json", `{`, "Mystery", "Mystery"},
		{"empty", ``, "Mystery", "Mystery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := targetFromInput(json.RawMessage(tc.input), tc.fb)
			if got != tc.want {
				t.Errorf("targetFromInput(%s)=%q want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDecodeBlocks(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		count int
		first string
	}{
		{"array", `[{"type":"text","text":"a"},{"type":"tool_use","id":"x"}]`, 2, blockText},
		{"bare string shorthand", `"hello"`, 1, blockText},
		{"empty string", `""`, 0, ""},
		{"null", `null`, 0, ""},
		{"object", `{"type":"text"}`, 0, ""},
		{"absent", ``, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeBlocks(json.RawMessage(tc.raw))
			if len(got) != tc.count {
				t.Fatalf("decodeBlocks(%s) len=%d want %d", tc.raw, len(got), tc.count)
			}
			if tc.count > 0 && got[0].Type != tc.first {
				t.Errorf("first block type=%q want %q", got[0].Type, tc.first)
			}
		})
	}
}

func TestResultText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare string", `"smoke\n"`, "smoke\n"},
		{"block array", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{"unknown shape falls back to raw", `{"x":1}`, `{"x":1}`},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resultText(json.RawMessage(tc.raw)); got != tc.want {
				t.Errorf("resultText(%s)=%q want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := []struct {
		raw  string
		zero bool
		want string
	}{
		{"2026-07-28T18:03:10.939Z", false, "2026-07-28T18:03:10.939Z"},
		{"2026-07-28T18:03:10Z", false, "2026-07-28T18:03:10Z"},
		{"", true, ""},
		{"not a time", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := parseTimestamp(tc.raw)
			if got.IsZero() != tc.zero {
				t.Fatalf("parseTimestamp(%q).IsZero()=%v want %v", tc.raw, got.IsZero(), tc.zero)
			}
			if !tc.zero && got.Format(time.RFC3339Nano) != tc.want {
				t.Errorf("parseTimestamp(%q)=%s want %s", tc.raw, got.Format(time.RFC3339Nano), tc.want)
			}
		})
	}
}

func TestContentHashStableAndShort(t *testing.T) {
	a := contentHash("payload")
	if a != contentHash("payload") {
		t.Error("contentHash is not stable")
	}
	if a == contentHash("payload2") {
		t.Error("contentHash collided on distinct inputs")
	}
	if len(a) != 16 {
		t.Errorf("contentHash length=%d want 16", len(a))
	}
}
