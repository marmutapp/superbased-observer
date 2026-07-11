package diag

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// TestCheckHandoffReaders_CapabilityReport verifies the doctor handoff
// probe renders honestly on an empty DB: it reports the declared
// capability for every adapter (no live probe fires with no sessions), an
// actions-only tool says "metadata handover only", and a readable tool
// with a reader says "no recent session to probe". Never a hard fail.
func TestCheckHandoffReaders_CapabilityReport(t *testing.T) {
	_, database, _, _ := newTestEnv(t)

	c := checkHandoffReaders(context.Background(), database)

	if c.Name != "handoff readers" {
		t.Fatalf("check name = %q", c.Name)
	}
	if c.Status == StatusFail {
		t.Errorf("capability report must never hard-fail, got %v: %s", c.Status, c.Message)
	}
	if len(c.Details) == 0 {
		t.Fatalf("expected per-adapter detail lines, got none")
	}

	joined := strings.Join(c.Details, "\n")
	// claude-code declares a full transcript AND implements the reader; on
	// an empty DB it should report the no-recent-session branch, never a
	// fabricated read.
	var sawClaudeNoProbe bool
	for _, d := range c.Details {
		if strings.HasPrefix(d, "claude-code:") {
			if !strings.Contains(d, "no recent session to probe") {
				t.Errorf("claude-code line unexpected on empty DB: %q", d)
			}
			sawClaudeNoProbe = true
		}
	}
	if !sawClaudeNoProbe {
		t.Errorf("expected a claude-code detail line; details:\n%s", joined)
	}
	// The capability report must be grounded in the registry lanes (the
	// universal file lane always appears).
	if !strings.Contains(joined, "lanes file") {
		t.Errorf("expected grounded lane summary in details:\n%s", joined)
	}
}

// TestProbeCandidates_RetryWindow verifies the multi-session retry core of
// the handoff probe: it stops at the first candidate that yields messages,
// falls back to the latest candidate for the age hint on empty/unreadable
// outcomes, and — the regression that matters for the opencode foreign-
// mount finding — reads a session that only resolves when the reader is
// handed the RIGHT source hint (the wrong store returns nothing, mirroring
// a latest session whose rows live only in a sibling store's WAL).
func TestProbeCandidates_RetryWindow(t *testing.T) {
	msg := []models.TranscriptMessage{{Role: "user"}, {Role: "assistant"}}
	errBoom := errors.New("boom")

	// A reader that only returns messages when the candidate carries the
	// "correct" store hint — models the opencode gap where nil hints picked
	// the wrong DB. The latest candidate (sLatest) is empty in the default
	// store but present once its recorded hint routes to the right one.
	hintAware := func(_ context.Context, _ string, hints []string) ([]models.TranscriptMessage, error) {
		for _, h := range hints {
			if h == "right-store" {
				return msg, nil
			}
		}
		return nil, nil
	}

	tests := []struct {
		name        string
		cands       []probeCandidate
		read        func(context.Context, string, []string) ([]models.TranscriptMessage, error)
		wantOutcome probeOutcome
		wantMsgs    int
		wantChosen  string
		wantErr     bool
	}{
		{
			name: "first-empty-second-reads",
			cands: []probeCandidate{
				{id: "s1", started: time.Unix(200, 0)},
				{id: "s2", started: time.Unix(100, 0)},
			},
			read: func(_ context.Context, id string, _ []string) ([]models.TranscriptMessage, error) {
				if id == "s2" {
					return msg, nil
				}
				return nil, nil
			},
			wantOutcome: probeReadOK, wantMsgs: 2, wantChosen: "s2",
		},
		{
			name: "hint-routes-to-right-store",
			cands: []probeCandidate{
				{id: "sLatest", started: time.Unix(300, 0), hints: []string{"right-store"}},
				{id: "sOld", started: time.Unix(100, 0), hints: []string{"wrong-store"}},
			},
			read:        hintAware,
			wantOutcome: probeReadOK, wantMsgs: 2, wantChosen: "sLatest",
		},
		{
			name: "all-empty-reports-latest",
			cands: []probeCandidate{
				{id: "s1", started: time.Unix(200, 0)},
				{id: "s2", started: time.Unix(100, 0)},
			},
			read: func(_ context.Context, _ string, _ []string) ([]models.TranscriptMessage, error) {
				return nil, nil
			},
			wantOutcome: probeAllEmpty, wantMsgs: 0, wantChosen: "s1",
		},
		{
			name: "all-unreadable-carries-error",
			cands: []probeCandidate{
				{id: "s1", started: time.Unix(200, 0)},
				{id: "s2", started: time.Unix(100, 0)},
			},
			read: func(_ context.Context, _ string, _ []string) ([]models.TranscriptMessage, error) {
				return nil, errBoom
			},
			wantOutcome: probeAllUnreadable, wantMsgs: 0, wantChosen: "s1", wantErr: true,
		},
		{
			name: "error-then-read-still-ok",
			cands: []probeCandidate{
				{id: "s1", started: time.Unix(200, 0)},
				{id: "s2", started: time.Unix(100, 0)},
			},
			read: func(_ context.Context, id string, _ []string) ([]models.TranscriptMessage, error) {
				if id == "s1" {
					return nil, errBoom
				}
				return msg, nil
			},
			wantOutcome: probeReadOK, wantMsgs: 2, wantChosen: "s2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, msgs, chosen, lastErr := probeCandidates(
				context.Background(), tt.cands, time.Second, tt.read,
			)
			if outcome != tt.wantOutcome {
				t.Errorf("outcome = %d, want %d", outcome, tt.wantOutcome)
			}
			if msgs != tt.wantMsgs {
				t.Errorf("msgs = %d, want %d", msgs, tt.wantMsgs)
			}
			if chosen.id != tt.wantChosen {
				t.Errorf("chosen = %q, want %q", chosen.id, tt.wantChosen)
			}
			if tt.wantErr && lastErr == nil {
				t.Errorf("expected a lastErr, got nil")
			}
			if !tt.wantErr && lastErr != nil && outcome == probeReadOK {
				t.Errorf("read-OK must clear lastErr, got %v", lastErr)
			}
		})
	}
}

