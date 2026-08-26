package dashboard

import (
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func subagentTestBase() time.Time { return time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC) }

func sidechainRow(ts time.Time, success bool) models.SubagentActionRef {
	return models.SubagentActionRef{
		Timestamp: ts, ActionType: models.ActionReadFile,
		Target: "/proj/a.go", Success: success, IsSidechain: true,
	}
}

func TestBuildSubagentSummaries_BracketedWindow(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{
			Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStart,
			Target: "Explore", RawToolName: "agent-1",
			Metadata: &models.ActionMetadata{AgentID: "agent-1"},
		},
		sidechainRow(base.Add(2*time.Second), true),
		sidechainRow(base.Add(3*time.Second), false),
		{
			Timestamp: base.Add(4 * time.Second), ActionType: models.ActionSubagentStop,
			Target: "Explore", Metadata: &models.ActionMetadata{AgentID: "agent-1"},
		},
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 1 {
		t.Fatalf("got %d summaries (%+v), want 1", len(got), got)
	}
	s := got[0]
	if s.ID != "agent-1" || s.Label != "agent-1" || s.Type != "Explore" {
		t.Fatalf("identity wrong: %+v", s)
	}
	if s.ActionCount != 2 || s.ErrorCount != 1 {
		t.Fatalf("counts wrong: %+v", s)
	}
	if s.Open {
		t.Fatal("closed bracket must not be Open")
	}
	if !s.End.Equal(base.Add(4 * time.Second)) {
		t.Fatalf("End = %v, want stop timestamp", s.End)
	}
}

func TestBuildSubagentSummaries_TwoSequentialWindows(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{Timestamp: base, ActionType: models.ActionSpawnSubagent, Target: "Explore"},
		sidechainRow(base.Add(time.Second), true),
		{Timestamp: base.Add(2 * time.Second), ActionType: models.ActionSubagentStop},
		{
			Timestamp: base.Add(3 * time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "planner"},
		},
		sidechainRow(base.Add(4*time.Second), true),
		sidechainRow(base.Add(5*time.Second), true),
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 2 {
		t.Fatalf("got %d summaries (%+v), want 2", len(got), got)
	}
	if got[0].Label != "sub-agent 1" || got[0].Type != "Explore" {
		t.Fatalf("first window mislabeled: %+v", got[0])
	}
	if got[1].Label != "planner" {
		t.Fatalf("second window should take metadata id: %+v", got[1])
	}
	if got[0].ActionCount != 1 || got[1].ActionCount != 2 {
		t.Fatalf("action counts leaked across windows: %+v", got)
	}
}

func TestBuildSubagentSummaries_UnattributedBucket(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	// Sidechain rows with NO brackets at all (e.g. hook events suppressed).
	refs := []models.SubagentActionRef{
		sidechainRow(base, true),
		sidechainRow(base.Add(time.Second), true),
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 1 {
		t.Fatalf("got %d summaries (%+v), want the unattributed bucket", len(got), got)
	}
	if got[0].Label != "unattributed sub-agent activity" || got[0].ActionCount != 2 {
		t.Fatalf("bucket wrong: %+v", got[0])
	}
}

func TestBuildSubagentSummaries_StopWithoutStartSkipped(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{Timestamp: base, ActionType: models.ActionSubagentStop, Target: "Explore"},
	}
	if got := buildSubagentSummaries(refs, nil); len(got) != 0 {
		t.Fatalf("got %+v, want nothing for a stop with no open window", got)
	}
}

func TestBuildSubagentSummaries_EmptyBracketDropped(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{Timestamp: base, ActionType: models.ActionSubagentStart},
		{Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStop},
	}
	if got := buildSubagentSummaries(refs, nil); len(got) != 0 {
		t.Fatalf("got %+v, want an empty bracket pair dropped", got)
	}
}

