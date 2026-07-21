package store

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/pidbridge"
)

// TestIngest_SessionProcessSeeds exercises the watcher-path attribution
// seam end to end: a ParseResult seed handed via IngestOptions is
// validated (liveness + identity) and written to session_pid_bridge for
// a live pid, and skipped for a dead one.
func TestIngest_SessionProcessSeeds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("ValidateLocalProcess is /proc-based; linux only")
	}
	s, d := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)

	ev := models.ToolEvent{
		SourceFile: "cline.db", SourceEventID: "ss1",
		SessionID: "sess-live", ProjectRoot: "/repo",
		Timestamp: now, Tool: models.ToolClineCLI,
		ActionType: models.ActionSessionStart, Target: "startup", Success: true,
	}
	// One live seed (this test process, liveness-only hint) and one
	// dead seed (a pid that can't exist).
	opts := IngestOptions{SessionProcessSeeds: []models.SessionProcessSeed{
		{PID: os.Getpid(), SessionID: "sess-live", Tool: models.ToolClineCLI, CWD: "/repo", ExecHint: ""},
		{PID: 999999, SessionID: "sess-dead", Tool: models.ToolClineCLI, CWD: "/repo", ExecHint: "cline"},
	}}
	if _, err := s.Ingest(ctx, []models.ToolEvent{ev}, nil, opts); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	bridge := pidbridge.New(d)
	if e, ok, err := bridge.Lookup(ctx, os.Getpid()); err != nil || !ok {
		t.Fatalf("live seed not written: ok=%v err=%v", ok, err)
	} else if e.SessionID != "sess-live" || e.Tool != models.ToolClineCLI {
		t.Errorf("bridge entry = %+v; want sess-live/cline-cli", e)
	}
	if _, ok, err := bridge.Lookup(ctx, 999999); err != nil {
		t.Fatalf("lookup dead: %v", err)
	} else if ok {
		t.Error("dead-pid seed was written; want skipped")
	}
}
