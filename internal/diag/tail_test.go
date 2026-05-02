package diag

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// safeBuffer wraps bytes.Buffer with a mutex so the test goroutine can read
// while Tail writes from its own goroutine.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestTail_StreamsNewActions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "obs.db")
	d, err := db.Open(context.Background(), db.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st := store.New(d)

	// Pre-existing action — Tail should ignore it because we set Since=now
	// after this insert.
	pre := models.ToolEvent{
		SourceFile: "p.jsonl", SourceEventID: "p1",
		SessionID: "s1", ProjectRoot: "/r",
		Timestamp: time.Now().Add(-time.Hour).UTC(),
		Tool:      models.ToolClaudeCode, ActionType: models.ActionReadFile,
		Target: "old.go",
	}
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{pre}, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	out := &safeBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- Tail(ctx, d, out, TailOptions{
			Interval: 50 * time.Millisecond,
			Since:    time.Now().UTC(),
		})
	}()

	// Give Tail a moment to bootstrap its lastID.
	time.Sleep(100 * time.Millisecond)
	now := time.Now().UTC()
	if _, err := st.Ingest(context.Background(), []models.ToolEvent{
		{
			SourceFile: "n.jsonl", SourceEventID: "n1",
			SessionID: "s1", ProjectRoot: "/r",
			Timestamp: now,
			Tool:      models.ToolClaudeCode, ActionType: models.ActionRunCommand,
			Target: "go build", Success: true,
		},
		{
			SourceFile: "n.jsonl", SourceEventID: "n2",
			SessionID: "s1", ProjectRoot: "/r",
			Timestamp: now.Add(time.Millisecond),
			Tool:      models.ToolClaudeCode, ActionType: models.ActionEditFile,
			Target: "main.go", Success: true,
		},
	}, nil, store.IngestOptions{}); err != nil {
		t.Fatalf("Ingest new: %v", err)
	}

	// Wait for at least one poll to land both rows.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "go build") && strings.Contains(out.String(), "main.go") {
			break
		}
		time.Sleep(60 * time.Millisecond)
	}
	cancel()
	<-done

	str := out.String()
	if !strings.Contains(str, "go build") {
		t.Errorf("first row missing: %q", str)
	}
	if !strings.Contains(str, "main.go") {
		t.Errorf("second row missing: %q", str)
	}
	if strings.Contains(str, "old.go") {
		t.Errorf("Tail emitted pre-existing row: %q", str)
	}
}

func TestFormatEntry_FailureMark(t *testing.T) {
	e := TailEntry{
		Timestamp:  time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC),
		Tool:       "claude-code",
		ActionType: "run_command",
		Target:     "go test",
		Success:    false,
		SessionID:  "sess-1",
	}
	line := formatEntry(e)
	if !strings.HasPrefix(line, "✗") {
		t.Errorf("expected ✗ prefix, got %q", line)
	}
}
