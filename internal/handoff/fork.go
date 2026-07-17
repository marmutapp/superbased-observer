package handoff

import (
	"fmt"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/models"
)

// ForkKind names how a fork point was addressed (plan §7). The zero value
// is ForkLast: fork after the final message, the default that never asks.
type ForkKind string

const (
	// ForkLast cuts after the final message (default; zero value).
	ForkLast ForkKind = ""
	// ForkMessageIndex cuts after the 1-based message index over the
	// normalized transcript.
	ForkMessageIndex ForkKind = "message_index"
	// ForkTime cuts after the last message at or before a timestamp.
	ForkTime ForkKind = "timestamp"
)

// ForkPoint is a normalized cut request. The zero value means "after the
// last message".
type ForkPoint struct {
	Kind         ForkKind
	MessageIndex int       // 1-based; ForkMessageIndex only
	Time         time.Time // ForkTime only
}

// ForkResolution reports where the cut actually landed after the
// stable-boundary snap table ran. ResolvedIndex is the 1-based index of
// the LAST INCLUDED message (== the count of included messages); 0 means
// the transcript is empty and the handoff proceeds metadata-only.
type ForkResolution struct {
	RequestedIndex int // 1-based request; 0 = last
	ResolvedIndex  int
	Snapped        bool
	Reason         string
	// ForkTime is the time of the last included message (zero when the
	// source carries no per-message timestamps); as-of-fork fact filtering
	// anchors on it.
	ForkTime time.Time
}

// snapRule is one row of the plan §7 stable-boundary table, matched
// against the message at the current cut. Exactly one row matches any
// message; accept stops the walk, otherwise the cut steps backward with
// the row's reason.
type snapRule struct {
	name    string
	matches func(m models.TranscriptMessage) bool
	accept  bool
	reason  string
}

// snapTable is walked top-down for the message at the cut (CLAUDE.md rule
// #5: ordered rules as data, one test case per row).
var snapTable = []snapRule{
	{
		name: "assistant-resolved",
		matches: func(m models.TranscriptMessage) bool {
			return m.Role == models.TranscriptAssistant && allResolved(m.ToolCalls)
		},
		accept: true,
	},
	{
		name:    "assistant-dangling",
		matches: func(m models.TranscriptMessage) bool { return m.Role == models.TranscriptAssistant },
		accept:  false,
		reason:  "inside an unresolved tool chain",
	},
	{
		name:    "user-awaiting-reply",
		matches: func(m models.TranscriptMessage) bool { return m.Role == models.TranscriptUser },
		accept:  false,
		reason:  "a fork must end on settled state, not a user message awaiting its reply",
	},
}

func allResolved(calls []models.ToolCallRef) bool {
	for _, c := range calls {
		if !c.Resolved {
			return false
		}
	}
	return true
}

// ResolveFork resolves a fork request against the normalized transcript
// through the stable-boundary snap table. An empty transcript resolves to
// index 0 (metadata-only) without error; an addressed request that cannot
// land on any stable boundary errors.
func ResolveFork(msgs []models.TranscriptMessage, fp ForkPoint) (ForkResolution, error) {
	res := ForkResolution{}
	if len(msgs) == 0 {
		res.Reason = "no transcript — handoff proceeds metadata-only"
		return res, nil
	}

	cut := len(msgs)
	switch fp.Kind {
	case ForkLast:
		// cut stays at len.
	case ForkMessageIndex:
		if fp.MessageIndex < 1 {
			return res, fmt.Errorf("handoff: fork message index %d is not a 1-based message position", fp.MessageIndex)
		}
		res.RequestedIndex = fp.MessageIndex
		if fp.MessageIndex < cut {
			cut = fp.MessageIndex
		}
	case ForkTime:
		if msgs[0].Time.IsZero() {
			return res, fmt.Errorf("handoff: source transcript carries no per-message timestamps; address the fork with a message index instead")
		}
		cut = 0
		for i, m := range msgs {
			if !m.Time.After(fp.Time) {
				cut = i + 1
			}
		}
		res.RequestedIndex = cut
		if cut == 0 {
			return res, fmt.Errorf("handoff: no message at or before %s", fp.Time.Format(time.RFC3339))
		}
	default:
		return res, fmt.Errorf("handoff: unknown fork kind %q", fp.Kind)
	}

	requested := cut
	for cut > 0 {
		m := msgs[cut-1]
		for _, rule := range snapTable {
			if !rule.matches(m) {
				continue
			}
			if rule.accept {
				res.ResolvedIndex = cut
				res.ForkTime = m.Time
				if cut != requested {
					res.Snapped = true
					res.Reason = fmt.Sprintf("requested message %d, snapped to %d — %s", requested, cut, snapReason(msgs, requested))
				}
				return res, nil
			}
			cut--
			break
		}
	}
	return res, fmt.Errorf("handoff: no stable fork point at or before message %d", requested)
}