func TestBuildSubagentSummaries_OpenWindow(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{
			Timestamp: base, ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "runner"},
		},
		sidechainRow(base.Add(time.Second), true),
		// No stop — still running.
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 1 || !got[0].Open {
		t.Fatalf("got %+v, want one open window", got)
	}
	// End carries the LAST OBSERVED ACTIVITY even while open (the UI shows
	// "running · last activity …"); Open is what marks the missing stop.
	if !got[0].End.Equal(base.Add(time.Second)) {
		t.Fatalf("open window End = %v, want last activity timestamp", got[0].End)
	}
}

func TestBuildSubagentSummaries_NonSidechainRowsIgnored(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{Timestamp: base, ActionType: models.ActionRunCommand, Target: "npm test", Success: true},
	}
	if got := buildSubagentSummaries(refs, nil); len(got) != 0 {
		t.Fatalf("got %+v, want non-sidechain rows ignored entirely", got)
	}
}

func sidechainToken(ts time.Time, in, out, cacheRead int64, cost float64) models.SubagentTokenRef {
	return models.SubagentTokenRef{
		Timestamp: ts, InputTokens: in, OutputTokens: out,
		CacheReadTokens: cacheRead, EstimatedCostUSD: cost,
	}
}

func TestBuildSubagentSummaries_TokensBucketIntoWindows(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		{
			Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "explore"},
		},
		sidechainRow(base.Add(2*time.Second), true),
		{
			Timestamp: base.Add(4 * time.Second), ActionType: models.ActionSubagentStop,
			Metadata: &models.ActionMetadata{AgentID: "explore"},
		},
		{
			Timestamp: base.Add(5 * time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "planner"},
		},
	}
	// One usage row inside window 1, one inside window 2 — must not leak.
	tokens := []models.SubagentTokenRef{
		sidechainToken(base.Add(3*time.Second), 1000, 50, 200, 0.01),
		sidechainToken(base.Add(6*time.Second), 2000, 100, 300, 0.02),
	}
	got := buildSubagentSummaries(refs, tokens)
	if len(got) != 2 {
		t.Fatalf("got %d summaries (%+v), want 2", len(got), got)
	}
	w1, w2 := got[0], got[1]
	if w1.Label != "explore" || w2.Label != "planner" {
		t.Fatalf("window labels wrong: %+v", got)
	}
	if w1.InputTokens != 1000 || w1.OutputTokens != 50 || w1.CacheReadTokens != 200 || w1.CostUSD != 0.01 {
		t.Errorf("window 1 tokens leaked or lost: %+v", w1)
	}
	if w2.InputTokens != 2000 || w2.OutputTokens != 100 || w2.CacheReadTokens != 300 || w2.CostUSD != 0.02 {
		t.Errorf("window 2 tokens wrong: %+v", w2)
	}
}

