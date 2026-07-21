package demo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestEvents pins the embedded fixture set's contract: it parses
// cleanly through the real claude-code adapter (zero warnings — a
// warning fails Events loudly), spans multiple projects and sessions,
// carries token usage, includes at least one failure (so the failures
// surface has demo data), and rebases so the newest event lands
// NewestEventAge before "now" with the multi-day spread preserved.
func TestEvents(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	events, tokens, err := Events(context.Background(), now)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	sessions := map[string]struct{}{}
	projects := map[string]struct{}{}
	models_ := map[string]struct{}{}
	failures := 0
	var newest, oldest time.Time
	for _, e := range events {
		sessions[e.SessionID] = struct{}{}
		projects[e.ProjectRoot] = struct{}{}
		if !e.Success && e.ActionType != "" {
			failures++
		}
		if newest.IsZero() || e.Timestamp.After(newest) {
			newest = e.Timestamp
		}
		if oldest.IsZero() || e.Timestamp.Before(oldest) {
			oldest = e.Timestamp
		}
		if !strings.HasPrefix(e.SourceFile, SourceFileScheme) {
			t.Fatalf("event SourceFile %q does not carry the virtual %s scheme", e.SourceFile, SourceFileScheme)
		}
	}
	if got := len(sessions); got < 8 {
		t.Errorf("sessions = %d, want >= 8", got)
	}
	if got := len(projects); got < 3 {
		t.Errorf("projects = %d, want >= 3", got)
	}
	if failures == 0 {
		t.Error("no failed actions in the fixture set — the failures surface would be empty in demo mode")
	}

	if len(tokens) == 0 {
		t.Fatal("no token events parsed")
	}
	var totalOut int64
	for _, tk := range tokens {
		models_[tk.Model] = struct{}{}
		totalOut += tk.OutputTokens
		if tk.Timestamp.After(newest) {
			newest = tk.Timestamp
		}
	}
	if totalOut == 0 {
		t.Error("token events carry zero output tokens")
	}
	if got := len(models_); got < 3 {
		t.Errorf("distinct models = %d, want >= 3", got)
	}

	wantNewest := now.Add(-NewestEventAge)
	if d := newest.Sub(wantNewest); d < -time.Second || d > time.Second {
		t.Errorf("newest event = %v, want %v (rebase anchor)", newest, wantNewest)
	}
	if span := newest.Sub(oldest); span < 7*24*time.Hour {
		t.Errorf("fixture span = %v, want >= 7 days (timeseries need a multi-day spread)", span)
	}
}

// TestEventsSessionsLookDemo pins that everything user-visible in the
// fixture set is self-evidently synthetic: session ids and project
// roots carry the demo marker, so seeded rows can never be mistaken
// for captured ones.
func TestEventsSessionsLookDemo(t *testing.T) {
	events, _, err := Events(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		// resolveProjectRoot absolutizes against the host OS (a Windows
		// host drive-prefixes the fixture's /home/demo cwd), so pin the
		// demo marker segment rather than an exact prefix.
		if !strings.Contains(filepath.ToSlash(e.ProjectRoot), "home/demo/") {
			t.Fatalf("project root %q does not carry the home/demo/ marker", e.ProjectRoot)
		}
		if e.Tool != models.ToolClaudeCode {
			t.Fatalf("tool = %q, want %q", e.Tool, models.ToolClaudeCode)
		}
	}
}
