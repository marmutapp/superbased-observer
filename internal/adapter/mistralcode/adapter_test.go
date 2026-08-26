package mistralcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// writeVibeSession builds a synthetic vibe session dir with meta.json +
// messages.jsonl mirroring the live shapes (session_prompt_tokens is GROSS,
// includes session_cached_tokens).
func writeVibeSession(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, ".vibe", "logs", "session", "session_20260811_080509_088a17fc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{
	  "session_id": "088a17fc-8965-5286-f8d3-b48a6cfb9017",
	  "start_time": "2026-08-11T08:05:09.665710+00:00",
	  "end_time": "2026-08-11T08:10:39.700318+00:00",
	  "git_branch": "main",
	  "environment": {"working_directory": "/home/dev/needlehaystack"},
	  "stats": {
	    "session_prompt_tokens": 146062,
	    "session_completion_tokens": 1023,
	    "session_cached_tokens": 115072,
	    "session_total_llm_tokens": 147085,
	    "session_cost": 0.0714183
	  },
	  "config": {"active_model": "", "models": {"mistral-medium-3.5": {"provider": "mistral"}}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	msgs := `{"role":"user","content":"summarize this project","message_id":"u1"}
{"role":"assistant","content":null,"reasoning_content":"I'll look around.","message_id":"a1","tool_calls":[{"id":"tc1","index":0,"type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls -la\"}"}}]}
{"role":"tool","name":"bash","tool_call_id":"tc1","content":"README.md\npyproject.toml\n"}
{"role":"assistant","content":null,"message_id":"a2","tool_calls":[{"id":"tc2","index":0,"type":"function","function":{"name":"read_file","arguments":"{\"file_path\":\"/home/dev/needlehaystack/README.md\"}"}}]}
{"role":"tool","name":"read_file","tool_call_id":"tc2","content":"Error: file not found"}
{"role":"assistant","content":"Here is the summary.","message_id":"a3"}
`
	msgPath := filepath.Join(dir, "messages.jsonl")
	if err := os.WriteFile(msgPath, []byte(msgs), 0o644); err != nil {
		t.Fatal(err)
	}
	return msgPath
}

func TestParseSessionFile_ActionsAndSessionTokens(t *testing.T) {
	root := t.TempDir()
	msgPath := writeVibeSession(t, root)
	a := NewWithOptions(nil, filepath.Join(root, ".vibe", "logs", "session"))
	if !a.IsSessionFile(msgPath) {
		t.Fatal("IsSessionFile should accept messages.jsonl under the watch root")
	}
	res, err := a.ParseSessionFile(context.Background(), msgPath, 0)
	if err != nil {
		t.Fatal(err)
	}

	at := map[string]int{}
	var bashEv, readEv *models.ToolEvent
	for i := range res.ToolEvents {
		ev := &res.ToolEvents[i]
		at[ev.ActionType]++
		if ev.SessionID != "088a17fc" {
			t.Errorf("SessionID = %q, want 088a17fc (dir suffix)", ev.SessionID)
		}
		if ev.ProjectRoot != "/home/dev/needlehaystack" {
			t.Errorf("ProjectRoot = %q, want /home/dev/needlehaystack", ev.ProjectRoot)
		}
		switch ev.RawToolName {
		case "bash":
			bashEv = ev
		case "read_file":
			readEv = ev
		}
	}
	if at[models.ActionUserPrompt] != 1 {
		t.Errorf("user_prompt count = %d, want 1", at[models.ActionUserPrompt])
	}
	if at[models.ActionRunCommand] != 1 {
		t.Errorf("run_command count = %d, want 1", at[models.ActionRunCommand])
	}
	if at[models.ActionAssistantMessage] != 1 {
		t.Errorf("assistant_message count = %d, want 1", at[models.ActionAssistantMessage])
	}
	if bashEv == nil || !bashEv.Success {
		t.Error("bash tool event missing or not marked success")
	}
	if bashEv != nil && bashEv.Target != "ls -la" {
		t.Errorf("bash target = %q, want 'ls -la'", bashEv.Target)
	}
	// read_file's tool result began with 'Error' → success=false
	if readEv == nil || readEv.Success {
		t.Error("read_file event should be marked NOT success (result begins with 'Error')")
	}

	if len(res.TokenEvents) != 1 {
		t.Fatalf("want exactly 1 session-level token event, got %d", len(res.TokenEvents))
	}
	tk := res.TokenEvents[0]
	if tk.SourceEventID != "tokens:session:088a17fc" {
		t.Errorf("token SourceEventID = %q, want tokens:session:088a17fc", tk.SourceEventID)
	}
	if tk.InputTokens != 30990 { // 146062 gross - 115072 cached
		t.Errorf("InputTokens (net) = %d, want 30990", tk.InputTokens)
	}
	if tk.OutputTokens != 1023 {
		t.Errorf("OutputTokens = %d, want 1023", tk.OutputTokens)
	}
	if tk.CacheReadTokens != 115072 {
		t.Errorf("CacheReadTokens = %d, want 115072", tk.CacheReadTokens)
	}
	if tk.Model != "mistral-medium-3.5" {
		t.Errorf("Model = %q, want mistral-medium-3.5", tk.Model)
	}
	if tk.EstimatedCostUSD < 0.07 || tk.EstimatedCostUSD > 0.072 {
		t.Errorf("EstimatedCostUSD = %v, want ~0.0714", tk.EstimatedCostUSD)
	}
}

func TestParseSessionFile_IdempotentReparse(t *testing.T) {
	root := t.TempDir()
	msgPath := writeVibeSession(t, root)
	a := NewWithOptions(nil, filepath.Join(root, ".vibe", "logs", "session"))
	first, err := a.ParseSessionFile(context.Background(), msgPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.NewOffset <= 0 {
		t.Fatalf("offset should advance, got %d", first.NewOffset)
	}
	second, err := a.ParseSessionFile(context.Background(), msgPath, first.NewOffset)
	if err != nil {
		t.Fatal(err)
	}
	// No new transcript lines → no new actions. (A session token event MAY
	// re-emit from meta.json; the store's MAX-upgrade dedupes it — but no
	// tool events should reappear.)
	if len(second.ToolEvents) != 0 {
		t.Errorf("reparse from offset yielded %d tool events, want 0", len(second.ToolEvents))
	}
}

func TestIsSessionFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".vibe", "logs", "session")
	a := NewWithOptions(nil, root)
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "session_x_ab", "messages.jsonl"), true},
		{filepath.Join(root, "session_x_ab", "meta.json"), false},
		{filepath.Join(t.TempDir(), "elsewhere", "messages.jsonl"), false},
	}
	for _, c := range cases {
		if got := a.IsSessionFile(c.path); got != c.want {
			t.Errorf("IsSessionFile(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestName(t *testing.T) {
	if got := New().Name(); got != models.ToolMistralCode {
		t.Errorf("Name() = %q, want %q", got, models.ToolMistralCode)
	}
}
