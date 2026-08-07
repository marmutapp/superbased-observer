package muse

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestMapToolName(t *testing.T) {
	cases := []struct {
		name       string
		native     string
		want       string
		recognised bool
	}{
		// grounded in the live capture
		{"bash", "bash", models.ActionRunCommand, true},
		{"read_file", "read_file", models.ActionReadFile, true},
		{"write_file", "write_file", models.ActionWriteFile, true},
		{"edit_file", "edit_file", models.ActionEditFile, true},
		{"submit_reminder_decision", "submit_reminder_decision", models.ActionHarnessCall, true},
		// grounded in the shipped binary's strings
		{"web_search", "web_search", models.ActionWebSearch, true},
		{"web_fetch", "web_fetch", models.ActionWebFetch, true},
		{"read_skill", "read_skill", models.ActionSkillInvoke, true},
		{"search", "search", models.ActionSearchText, true},
		// spelling normalization
		{"camelCase", "readFile", models.ActionReadFile, true},
		{"kebab-case", "web-search", models.ActionWebSearch, true},
		{"padded + upper", "  BASH ", models.ActionRunCommand, true},
		// MCP routing
		{"mcp prefix", "mcp_github_list_issues", models.ActionMCPCall, true},
		{"double underscore", "github__list_issues", models.ActionMCPCall, true},
		// honest unknown
		{"unmapped", "teleport_to_mars", models.ActionUnknown, false},
		{"empty", "", models.ActionUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := mapToolName(tc.native)
			if got != tc.want || ok != tc.recognised {
				t.Errorf("mapToolName(%q) = (%q, %v), want (%q, %v)", tc.native, got, ok, tc.want, tc.recognised)
			}
		})
	}
}