// TestRecentSessionsToProbe_OrdersLimitsAndAttachesHints verifies the
// probe's candidate query: newest-first, bounded by limit, out-of-window
// sessions excluded, and each candidate annotated with its recorded
// distinct source_file hints (the fix that routes the reader at the exact
// store a foreign-mount session was captured from).
func TestRecentSessionsToProbe_OrdersLimitsAndAttachesHints(t *testing.T) {
	_, database, _, _ := newTestEnv(t)
	ctx := context.Background()

	pid, err := upsertOneProject(database, "/r")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	seed := func(id string, started time.Time) {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO sessions (id, project_id, tool, started_at) VALUES (?, ?, 'opencode', ?)`,
			id, pid, started.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	act := func(id, src string) {
		if _, err := database.ExecContext(ctx,
			`INSERT INTO actions (session_id, project_id, tool, timestamp, action_type, source_file)
			 VALUES (?, ?, 'opencode', ?, 'edit', ?)`,
			id, pid, now.Format(time.RFC3339Nano), src); err != nil {
			t.Fatal(err)
		}
	}

	seed("newest", now.Add(-1*time.Hour))
	seed("middle", now.Add(-2*time.Hour))
	seed("oldest", now.Add(-3*time.Hour))
	seed("stale", now.Add(-40*24*time.Hour)) // outside the 30-day window
	// newest has two actions from the SAME store (should dedup to one hint).
	act("newest", "/mnt/c/store/opencode.db")
	act("newest", "/mnt/c/store/opencode.db")
	act("middle", "/home/u/store/opencode.db")
	// oldest has no actions → empty hints, must not error.

	cands, err := recentSessionsToProbe(ctx, database, "opencode", 3)
	if err != nil {
		t.Fatalf("recentSessionsToProbe: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("want 3 candidates (limit), got %d", len(cands))
	}
	wantOrder := []string{"newest", "middle", "oldest"}
	for i, w := range wantOrder {
		if cands[i].id != w {
			t.Errorf("cand[%d] = %q, want %q (newest-first)", i, cands[i].id, w)
		}
	}
	if len(cands[0].hints) != 1 || cands[0].hints[0] != "/mnt/c/store/opencode.db" {
		t.Errorf("newest hints = %v, want one deduped foreign-mount path", cands[0].hints)
	}
	if len(cands[1].hints) != 1 || cands[1].hints[0] != "/home/u/store/opencode.db" {
		t.Errorf("middle hints = %v", cands[1].hints)
	}
	if len(cands[2].hints) != 0 {
		t.Errorf("oldest should have no hints, got %v", cands[2].hints)
	}
	for _, c := range cands {
		if c.id == "stale" {
			t.Errorf("stale session outside the %d-day window must be excluded", handoffProbeRecentDays)
		}
	}
}
