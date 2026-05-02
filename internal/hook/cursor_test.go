package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
	"github.com/marmutapp/superbased-observer/internal/store"
)

type fakeSink struct {
	called []models.ToolEvent
	tokens []models.TokenEvent
	err    error
	delay  time.Duration
}

func (f *fakeSink) Ingest(ctx context.Context, events []models.ToolEvent, tokens []models.TokenEvent, _ store.IngestOptions) (store.IngestResult, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return store.IngestResult{}, ctx.Err()
		}
	}
	if f.err != nil {
		return store.IngestResult{}, f.err
	}
	f.called = append(f.called, events...)
	f.tokens = append(f.tokens, tokens...)
	return store.IngestResult{ActionsInserted: len(events), TokensInserted: len(tokens)}, nil
}

func TestHandleCursorEvent_HappyPath(t *testing.T) {
	body := `{
		"hook_event_name": "beforeShellExecution",
		"conversation_id": "c1",
		"generation_id": "g1",
		"workspace_roots": ["/repo"],
		"command": "go test"
	}`
	sink := &fakeSink{}
	var stdout, stderr bytes.Buffer
	HandleCursorEvent(cursor.EventBeforeShellCommand, sink, scrub.New(),
		strings.NewReader(body), &stdout, &stderr, 250*time.Millisecond)

	var reply map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &reply); err != nil {
		t.Fatalf("reply: %v: %q", err, stdout.String())
	}
	if reply["permission"] != "allow" {
		t.Errorf("permission: %v", reply["permission"])
	}
	if continueVal, _ := reply["continue"].(bool); !continueVal {
		t.Errorf("continue: %v", reply["continue"])
	}

	if len(sink.called) != 1 {
		t.Fatalf("ingest call count: %d", len(sink.called))
	}
	if sink.called[0].Target != "go test" {
		t.Errorf("target: %s", sink.called[0].Target)
	}
}

func TestHandleCursorEvent_StopIngestsTokenUsage(t *testing.T) {
	sink := &fakeSink{}
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join([]string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>Hello</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"Checking."},{"type":"tool_use","name":"ReadFile","input":{"path":"d:\\repo\\README.md"}}]}}`,
		"",
	}, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	HandleCursorEvent(cursor.EventStop, sink, nil,
		strings.NewReader(`{
			"conversation_id":"c1","generation_id":"g1",
			"workspace_roots":["/r"],"model":"default",
			"input_tokens":123,"output_tokens":45,"cache_read_tokens":67,"cache_write_tokens":8,
			"transcript_path":"`+strings.ReplaceAll(transcript, `\`, `\\`)+`"
		}`),
		&stdout, &stderr, 250*time.Millisecond)
	if len(sink.tokens) != 1 {
		t.Fatalf("stop token ingest count: %d", len(sink.tokens))
	}
	if len(sink.called) != 1 || sink.called[0].ActionType != models.ActionReadFile {
		t.Fatalf("stop transcript events = %+v", sink.called)
	}
	if sink.tokens[0].InputTokens != 123 || sink.tokens[0].Model != "default" || sink.tokens[0].MessageID != "g1" {
		t.Fatalf("unexpected token event: %+v", sink.tokens[0])
	}
	// Reply still emitted.
	if stdout.Len() == 0 {
		t.Error("no reply written for stop event")
	}
}

func TestHandleCursorEvent_DeadlineDoesNotPanic(t *testing.T) {
	// Sink takes 100ms but deadline is 5ms — ingest must time out cleanly,
	// reply still written, no panic.
	sink := &fakeSink{delay: 100 * time.Millisecond}
	var stdout, stderr bytes.Buffer
	HandleCursorEvent(cursor.EventBeforeShellCommand, sink, nil,
		strings.NewReader(`{
			"conversation_id":"c1","generation_id":"g1",
			"workspace_roots":["/r"],"command":"ls"
		}`),
		&stdout, &stderr, 5*time.Millisecond)
	if stdout.Len() == 0 {
		t.Error("reply not written")
	}
	if !strings.Contains(stderr.String(), "deadline") {
		t.Errorf("expected deadline note on stderr, got %q", stderr.String())
	}
}

func TestHandleCursorEvent_BadJSONStillReplies(t *testing.T) {
	sink := &fakeSink{}
	var stdout, stderr bytes.Buffer
	HandleCursorEvent(cursor.EventBeforeShellCommand, sink, nil,
		strings.NewReader(`{not json`),
		&stdout, &stderr, 250*time.Millisecond)
	if stdout.Len() == 0 {
		t.Error("reply not written despite bad JSON")
	}
	if len(sink.called) != 0 {
		t.Errorf("should not ingest malformed payload")
	}
	if stderr.Len() == 0 {
		t.Error("expected stderr log")
	}
}

func TestHandleCursorEvent_StripsUTF8BOM(t *testing.T) {
	sink := &fakeSink{}
	var stdout, stderr bytes.Buffer
	body := "\xEF\xBB\xBF" + `{
		"conversation_id":"c1","generation_id":"g1",
		"workspace_roots":["/r"],"command":"ls"
	}`
	HandleCursorEvent(cursor.EventBeforeShellCommand, sink, nil,
		strings.NewReader(body), &stdout, &stderr, 250*time.Millisecond)
	if stdout.Len() == 0 {
		t.Error("reply not written")
	}
	if len(sink.called) != 1 {
		t.Fatalf("expected one ingested event, got %d", len(sink.called))
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
}

func TestHandleCursorEvent_IngestErrorLoggedNotReturned(t *testing.T) {
	sink := &fakeSink{err: errors.New("boom")}
	var stdout, stderr bytes.Buffer
	HandleCursorEvent(cursor.EventBeforeShellCommand, sink, nil,
		strings.NewReader(`{
			"conversation_id":"c1","generation_id":"g1",
			"workspace_roots":["/r"],"command":"ls"
		}`),
		&stdout, &stderr, 250*time.Millisecond)
	if stdout.Len() == 0 {
		t.Error("reply not written")
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("expected error log, got %q", stderr.String())
	}
}
