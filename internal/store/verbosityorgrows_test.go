package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// TestSelectSessionVerbositySummaries proves the org wire row is built
// correctly from a seeded session's authored writes + commands + assistant
// text, and that it agrees with the direct LoadSessionVerbosity /
// AuthoredCaptureStats numbers for the same session (the guarantee this file
// exists to provide: the org panel matches the node's own VerbosityCard).
func TestSelectSessionVerbositySummaries(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	pid, _ := s.UpsertProject(ctx, "/tmp/svo", "")
	if err := s.UpsertSession(ctx, models.Session{
		ID: "sv-org-1", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	assistantBody := "Here is the fix.\n```go\nfunc main() {}\n```\nThat should do it."
	mk := func(eid, atype, rawName, target, body string, cb int64) models.Action {
		return models.Action{
			SessionID: "sv-org-1", ProjectID: pid, Timestamp: time.Now().UTC(),
			ActionType: atype, Target: target, Success: true,
			Tool: models.ToolClaudeCode, RawToolName: rawName, RawToolOutput: body,
			ContentBytes: cb, SourceFile: "f.jsonl", SourceEventID: eid,
		}
	}
	batch := []models.Action{
		mk("e1", models.ActionTaskComplete, "claudecode.assistant_text", "preview", assistantBody, 0),
		mk("e2", models.ActionWriteFile, "Write", "internal/x/foo.go", "", 1200),
		mk("e3", models.ActionEditFile, "Edit", "web/app.tsx", "", 300),
		mk("e4", models.ActionWriteFile, "Write", "README.md", "", 500),
		mk("e5", models.ActionRunCommand, "Bash", "go test ./...", "", 42),
	}
	if _, err := s.InsertActions(ctx, batch); err != nil {
		t.Fatal(err)
	}

	rows, err := s.SelectSessionVerbositySummaries(ctx)
	if err != nil {
		t.Fatalf("SelectSessionVerbositySummaries: %v", err)
	}
	var got *orgcontract.SessionVerbosityRow
	for i := range rows {
		if rows[i].SessionID == "sv-org-1" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("session sv-org-1 not in result (got %d rows)", len(rows))
	}

	// Cross-check against the direct node read-side for the same session.
	b, err := s.LoadSessionVerbosity(ctx, "sv-org-1")
	if err != nil {
		t.Fatal(err)
	}
	captured, total, err := s.AuthoredCaptureStats(ctx, "sv-org-1")
	if err != nil {
		t.Fatal(err)
	}
	wantCode := b.CodeBytes()
	wantExplain := b.ExplainBytes()
	wantAuthoredCaptured := total == 0 || captured > 0

	if got.CodeBytes != wantCode {
		t.Errorf("CodeBytes = %d, want %d", got.CodeBytes, wantCode)
	}
	if got.ExplainBytes != wantExplain {
		t.Errorf("ExplainBytes = %d, want %d", got.ExplainBytes, wantExplain)
	}
	if got.AuthoredCaptured != wantAuthoredCaptured {
		t.Errorf("AuthoredCaptured = %v, want %v", got.AuthoredCaptured, wantAuthoredCaptured)
	}

	// Docs write (README.md, 500 bytes) must land in DocsBytes, not CodeBytes.
	if got.DocsBytes != 500 {
		t.Errorf("DocsBytes = %d, want 500", got.DocsBytes)
	}
	// go(1200) + tsx(300) written, both category=code, plus the bash command
	// (42) and the fenced go artifact — all folded into CodeBytes already
	// asserted above; spot-check the sum of every category field reproduces
	// TotalBytes (no bytes silently dropped between ByCategory and the row).
	sumCats := got.ProseBytes + got.CodeBytes + got.DocsBytes + got.ConfigBytes + got.DataBytes + got.UnknownBytes
	if sumCats != got.TotalBytes {
		t.Errorf("category fields sum = %d, want TotalBytes %d", sumCats, got.TotalBytes)
	}

	// Channels: WrittenBytes covers both writes (1200+300+500=2000); CommandBytes
	// covers the bash run (42).
	if got.WrittenBytes != 2000 {
		t.Errorf("WrittenBytes = %d, want 2000", got.WrittenBytes)
	}
	if got.CommandBytes != 42 {
		t.Errorf("CommandBytes = %d, want 42", got.CommandBytes)
	}

	// CodeByLanguageJSON must decode to a language/category array covering go
	// and tsx (both authored code), never raw content.
	var langs []orgcontract.VerbosityLanguageBytes
	if err := json.Unmarshal([]byte(got.CodeByLanguageJSON), &langs); err != nil {
		t.Fatalf("CodeByLanguageJSON did not decode: %v", err)
	}
	seen := map[string]bool{}
	for _, l := range langs {
		seen[l.Language] = true
		if l.Bytes <= 0 {
			t.Errorf("language %q has non-positive bytes %d", l.Language, l.Bytes)
		}
	}
	if !seen["go"] || !seen["tsx"] {
		t.Errorf("CodeByLanguage missing go/tsx: %+v", langs)
	}

	if got.SessionID != "sv-org-1" {
		t.Errorf("SessionID = %q, want sv-org-1", got.SessionID)
	}
}

// TestSelectSessionVerbositySummaries_WindowExcludesOldSessions proves the
// trailing-window recompute (mirroring cacheSummaryWindowDays) excludes
// sessions that started well before the window, keeping the push payload
// bounded like the Arc-4 summaries.
func TestSelectSessionVerbositySummaries_WindowExcludesOldSessions(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	pid, _ := s.UpsertProject(ctx, "/tmp/svo-old", "")
	old := time.Now().UTC().AddDate(0, 0, -(verbositySummaryWindowDays + 30))
	if err := s.UpsertSession(ctx, models.Session{
		ID: "sv-old", ProjectID: pid, Tool: models.ToolClaudeCode, StartedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertActions(ctx, []models.Action{
		{
			SessionID: "sv-old", ProjectID: pid, Timestamp: old,
			ActionType: models.ActionWriteFile, Target: "x.go", Success: true,
			Tool: models.ToolClaudeCode, ContentBytes: 100,
			SourceFile: "f.jsonl", SourceEventID: "eold",
		},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.SelectSessionVerbositySummaries(ctx)
	if err != nil {
		t.Fatalf("SelectSessionVerbositySummaries: %v", err)
	}
	for _, r := range rows {
		if r.SessionID == "sv-old" {
			t.Fatalf("expected sv-old to be excluded by the trailing window, got a row: %+v", r)
		}
	}
}
