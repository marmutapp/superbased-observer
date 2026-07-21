package handoff

import (
	"strings"
	"testing"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

func t0(min int) time.Time {
	return time.Date(2026, 7, 3, 10, min, 0, 0, time.UTC)
}

func user(idx, min int, text string) models.TranscriptMessage {
	return models.TranscriptMessage{Index: idx, Role: models.TranscriptUser, Time: t0(min), Text: text}
}

func assistant(idx, min int, text string, calls ...models.ToolCallRef) models.TranscriptMessage {
	return models.TranscriptMessage{Index: idx, Role: models.TranscriptAssistant, Time: t0(min), Text: text, ToolCalls: calls}
}

func resolved(id string) models.ToolCallRef {
	return models.ToolCallRef{ID: id, Name: "Edit", InputExcerpt: "{f}", ResultExcerpt: "ok", Resolved: true}
}

func dangling(id string) models.ToolCallRef {
	return models.ToolCallRef{ID: id, Name: "Edit", InputExcerpt: "{f}", Resolved: false}
}

// fixture: u a u a(dangling) a u  — indices 1..6 in fork terms.
func forkFixture() []models.TranscriptMessage {
	return []models.TranscriptMessage{
		user(0, 1, "build the feature"),
		assistant(1, 2, "done part one", resolved("c1")),
		user(2, 3, "now part two"),
		assistant(3, 4, "working", dangling("c2")),
		assistant(4, 5, "settled", resolved("c3")),
		user(5, 6, "and part three?"),
	}
}

// TestResolveFork_SnapTable exercises one case per plan §7 snap-table row.
func TestResolveFork_SnapTable(t *testing.T) {
	msgs := forkFixture()
	cases := []struct {
		name       string
		fp         ForkPoint
		wantIdx    int
		wantSnap   bool
		wantErrSub string
	}{
		{name: "row1 request past end resolves to last stable", fp: ForkPoint{Kind: ForkMessageIndex, MessageIndex: 99}, wantIdx: 5, wantSnap: true},
		{name: "row2 accept at resolved assistant", fp: ForkPoint{Kind: ForkMessageIndex, MessageIndex: 2}, wantIdx: 2},
		{name: "row3 snap back from dangling chain", fp: ForkPoint{Kind: ForkMessageIndex, MessageIndex: 4}, wantIdx: 2, wantSnap: true},
		{name: "row4 snap back past unanswered user", fp: ForkPoint{Kind: ForkMessageIndex, MessageIndex: 3}, wantIdx: 2, wantSnap: true},
		{name: "row5 no stable boundary errors", fp: ForkPoint{Kind: ForkMessageIndex, MessageIndex: 1}, wantErrSub: "no stable fork point"},
		{name: "default last snaps past trailing user", fp: ForkPoint{}, wantIdx: 5, wantSnap: true},
		{name: "invalid index errors", fp: ForkPoint{Kind: ForkMessageIndex, MessageIndex: 0}, wantErrSub: "1-based"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ResolveFork(msgs, tc.fp)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("want error containing %q, got %v", tc.wantErrSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.ResolvedIndex != tc.wantIdx {
				t.Errorf("resolved index = %d, want %d (reason %q)", res.ResolvedIndex, tc.wantIdx, res.Reason)
			}
			if res.Snapped != tc.wantSnap {
				t.Errorf("snapped = %v, want %v (reason %q)", res.Snapped, tc.wantSnap, res.Reason)
			}
			if res.Snapped && res.Reason == "" {
				t.Error("snapped resolution must carry a reason")
			}
		})
	}
}

func TestResolveFork_ByTimestamp(t *testing.T) {
	msgs := forkFixture()
	res, err := ResolveFork(msgs, ForkPoint{Kind: ForkTime, Time: t0(4).Add(30 * time.Second)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Cut at message 4 (the dangling assistant) must snap back to 2.
	if res.ResolvedIndex != 2 || !res.Snapped {
		t.Fatalf("got idx %d snapped %v, want 2/true", res.ResolvedIndex, res.Snapped)
	}
}

func TestResolveFork_TimestampWithoutTimes(t *testing.T) {
	msgs := []models.TranscriptMessage{
		{Role: models.TranscriptUser, Text: "hi"},
		{Role: models.TranscriptAssistant, Text: "hello"},
	}
	if _, err := ResolveFork(msgs, ForkPoint{Kind: ForkTime, Time: t0(1)}); err == nil {
		t.Fatal("want error for timestamp fork on a time-less transcript")
	}
}

func TestResolveFork_EmptyTranscriptIsMetadataOnly(t *testing.T) {
	res, err := ResolveFork(nil, ForkPoint{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ResolvedIndex != 0 || res.Reason == "" {
		t.Fatalf("want metadata-only resolution with reason, got %+v", res)
	}
}

func TestForkShare(t *testing.T) {
	msgs := forkFixture()
	res := ForkResolution{ResolvedIndex: 2}
	share := ForkShare(msgs, res)
	if share <= 0 || share >= 1 {
		t.Fatalf("mid-fork share must be in (0,1), got %v", share)
	}
	if got := ForkShare(msgs, ForkResolution{ResolvedIndex: len(msgs)}); got != 1.0 {
		t.Fatalf("full-cut share = %v, want 1.0", got)
	}
	if got := ForkShare(nil, ForkResolution{}); got != 1.0 {
		t.Fatalf("empty share = %v, want 1.0", got)
	}
}

// TestBoundaries pins the fork-picker view: one row per message, snap-table
// stability mirrored, cumulative share monotone and ending at 1.0.
func TestBoundaries(t *testing.T) {
	msgs := forkFixture()
	rows := Boundaries(msgs)
	if len(rows) != len(msgs) {
		t.Fatalf("rows = %d, want %d", len(rows), len(msgs))
	}
	// fixture: u a u a(dangling) a u — stability per snap table.
	wantStable := []bool{false, true, false, false, true, false}
	prev := 0.0
	for i, b := range rows {
		if b.Index != i+1 {
			t.Errorf("row %d index = %d, want %d", i, b.Index, i+1)
		}
		if b.Stable != wantStable[i] {
			t.Errorf("row %d stable = %v, want %v", i, b.Stable, wantStable[i])
		}
		if !b.Stable && b.Reason == "" {
			t.Errorf("row %d unstable without a reason", i)
		}
		if b.Stable && b.Reason != "" {
			t.Errorf("row %d stable with reason %q", i, b.Reason)
		}
		if b.CumulativeShare < prev || b.CumulativeShare <= 0 || b.CumulativeShare > 1 {
			t.Errorf("row %d cumulative share %v not monotone in (0,1]", i, b.CumulativeShare)
		}
		prev = b.CumulativeShare
	}
	if last := rows[len(rows)-1].CumulativeShare; last != 1.0 {
		t.Errorf("last cumulative share = %v, want 1.0", last)
	}
	// Cumulative share at a mid cut must equal ForkShare at the same cut —
	// one weighting definition (messageWeight).
	if got, want := rows[1].CumulativeShare, ForkShare(msgs, ForkResolution{ResolvedIndex: 2}); got != want {
		t.Errorf("boundary share %v != fork share %v", got, want)
	}
	if rows[3].ToolCallCount != 1 {
		t.Errorf("dangling assistant tool call count = %d, want 1", rows[3].ToolCallCount)
	}
	if Boundaries(nil) != nil {
		t.Error("empty transcript must yield nil boundaries")
	}
}

// TestBoundaries_PreviewCapsAndOneLine pins the preview rules: first line
// only, capped at boundaryPreviewCap.
func TestBoundaries_PreviewCapsAndOneLine(t *testing.T) {
	long := strings.Repeat("x", boundaryPreviewCap+50)
	msgs := []models.TranscriptMessage{
		{Role: models.TranscriptUser, Text: "first line\nsecond line"},
		{Role: models.TranscriptAssistant, Text: long, ToolCalls: []models.ToolCallRef{{Resolved: true}}},
	}
	rows := Boundaries(msgs)
	if rows[0].Preview != "first line" {
		t.Errorf("preview = %q, want first line only", rows[0].Preview)
	}
	if len(rows[1].Preview) > boundaryPreviewCap+len("…") || !strings.HasSuffix(rows[1].Preview, "…") {
		t.Errorf("long preview not capped: %d bytes", len(rows[1].Preview))
	}
}