func TestTokenBundle(t *testing.T) {
	cases := []struct {
		name string
		in   *rawUsage
		want tokenParts
	}{
		{"nil", nil, tokenParts{}},
		{
			// The Phase-0 capture's second model_completed row, verbatim.
			name: "live cached turn",
			in: &rawUsage{
				InputTokens: 15924, OutputTokens: 355,
				CacheReadTokens: 15665, CachedTokens: 15665, ReasoningTokens: 111,
			},
			want: tokenParts{inputNet: 259, outputNet: 244, cacheRead: 15665, reasoning: 111},
		},
		{
			name: "uncached first turn",
			in:   &rawUsage{InputTokens: 15719, OutputTokens: 101, ReasoningTokens: 84},
			want: tokenParts{inputNet: 15719, outputNet: 17, reasoning: 84},
		},
		{
			// cached_tokens is a duplicate of cache_read_tokens in every
			// observed row; it is a FALLBACK, never additive.
			name: "cached_tokens fallback when cache_read absent",
			in:   &rawUsage{InputTokens: 100, OutputTokens: 10, CachedTokens: 40},
			want: tokenParts{inputNet: 60, outputNet: 10, cacheRead: 40},
		},
		{
			name: "cache write carried through",
			in:   &rawUsage{InputTokens: 50, OutputTokens: 5, CacheWriteTokens: 30},
			want: tokenParts{inputNet: 50, outputNet: 5, cacheWrit: 30},
		},
		{
			// Defensive clamps: a provider that ever reports NET fields
			// must not produce negative counts.
			name: "clamps below zero",
			in:   &rawUsage{InputTokens: 10, CacheReadTokens: 99, OutputTokens: 3, ReasoningTokens: 9},
			want: tokenParts{inputNet: 0, outputNet: 0, cacheRead: 99, reasoning: 9},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenBundle(tc.in); got != tc.want {
				t.Errorf("tokenBundle() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUsageIsZero(t *testing.T) {
	if !(*rawUsage)(nil).isZero() {
		t.Error("nil usage should be zero")
	}
	if !(&rawUsage{}).isZero() {
		t.Error("empty usage should be zero")
	}
	if (&rawUsage{ReasoningTokens: 1}).isZero() {
		t.Error("a usage envelope with any non-zero field is not zero")
	}
}

func TestParseTimestamp(t *testing.T) {
	// The live unit: 16-digit microseconds.
	const liveMicros = int64(1785962540739784)
	cases := []struct {
		name string
		in   int64
		want time.Time
	}{
		{"zero", 0, time.Time{}},
		{"negative", -5, time.Time{}},
		{"micros (the live unit)", liveMicros, time.UnixMicro(liveMicros).UTC()},
		{"millis", liveMicros / 1000, time.UnixMilli(liveMicros / 1000).UTC()},
		{"seconds", liveMicros / 1e6, time.Unix(liveMicros/1e6, 0).UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTimestamp(tc.in)
			if !got.Equal(tc.want) {
				t.Errorf("parseTimestamp(%d) = %v, want %v", tc.in, got, tc.want)
			}
			if !tc.want.IsZero() && got.Year() != 2026 {
				t.Errorf("parseTimestamp(%d) landed in %d, not 2026 — the unit ladder is wrong", tc.in, got.Year())
			}
		})
	}
}

func TestTargetAndAuthoredBytes(t *testing.T) {
	cases := []struct {
		name       string
		action     string
		args       string
		fallback   string
		wantTarget string
		wantBytes  int64
	}{
		{
			name: "bash", action: models.ActionRunCommand,
			args:       `{"command":"ls -la","workdir":"/w","description":"list"}`,
			fallback:   "bash",
			wantTarget: "ls -la", wantBytes: 6,
		},
		{
			name: "read_file", action: models.ActionReadFile,
			args:       `{"limit":100,"offset":1,"path":"README.md"}`,
			fallback:   "read_file",
			wantTarget: "README.md", wantBytes: 0,
		},
		{
			name: "write_file", action: models.ActionWriteFile,
			args:     `{"content":"print(1)\n","path":"a.py"}`,
			fallback: "write_file",
			// `print(1)` + the JSON-decoded newline.
			wantTarget: "a.py", wantBytes: 9,
		},
		{
			name: "edit_file", action: models.ActionEditFile,
			args:       `{"find":"aa","path":"a.py","replace":"bbbb"}`,
			fallback:   "edit_file",
			wantTarget: "a.py", wantBytes: 4,
		},
		{
			name:   "unparseable args fall back to the tool name",
			action: models.ActionUnknown, args: "not json", fallback: "mystery",
			wantTarget: "mystery", wantBytes: 0,
		},
		{
			name:   "empty args fall back to the tool name",
			action: models.ActionUnknown, args: "", fallback: "mystery",
			wantTarget: "mystery", wantBytes: 0,
		},
		{
			name:   "no known key falls back to the tool name",
			action: models.ActionUnknown, args: `{"weird":1}`, fallback: "mystery",
			wantTarget: "mystery", wantBytes: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := decodeArgs(tc.args)
			if got := targetFromArgs(args, tc.fallback); got != tc.wantTarget {
				t.Errorf("targetFromArgs = %q, want %q", got, tc.wantTarget)
			}
			if got := authoredBytes(tc.action, args); got != tc.wantBytes {
				t.Errorf("authoredBytes = %d, want %d", got, tc.wantBytes)
			}
		})
	}
}

// TestFailedOutcomePolarity documents the deliberate asymmetry: only an
// EXPLICITLY named failure kind marks a call failed, so an unseen future
// outcome kind cannot invent one.
func TestFailedOutcomePolarity(t *testing.T) {
	if failedOutcomes["completed"] {
		t.Error("`completed` must not be a failure")
	}
	if failedOutcomes["some_future_kind"] {
		t.Error("an unrecognised outcome kind must not be a failure")
	}
	for _, k := range []string{"failed", "cancelled", "rejected", "timed_out"} {
		if !failedOutcomes[k] {
			t.Errorf("%q should mark a call failed", k)
		}
	}
}

func TestMatchesShape(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/h/.local/share/muse/sessions/2026/08/06/a/session.jsonl", true},
		{`C:\U\d\.local\share\MUSE\SESSIONS\2026\08\06\a\session.jsonl`, true},
		{"/h/.local/share/muse/sessions/2026/08/06/a/cron.db", false},
		{"/h/.local/share/muse/tui-history.jsonl", false},
		{"/h/.local/share/goose/sessions/2026/08/06/a/session.jsonl", false},
		{"/h/.local/share/muse/sessions/2026/08/06/a/session.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := matchesShape(tc.path); got != tc.want {
				t.Errorf("matchesShape(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestParentLogPath(t *testing.T) {
	got := parentLogPath("/h/.local/share/muse/sessions/2026/08/06/AAA/subagent/BBB/session.jsonl")
	want := "/h/.local/share/muse/sessions/2026/08/06/AAA/session.jsonl"
	if got != want {
		t.Errorf("parentLogPath = %q, want %q", got, want)
	}
	if got := parentLogPath("/h/.local/share/muse/sessions/2026/08/06/AAA/session.jsonl"); got != "" {
		t.Errorf("parentLogPath of a main log = %q, want empty", got)
	}
}

func TestIsMarker(t *testing.T) {
	if !(&rawRecord{RetainedMarker: "omitted_live_only"}).isMarker() {
		t.Error("a retained_marker line is a marker")
	}
	if (&rawRecord{PayloadType: ptRuntimeSession}).isMarker() {
		t.Error("an event record is not a marker")
	}
}