func TestBuildSubagentSummaries_TokensOutsideWindowsUnattributed(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	// A closed window plus usage rows before it opened and after it closed.
	refs := []models.SubagentActionRef{
		{
			Timestamp: base.Add(2 * time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "w"},
		},
		{
			Timestamp: base.Add(4 * time.Second), ActionType: models.ActionSubagentStop,
			Metadata: &models.ActionMetadata{AgentID: "w"},
		},
	}
	tokens := []models.SubagentTokenRef{
		sidechainToken(base, 10, 1, 0, 0.001),                // before any bracket
		sidechainToken(base.Add(3*time.Second), 20, 2, 0, 0), // inside
		sidechainToken(base.Add(5*time.Second), 40, 4, 0, 0), // after close
		sidechainToken(base.Add(6*time.Second), 80, 8, 0, 0), // still outside
	}
	got := buildSubagentSummaries(refs, tokens)
	if len(got) != 2 {
		t.Fatalf("got %d summaries (%+v), want window + unattributed bucket", len(got), got)
	}
	var bucketed, unatt *SubagentSummary
	for i := range got {
		switch got[i].Label {
		case "w":
			bucketed = &got[i]
		case "unattributed sub-agent activity":
			unatt = &got[i]
		}
	}
	if bucketed == nil || unatt == nil {
		t.Fatalf("missing windows: %+v", got)
	}
	if bucketed.InputTokens != 20 || bucketed.OutputTokens != 2 {
		t.Errorf("in-window token misattributed: %+v", bucketed)
	}
	if unatt.InputTokens != 130 || unatt.OutputTokens != 13 {
		t.Errorf("outside-window tokens must ALL land unattributed (10+40+80 / 1+4+8): %+v", unatt)
	}
	// Tie rule: a stop sharing the row's exact timestamp closes first, so
	// such a row is outside the closed window (documented cross-table rule
	// — timestamp ties carry no sub-row ordering to recover).
	atClose := buildSubagentSummaries([]models.SubagentActionRef{
		{
			Timestamp: base, ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "w"},
		},
		{
			Timestamp: base.Add(4 * time.Second), ActionType: models.ActionSubagentStop,
			Metadata: &models.ActionMetadata{AgentID: "w"},
		},
	}, []models.SubagentTokenRef{sidechainToken(base.Add(4*time.Second), 7, 0, 0, 0)})
	if len(atClose) != 2 {
		t.Fatalf("got %+v, want window + unattributed bucket", atClose)
	}
	if atClose[1].InputTokens != 7 ||
		atClose[1].Label != "unattributed sub-agent activity" {
		t.Fatalf("token at stop timestamp must be unattributed: %+v", atClose)
	}
}

func TestBuildSubagentSummaries_UsageOnlyWindowSurvives(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	// Brackets whose only payload is usage rows (actions suppressed):
	// the empty-bracket drop rule must not discard a window that carries
	// a token/cost story.
	refs := []models.SubagentActionRef{
		{
			Timestamp: base, ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "ghost"},
		},
		{
			Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStop,
			Metadata: &models.ActionMetadata{AgentID: "ghost"},
		},
	}
	tokens := []models.SubagentTokenRef{sidechainToken(base.Add(500*time.Millisecond), 500, 25, 0, 0.005)}
	got := buildSubagentSummaries(refs, tokens)
	if len(got) != 1 {
		t.Fatalf("got %+v, want the usage-only window kept", got)
	}
	if got[0].Label != "ghost" || got[0].ActionCount != 0 ||
		got[0].InputTokens != 500 || got[0].CostUSD != 0.005 {
		t.Fatalf("usage-only window wrong: %+v", got[0])
	}
}

func stopRow(ts time.Time, agentID string) models.SubagentActionRef {
	ref := models.SubagentActionRef{Timestamp: ts, ActionType: models.ActionSubagentStop}
	if agentID != "" {
		ref.Metadata = &models.ActionMetadata{AgentID: agentID}
	}
	return ref
}

// C2: a stop-without-start (claude-code's common shape — SubagentStop fires
// per sub-agent but there is no SubagentStart hook) claims the accumulated
// sidechain activity as its own closed window, labeled by the agent_id.
func TestBuildSubagentSummaries_RetroactiveClaim(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true),
		sidechainRow(base.Add(time.Second), false),
		sidechainRow(base.Add(2*time.Second), true),
		stopRow(base.Add(3*time.Second), "agent-a1"),
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 1 {
		t.Fatalf("got %d summaries (%+v), want 1 retroactive window", len(got), got)
	}
	s := got[0]
	if s.ID != "agent-a1" || s.Label != "agent-a1" || s.Open {
		t.Fatalf("retroactive identity/state wrong: %+v", s)
	}
	if s.ActionCount != 3 || s.ErrorCount != 1 {
		t.Fatalf("claimed activity wrong: %+v", s)
	}
	if !s.Start.Equal(base) || !s.End.Equal(base.Add(3*time.Second)) {
		t.Fatalf("window bounds wrong: %v..%v", s.Start, s.End)
	}
}

