package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/handoff"
	"github.com/marmutapp/superbased-observer/internal/handoffsvc"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// continueSessionStub returns a canned handoff result and records the
// request — the tool is a thin wrapper, so the tests pin argument
// mapping + addressing, not the service logic.
func continueSessionStub(got *handoffsvc.Request) HandoffRunner {
	return func(_ context.Context, req handoffsvc.Request) (handoffsvc.Result, error) {
		*got = req
		return handoffsvc.Result{
			Doc:         "<!-- superbased-handoff aa11 -->\n# Session handoff — codex → here",
			ShortID:     "aa11",
			CarryUsed:   handoff.CarryDistilledTail,
			TargetModel: "gpt-5.4",
			Fork:        handoff.ForkResolution{ResolvedIndex: 4, Snapped: true, Reason: "requested message 5, snapped to 4 — inside an unresolved tool chain"},
			Estimate: handoff.EstimateResult{
				TargetModel: "gpt-5.4",
				ForkShare:   1,
				Rows: []handoff.CarryEstimate{
					{Mode: handoff.CarryMetadata, Tokens: 500, CostUSD: 0.01, Note: "action-derived facts only"},
					{Mode: handoff.CarryDistilledTail, Tokens: 2100, CostUSD: 0.04, Note: "facts + mission + verbatim tail"},
				},
			},
		}, nil
	}
}

func newContinueSessionServer(t *testing.T) (*Server, *store.Store, *handoffsvc.Request) {
	t.Helper()
	s, database, _ := testServer(t)
	var got handoffsvc.Request
	s.Register(newContinueSessionTool(database, continueSessionStub(&got)))
	return s, store.New(database), &got
}

func TestContinueSession_BySessionID(t *testing.T) {
	s, _, got := newContinueSessionServer(t)

	out := callTool(t, s, "continue_session", map[string]any{
		"session_id":   "sess-1",
		"fork_message": 5,
		"carry":        "distilled_tail",
		"target_model": "gpt-5.4",
	})

	if got.SessionID != "sess-1" || got.Carry != handoff.CarryDistilledTail ||
		got.TargetModel != "gpt-5.4" || !got.DryRun {
		t.Errorf("request = %+v", *got)
	}
	if got.Fork.Kind != handoff.ForkMessageIndex || got.Fork.MessageIndex != 5 {
		t.Errorf("fork = %+v", got.Fork)
	}
	if h, _ := out["handover"].(string); !strings.Contains(h, "superbased-handoff") {
		t.Errorf("handover = %q", h)
	}
	if out["carry_used"] != "distilled_tail" || out["fork_index"] != float64(4) {
		t.Errorf("out = %+v", out)
	}
	if note, _ := out["fork_note"].(string); !strings.Contains(note, "snapped") {
		t.Errorf("fork_note = %q", note)
	}
	rows := out["estimate"].([]any)
	if len(rows) != 2 {
		t.Fatalf("estimate rows = %d, want 2", len(rows))
	}
}

func TestContinueSession_LatestAddressing(t *testing.T) {
	s, st, got := newContinueSessionServer(t)
	ctx := context.Background()
	pid, _ := st.UpsertProject(ctx, "/tmp/csA", "")
	pid2, _ := st.UpsertProject(ctx, "/tmp/csB", "")
	base := time.Now().UTC()
	for _, row := range []struct {
		id, tool string
		pid      int64
		at       time.Time
	}{
		{"cs-old", "codex", pid, base.Add(-2 * time.Hour)},
		{"cs-new", "codex", pid, base.Add(-1 * time.Hour)},
		{"cs-other", "claude-code", pid2, base.Add(-30 * time.Minute)},
	} {
		if err := st.UpsertSession(ctx, models.Session{
			ID: row.id, ProjectID: row.pid, Tool: row.tool, StartedAt: row.at,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// latest with a tool filter picks the newest codex session.
	callTool(t, s, "continue_session", map[string]any{"latest": true, "tool": "codex"})
	if got.SessionID != "cs-new" {
		t.Errorf("latest codex = %q, want cs-new", got.SessionID)
	}
	// latest unfiltered picks the newest overall.
	callTool(t, s, "continue_session", map[string]any{"latest": true})
	if got.SessionID != "cs-other" {
		t.Errorf("latest = %q, want cs-other", got.SessionID)
	}
	// latest with a project filter.
	callTool(t, s, "continue_session", map[string]any{"latest": true, "project_root": "/tmp/csA"})
	if got.SessionID != "cs-new" {
		t.Errorf("latest /tmp/csA = %q, want cs-new", got.SessionID)
	}
	// write_file flips the dry run off.
	callTool(t, s, "continue_session", map[string]any{"session_id": "cs-old", "write_file": true})
	if got.DryRun {
		t.Error("write_file=true must not be a dry run")
	}
}

func TestContinueSession_Errors(t *testing.T) {
	s, _, _ := newContinueSessionServer(t)
	msg := callToolExpectError(t, s, "continue_session", map[string]any{})
	if !strings.Contains(msg, "session_id") || !strings.Contains(msg, "latest") {
		t.Errorf("missing-args error = %q", msg)
	}
	msg = callToolExpectError(t, s, "continue_session", map[string]any{"latest": true, "tool": "no-such-tool"})
	if !strings.Contains(msg, "no session matches") {
		t.Errorf("no-match error = %q", msg)
	}
}

func TestContinueSession_DegradesWithoutRunner(t *testing.T) {
	s, _, _ := testServer(t) // default server: BuildHandoff nil
	msg := callToolExpectError(t, s, "continue_session", map[string]any{"session_id": "x"})
	if !strings.Contains(msg, "not wired") {
		t.Errorf("degraded error = %q", msg)
	}
}
