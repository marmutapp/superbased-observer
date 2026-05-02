package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func TestParseSessionFile_CapturesMessagesToolsAndUsage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_1","timestamp":"2026-04-23T05:47:02.362Z","cwd":"D:\\repo"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-04-23T05:47:02.372Z","provider":"ollama","modelId":"gemma4:e4b"}`,
		`{"type":"message","id":"u1","parentId":"m1","timestamp":"2026-04-23T05:47:02.380Z","message":{"role":"user","content":[{"type":"text","text":"Read README.md"}],"timestamp":1776923222379}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-04-23T05:47:20.799Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Need to read."},{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"README.md"}}],"provider":"ollama","model":"gemma4:e4b","usage":{"input":10,"output":2,"cacheRead":0,"cacheWrite":0,"totalTokens":12},"stopReason":"toolUse","timestamp":1776923222487}}`,
		`{"type":"message","id":"r1","parentId":"a1","timestamp":"2026-04-23T05:47:20.856Z","message":{"role":"toolResult","toolCallId":"call_1","toolName":"read","content":[{"type":"text","text":"# SuperBased Observer"}],"isError":false,"timestamp":1776923240856}}`,
		`{"type":"message","id":"a2","parentId":"r1","timestamp":"2026-04-23T05:47:24.336Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Done."},{"type":"text","text":"# SuperBased Observer"}],"provider":"ollama","model":"gemma4:e4b","usage":{"input":20,"output":3,"cacheRead":0,"cacheWrite":0,"totalTokens":23},"stopReason":"stop","timestamp":1776923240856}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 3 {
		t.Fatalf("expected 3 tool events, got %d", got)
	}
	if got := res.ToolEvents[0].ActionType; got != models.ActionUserPrompt {
		t.Fatalf("first action_type = %q", got)
	}
	if got := res.ToolEvents[0].MessageID; got != "user:u1" {
		t.Fatalf("user message_id = %q", got)
	}
	if got := res.ToolEvents[1].ActionType; got != models.ActionReadFile {
		t.Fatalf("second action_type = %q", got)
	}
	if got := res.ToolEvents[1].MessageID; got != "a1" {
		t.Fatalf("tool message_id = %q", got)
	}
	if got := res.ToolEvents[1].PrecedingReasoning; got != "Need to read." {
		t.Fatalf("preceding reasoning = %q", got)
	}
	if got := res.ToolEvents[1].ToolOutput; got != "# SuperBased Observer" {
		t.Fatalf("tool output = %q", got)
	}
	if got := res.ToolEvents[2].ActionType; got != models.ActionTaskComplete {
		t.Fatalf("third action_type = %q", got)
	}
	if got := res.ToolEvents[2].MessageID; got != "a2" {
		t.Fatalf("task_complete message_id = %q", got)
	}
	if got := len(res.TokenEvents); got != 2 {
		t.Fatalf("expected 2 token events, got %d", got)
	}
	if got := res.TokenEvents[0].Model; got != "ollama/gemma4:e4b" {
		t.Fatalf("token model = %q", got)
	}
	if got := res.TokenEvents[0].MessageID; got != "a1" {
		t.Fatalf("token[0] message_id = %q", got)
	}
	if got := res.TokenEvents[1].MessageID; got != "a2" {
		t.Fatalf("token[1] message_id = %q", got)
	}
}

func TestParseSessionFile_BashExecutionRoleEmitsRunCommand(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "bash.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_b","timestamp":"2026-04-29T10:00:00.000Z","cwd":"/tmp/p"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-04-29T10:00:01.000Z","provider":"anthropic","modelId":"claude-sonnet-4-5"}`,
		`{"type":"message","id":"b1","parentId":"m1","timestamp":"2026-04-29T10:00:02.000Z","message":{"role":"bashExecution","command":"ls -la","output":"total 0","exitCode":0,"cancelled":false,"truncated":false,"timestamp":1777795202000}}`,
		`{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-04-29T10:00:03.000Z","message":{"role":"bashExecution","command":"false","output":"","exitCode":1,"cancelled":false,"truncated":false,"timestamp":1777795203000}}`,
		`{"type":"message","id":"b3","parentId":"b2","timestamp":"2026-04-29T10:00:04.000Z","message":{"role":"bashExecution","command":"sleep 60","output":"","cancelled":true,"truncated":false,"timestamp":1777795204000}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.ToolEvents); got != 3 {
		t.Fatalf("expected 3 bash events, got %d", got)
	}
	for i, want := range []struct {
		target  string
		success bool
		errMsg  string
	}{
		{"ls -la", true, ""},
		{"false", false, ""},
		{"sleep 60", false, "cancelled"},
	} {
		ev := res.ToolEvents[i]
		if ev.ActionType != models.ActionRunCommand {
			t.Fatalf("event[%d] action_type = %q, want run_command", i, ev.ActionType)
		}
		if ev.Target != want.target {
			t.Fatalf("event[%d] target = %q, want %q", i, ev.Target, want.target)
		}
		if ev.Success != want.success {
			t.Fatalf("event[%d] success = %v, want %v", i, ev.Success, want.success)
		}
		if want.errMsg != "" && ev.ErrorMessage != want.errMsg {
			t.Fatalf("event[%d] error_message = %q, want %q", i, ev.ErrorMessage, want.errMsg)
		}
		if ev.RawToolName != "message.bashExecution" {
			t.Fatalf("event[%d] raw_tool_name = %q", i, ev.RawToolName)
		}
	}
}

func TestParseSessionFile_AssistantUsageCarriesCost(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "cost.jsonl")
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"ses_c","timestamp":"2026-04-29T10:00:00.000Z","cwd":"/tmp/p"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-04-29T10:00:01.000Z","provider":"anthropic","modelId":"claude-sonnet-4-5"}`,
		`{"type":"message","id":"u1","parentId":"m1","timestamp":"2026-04-29T10:00:02.000Z","message":{"role":"user","content":[{"type":"text","text":"Hi"}],"timestamp":1777795202000}}`,
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-04-29T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}],"provider":"anthropic","model":"claude-sonnet-4-5","usage":{"input":1000,"output":50,"cacheRead":200,"cacheWrite":0,"totalTokens":1250,"cost":{"input":0.003,"output":0.0015,"cacheRead":0.00006,"cacheWrite":0,"total":0.00456}},"stopReason":"stop","timestamp":1777795203000}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	a := NewWithOptions(nil, []string{root})
	res, err := a.ParseSessionFile(context.Background(), path, 0)
	if err != nil {
		t.Fatalf("ParseSessionFile: %v", err)
	}
	if got := len(res.TokenEvents); got != 1 {
		t.Fatalf("expected 1 token event, got %d", got)
	}
	tok := res.TokenEvents[0]
	if tok.EstimatedCostUSD != 0.00456 {
		t.Fatalf("cost = %v, want 0.00456", tok.EstimatedCostUSD)
	}
	if tok.InputTokens != 1000 || tok.OutputTokens != 50 || tok.CacheReadTokens != 200 {
		t.Fatalf("token breakdown wrong: %+v", tok)
	}
}