// Two consecutive stops without starts partition the stream into two
// sequential labeled windows instead of one unattributed bucket.
func TestBuildSubagentSummaries_RetroactiveSequentialSplits(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true),
		stopRow(base.Add(time.Second), "a"),
		sidechainRow(base.Add(2*time.Second), true),
		sidechainRow(base.Add(3*time.Second), true),
		stopRow(base.Add(4*time.Second), "b"),
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 2 {
		t.Fatalf("got %d summaries (%+v), want 2", len(got), got)
	}
	if got[0].ID != "a" || got[0].ActionCount != 1 {
		t.Fatalf("first split wrong: %+v", got[0])
	}
	if got[1].ID != "b" || got[1].ActionCount != 2 {
		t.Fatalf("second split wrong: %+v", got[1])
	}
}

// Tokens inside a retrospective span bind to its window via the replayed
// marks; tokens after the last boundary stay unattributed.
func TestBuildSubagentSummaries_RetroactiveTokenReplay(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true),
		stopRow(base.Add(2*time.Second), "a"),
	}
	tokens := []models.SubagentTokenRef{
		{Timestamp: base.Add(time.Second), InputTokens: 100, OutputTokens: 50, EstimatedCostUSD: 0.5},
		{Timestamp: base.Add(5 * time.Second), InputTokens: 7, OutputTokens: 7, EstimatedCostUSD: 0.07},
	}
	got := buildSubagentSummaries(refs, tokens)
	if len(got) != 2 {
		t.Fatalf("got %d summaries (%+v), want window + leftover bucket", len(got), got)
	}
	w := got[0]
	if w.ID != "a" || w.InputTokens != 100 || w.OutputTokens != 50 || w.CostUSD != 0.5 {
		t.Fatalf("retroactive token binding wrong: %+v", w)
	}
	left := got[1]
	if left.Label != "unattributed sub-agent activity" || left.InputTokens != 7 {
		t.Fatalf("post-stop token should stay unattributed: %+v", left)
	}
}

// A start bracket takes priority: activity before it stays unattributed,
// and the forward close path is untouched by the retroactive logic.
func TestBuildSubagentSummaries_RetroactiveYieldsToForwardStart(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true), // before any bracket: unattributed forever
		{
			Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "forward"},
		},
		sidechainRow(base.Add(2*time.Second), true),
		stopRow(base.Add(3*time.Second), "forward"),
		stopRow(base.Add(4*time.Second), "stray"), // nothing pending, no id claim on forward's window
	}
	got := buildSubagentSummaries(refs, nil)
	var forward, unattr, emptyStray int
	for _, s := range got {
		switch {
		case s.Label == "unattributed sub-agent activity":
			unattr++
		case s.Label == "forward":
			forward++
			if s.ActionCount != 1 {
				t.Fatalf("forward window count wrong: %+v", s)
			}
			if !s.End.Equal(base.Add(3 * time.Second)) {
				t.Fatalf("forward closed at wrong mark: %+v", s)
			}
		case s.ID == "stray" && s.ActionCount == 0:
			emptyStray++ // named but zero-work window past the boundary
		default:
			t.Fatalf("unexpected window %+v", s)
		}
	}
	if forward != 1 || unattr != 1 || emptyStray != 1 {
		t.Fatalf("want one forward + one unattributed + one empty stray, got %d/%d/%d (%+v)",
			forward, unattr, emptyStray, got)
	}
}

// A stop whose pending bucket predates the last boundary must NOT reach
// back across it — the stale bucket stays unattributed and a named but
// empty window records the stop's agent honestly.
func TestBuildSubagentSummaries_RetroactiveStopsAtBoundary(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true), // pre-start activity
		{
			Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "forward"},
		},
		sidechainRow(base.Add(2*time.Second), true),
		stopRow(base.Add(3*time.Second), "forward"),
		stopRow(base.Add(4*time.Second), "stray"), // nothing claimed since t3 boundary
	}
	got := buildSubagentSummaries(refs, nil)
	var stray *SubagentSummary
	counts := map[string]int{}
	for i := range got {
		s := got[i]
		counts[s.Label]++
		if s.ID == "stray" {
			stray = &got[i]
		}
	}
	if stray == nil {
		t.Fatalf("named empty window missing: %+v", got)
	}
	if stray.ActionCount != 0 || !stray.End.Equal(base.Add(4*time.Second)) ||
		!stray.Start.Equal(stray.End) {
		t.Fatalf("stray window must be empty and zero-width: %+v", *stray)
	}
	if counts["unattributed sub-agent activity"] != 1 || counts["forward"] != 1 {
		t.Fatalf("other windows wrong: %+v", got)
	}
}

