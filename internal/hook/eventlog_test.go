package hook

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withTempHome redirects HOME (and USERPROFILE on Windows) to a temp
// directory for the test's duration, so appendHookEventLog writes
// into the test's tmp area instead of the real ~/.observer/.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	return tmp
}

// TestAppendHookEventLog_RoundTrip pins the basic contract: one call
// → one JSONL line in ~/.observer/hook-events.jsonl, parseable back
// into a hookEvent.
func TestAppendHookEventLog_RoundTrip(t *testing.T) {
	home := withTempHome(t)
	appendHookEventLog(hookEvent{
		Event:     "PreToolUse",
		Bytes:     1024,
		SessionID: "sess-1",
		Tool:      "Read",
		Action:    "approve",
	})

	path := filepath.Join(home, ".observer", "hook-events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("log line missing trailing newline: %q", data)
	}
	var ev hookEvent
	if err := json.Unmarshal(data[:len(data)-1], &ev); err != nil {
		t.Fatalf("parse log: %v\nbody: %q", err, data)
	}
	if ev.Event != "PreToolUse" || ev.Bytes != 1024 || ev.SessionID != "sess-1" || ev.Tool != "Read" || ev.Action != "approve" {
		t.Errorf("round-trip fields: %+v", ev)
	}
	if ev.Ts == "" {
		t.Errorf("Ts should be auto-populated")
	}
}

// TestAppendHookEventLog_Append pins append semantics: N writes →
// N JSONL lines, each independently parseable.
func TestAppendHookEventLog_Append(t *testing.T) {
	home := withTempHome(t)
	for i := 0; i < 5; i++ {
		appendHookEventLog(hookEvent{Event: "Stop", Bytes: i})
	}
	path := filepath.Join(home, ".observer", "hook-events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		var ev hookEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Errorf("line %d not valid JSON: %v", count, err)
		}
		count++
	}
	if count != 5 {
		t.Errorf("got %d lines, want 5", count)
	}
}

// TestAppendHookEventLog_ConcurrentSafe pins that the per-process
// mutex prevents byte interleaving from concurrent writers. We fire
// 50 goroutines that each write distinct payloads, then verify every
// line parses cleanly.
func TestAppendHookEventLog_ConcurrentSafe(t *testing.T) {
	home := withTempHome(t)
	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			appendHookEventLog(hookEvent{Event: "PostToolUse", Bytes: i})
		}(i)
	}
	wg.Wait()

	path := filepath.Join(home, ".observer", "hook-events.jsonl")
	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		var ev hookEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Errorf("line %d torn: %v\ncontent: %q", count, err, scanner.Bytes())
		}
		count++
	}
	if count != N {
		t.Errorf("got %d lines, want %d", count, N)
	}
}

// TestAppendHookEventLog_NoHomeIsNoOp: when HOME is unset, the
// function is a silent no-op. Critical — the hook MUST NOT panic
// on degenerate operator environments.
func TestAppendHookEventLog_NoHomeIsNoOp(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	// No assertion needed beyond "doesn't panic / write anywhere"
	// — the function silently returns.
	appendHookEventLog(hookEvent{Event: "Stop"})
}

// TestPayloadFieldString_EmptyAndMissing: defensive parsing.
func TestPayloadFieldString_EmptyAndMissing(t *testing.T) {
	for _, c := range []struct {
		name, body, key, want string
	}{
		{"empty-body", "", "session_id", ""},
		{"malformed-json", "{not json", "session_id", ""},
		{"missing-key", `{"other": "x"}`, "session_id", ""},
		{"non-string", `{"bytes": 100}`, "bytes", ""},
		{"happy", `{"session_id": "sess-abc"}`, "session_id", "sess-abc"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := payloadFieldString([]byte(c.body), c.key); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