func TestParseSessionFile_TerminalStopReasonsEmitTaskComplete(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopReason string
		errorMsg   string
		wantEmit   bool
		wantOK     bool
	}{
		{"stop", "stop", "", true, true},
		{"length", "length", "", true, true},
		{"error", "error", "5xx upstream", true, false},
		{"aborted", "aborted", "", true, false},
		{"toolUse", "toolUse", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "stop.jsonl")
			line := `{"type":"message","id":"a1","parentId":"m1","timestamp":"2026-04-29T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"x"}],"provider":"anthropic","model":"claude-sonnet-4-5","stopReason":"` + tc.stopReason + `","errorMessage":"` + tc.errorMsg + `","timestamp":1777795203000}}`
			body := strings.Join([]string{
				`{"type":"session","version":3,"id":"ses_s","timestamp":"2026-04-29T10:00:00.000Z","cwd":"/tmp/p"}`,
				`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-04-29T10:00:01.000Z","provider":"anthropic","modelId":"claude-sonnet-4-5"}`,
				line,
				"",
			}, "\n")
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			a := NewWithOptions(nil, []string{root})
			res, err := a.ParseSessionFile(context.Background(), path, 0)
			if err != nil {
				t.Fatal(err)
			}
			var got *models.ToolEvent
			for i := range res.ToolEvents {
				if res.ToolEvents[i].ActionType == models.ActionTaskComplete {
					got = &res.ToolEvents[i]
					break
				}
			}
			if tc.wantEmit && got == nil {
				t.Fatalf("expected task_complete for stopReason=%q, got none", tc.stopReason)
			}
			if !tc.wantEmit && got != nil {
				t.Fatalf("expected no task_complete for stopReason=%q, got %+v", tc.stopReason, got)
			}
			if tc.wantEmit {
				if got.Target != tc.stopReason {
					t.Fatalf("target = %q, want %q", got.Target, tc.stopReason)
				}
				if got.Success != tc.wantOK {
					t.Fatalf("success = %v, want %v", got.Success, tc.wantOK)
				}
			}
		})
	}
}