// snapReason names why the originally requested cut was unstable (the
// first back-step's rule reason).
func snapReason(msgs []models.TranscriptMessage, requested int) string {
	m := msgs[requested-1]
	for _, rule := range snapTable {
		if rule.matches(m) {
			return rule.reason
		}
	}
	return "unstable boundary"
}

// ForkShare returns the fraction (0..1] of transcript characters included
// at the resolved cut — the honesty-scaled weight the full-carry estimate
// row uses. 1.0 when the transcript is empty or fully included.
func ForkShare(msgs []models.TranscriptMessage, res ForkResolution) float64 {
	if len(msgs) == 0 || res.ResolvedIndex <= 0 || res.ResolvedIndex >= len(msgs) {
		return 1.0
	}
	total, included := 0, 0
	for i, m := range msgs {
		n := messageWeight(m)
		total += n
		if i < res.ResolvedIndex {
			included += n
		}
	}
	if total == 0 {
		return 1.0
	}
	return float64(included) / float64(total)
}

// messageWeight is the character weight one message contributes to
// ForkShare / Boundary.CumulativeShare: flattened text plus tool-call
// excerpts. One definition so the fork picker's cumulative column and the
// estimate's fork share can never disagree.
func messageWeight(m models.TranscriptMessage) int {
	n := len(m.Text)
	for _, c := range m.ToolCalls {
		n += len(c.InputExcerpt) + len(c.ResultExcerpt)
	}
	return n
}

// boundaryPreviewCap bounds the per-message preview a fork picker shows.
const boundaryPreviewCap = 140

// Boundary is one fork-pickable position of the normalized transcript:
// forking at Index makes that message the LAST included one. Unstable
// positions carry the snap-table reason the picker shows instead of
// silently hiding the row.
type Boundary struct {
	// Index is the 1-based message position (== ForkPoint.MessageIndex).
	Index int                   `json:"index"`
	Role  models.TranscriptRole `json:"role"`
	// Time is zero when the source carries no per-message timestamps.
	Time time.Time `json:"time,omitzero"`
	// Stable reports whether the snap table accepts a cut here; Reason
	// names the rule that refuses it ("" when stable).
	Stable bool   `json:"stable"`
	Reason string `json:"reason,omitempty"`
	// CumulativeShare is the fraction (0..1] of transcript characters
	// included through this message — the same weighting ForkShare uses.
	CumulativeShare float64 `json:"cumulative_share"`
	// Preview is a capped excerpt of the message text (the boundary
	// scrubs it before serving).
	Preview string `json:"preview"`
	// ToolCallCount is the number of tool invocations in the exchange.
	ToolCallCount int `json:"tool_call_count,omitempty"`
}

// Boundaries maps the normalized transcript onto fork-picker rows: every
// message position with its snap-table stability and cumulative character
// share. Nil for an empty transcript.
func Boundaries(msgs []models.TranscriptMessage) []Boundary {
	if len(msgs) == 0 {
		return nil
	}
	total := 0
	for _, m := range msgs {
		total += messageWeight(m)
	}
	out := make([]Boundary, 0, len(msgs))
	cum := 0
	for i, m := range msgs {
		cum += messageWeight(m)
		b := Boundary{
			Index:           i + 1,
			Role:            m.Role,
			Time:            m.Time,
			CumulativeShare: 1.0,
			ToolCallCount:   len(m.ToolCalls),
		}
		if total > 0 {
			b.CumulativeShare = float64(cum) / float64(total)
		}
		b.Preview = previewText(m.Text)
		for _, rule := range snapTable {
			if !rule.matches(m) {
				continue
			}
			b.Stable = rule.accept
			b.Reason = rule.reason
			break
		}
		out = append(out, b)
	}
	return out
}

// previewText caps a message text for the picker row, cutting at the
// first newline so a row stays one line.
func previewText(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > boundaryPreviewCap {
		s = s[:boundaryPreviewCap] + "…"
	}
	return s
}