// Activity after a real bracket closes must start a fresh retrospective
// bucket. Reusing the pre-start unattributed bucket makes the later stop reject
// the whole accumulator as stale and silently loses the post-boundary work.
func TestBuildSubagentSummaries_RetroactiveDetachesStaleBucket(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true), // belongs to the pre-boundary leftover
		{
			Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStart,
			Metadata: &models.ActionMetadata{AgentID: "forward"},
		},
		sidechainRow(base.Add(2*time.Second), true),
		stopRow(base.Add(3*time.Second), "forward"),
		sidechainRow(base.Add(3*time.Second), false), // ordered after the close
		stopRow(base.Add(4*time.Second), "retro"),
	}
	tokens := []models.SubagentTokenRef{
		sidechainToken(base.Add(500*time.Millisecond), 10, 0, 0, 0),
		sidechainToken(base.Add(3500*time.Millisecond), 20, 0, 0, 0),
		sidechainToken(base.Add(5*time.Second), 30, 0, 0, 0),
	}
	got := buildSubagentSummaries(refs, tokens)
	if len(got) != 3 {
		t.Fatalf("got %d summaries (%+v), want pre-boundary + forward + retro", len(got), got)
	}
	byLabel := make(map[string]SubagentSummary, len(got))
	for _, s := range got {
		byLabel[s.Label] = s
	}
	if s := byLabel["unattributed sub-agent activity"]; s.ActionCount != 1 || s.InputTokens != 40 {
		t.Fatalf("pre-boundary bucket changed: %+v", s)
	}
	if s := byLabel["forward"]; s.ActionCount != 1 || s.Open {
		t.Fatalf("forward window wrong: %+v", s)
	}
	if s := byLabel["retro"]; s.ActionCount != 1 || s.ErrorCount != 1 || s.InputTokens != 20 || s.Open ||
		!s.Start.Equal(base.Add(3*time.Second)) || !s.End.Equal(base.Add(4*time.Second)) {
		t.Fatalf("post-boundary retrospective window wrong: %+v", s)
	}
}

// A stop carrying an agent_id but no accumulated activity still records the
// sub-agent (honest event count); an id-less stop with nothing to claim is
// dropped exactly as before.
func TestBuildSubagentSummaries_RetroactiveEmptyAndIdless(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		stopRow(base, "ghost"),
		stopRow(base.Add(time.Second), ""),
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 1 || got[0].ID != "ghost" || got[0].ActionCount != 0 {
		t.Fatalf("want only the named empty window kept: %+v", got)
	}
	if !got[0].End.Equal(base) {
		t.Fatalf("empty retroactive window bounds wrong: %+v", got[0])
	}
}

// An id-less stop with pending activity claims it under an ordinal label.
func TestBuildSubagentSummaries_RetroactiveOrdinalFallback(t *testing.T) {
	t.Parallel()
	base := subagentTestBase()
	refs := []models.SubagentActionRef{
		sidechainRow(base, true),
		{Timestamp: base.Add(time.Second), ActionType: models.ActionSubagentStop, Target: "Explore"},
	}
	got := buildSubagentSummaries(refs, nil)
	if len(got) != 1 {
		t.Fatalf("got %+v, want 1", got)
	}
	if got[0].Label != "sub-agent 1" || got[0].Type != "Explore" || got[0].ActionCount != 1 {
		t.Fatalf("ordinal fallback wrong: %+v", got[0])
	}
}
