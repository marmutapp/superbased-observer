package conversation

import (
	"fmt"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/compression/conversation/types"
)

// Mode selects the enforcer strategy.
const (
	// ModeToken is the default; the enforcer compresses then drops the
	// lowest-scored non-preserved messages until the target is met.
	ModeToken = "token"
	// ModeCache restricts drops to the tail half of the conversation so
	// the prefix stays stable for Anthropic's prefix cache. Compression
	// still applies to the whole window.
	ModeCache = "cache"
)

// BudgetOptions tunes the budget enforcer.
type BudgetOptions struct {
	// TargetRatio is the cap on output bytes expressed as a fraction of
	// the input (0 < r < 1). 0.85 means "compress the conversation to at
	// most 85% of its original size". Matches the
	// [compression.conversation.target_ratio] config knob.
	TargetRatio float64
	// Registry supplies per-type compressors. When nil, only the drop
	// pass runs.
	Registry *Registry
	// CompressTypes is the allow-list of content types eligible for
	// per-type compression, mirroring
	// [compression.conversation.compress_types]. When nil, every type
	// the registry can handle is eligible.
	CompressTypes map[types.ContentType]bool
	// Mode selects the drop strategy (see [ModeToken] / [ModeCache]).
	// Empty string defaults to [ModeToken].
	Mode string
}

// BudgetResult is the output of [Enforce].
type BudgetResult struct {
	// Messages is the enforced message set in original order; dropped
	// runs have been collapsed into single marker messages.
	Messages []Message
	// Stats carries per-turn counters for telemetry.
	Stats BudgetStats
}

// BudgetStats is the counter set surfaced to observer_log and the
// dashboard.
type BudgetStats struct {
	OriginalBytes   int
	CompressedBytes int
	// CompressedCount is the number of messages whose bodies were
	// rewritten by a per-type compressor.
	CompressedCount int
	// DroppedCount is the number of original messages replaced by a
	// marker.
	DroppedCount int
	// MarkerCount is the number of distinct marker messages emitted
	// (consecutive drops collapse into one marker).
	MarkerCount int
	// MessageCount is the length of Messages after enforcement.
	MessageCount int
	// Events is the per-decision detail (one record per compress or
	// drop). Empty when nothing happened. Persisted into
	// compression_events so the dashboard can break down savings by
	// mechanism. Migration 009 introduces the table; pre-migration
	// installs ignore the slice harmlessly.
	Events []Event
}

// Event is one compression decision recorded by the budget enforcer.
// Mechanism is one of types.ContentType.String() ('json', 'code',
// 'logs', 'text', 'diff', 'html') or the string 'drop' for low-
// importance message replacements.
type Event struct {
	Mechanism       string
	OriginalBytes   int
	CompressedBytes int
	MsgIndex        int
	// ImportanceScore is set only for Mechanism == "drop". Zero for
	// per-type compress events.
	ImportanceScore float64
}

// Enforce applies per-type compression and, if still over budget, drops
// the lowest-scored non-preserved messages until the total byte count
// fits under opts.TargetRatio × original. Consecutive drops are replaced
// by a single marker message pointing at search_past_outputs.
//
// The input scored slice is not modified. The returned Messages slice is
// in original order.
func Enforce(scored []ScoredMessage, opts BudgetOptions) BudgetResult {
	if len(scored) == 0 {
		return BudgetResult{}
	}
	mode := opts.Mode
	if mode == "" {
		mode = ModeToken
	}
	ratio := opts.TargetRatio
	if ratio <= 0 || ratio >= 1 {
		ratio = 0.85
	}

	work := make([]ScoredMessage, len(scored))
	copy(work, scored)

	stats := BudgetStats{}
	for _, m := range work {
		stats.OriginalBytes += m.ByteLen
	}
	target := int(float64(stats.OriginalBytes) * ratio)

	// Pass 1 — per-type compression, in order. Operates on work[i] in
	// place; Raw is invalidated on compression so the proxy will
	// re-serialize from Role+Text.
	if opts.Registry != nil {
		for i := range work {
			if work[i].Preserved {
				continue
			}
			before := work[i].ByteLen
			after, ct, ok := compressMessage(&work[i].Message, opts.Registry, opts.CompressTypes)
			if !ok {
				continue
			}
			if after >= before {
				continue
			}
			stats.CompressedCount++
			stats.Events = append(stats.Events, Event{
				Mechanism:       string(ct),
				OriginalBytes:   before,
				CompressedBytes: after,
				MsgIndex:        i,
			})
		}
	}

	stats.CompressedBytes = sumBytes(work)
	if stats.CompressedBytes <= target {
		return BudgetResult{
			Messages: finalize(work),
			Stats:    finalStats(stats, work),
		}
	}

	// Pass 2 — drop lowest-score non-preserved messages until the total
	// fits. In ModeCache, restrict drop candidates to the tail half.
	dropped := make(map[int]bool)
	candidates := dropCandidates(work, mode)
	for _, idx := range candidates {
		if stats.CompressedBytes <= target {
			break
		}
		dropped[idx] = true
		stats.CompressedBytes -= work[idx].ByteLen
		stats.DroppedCount++
		// Record the drop event with the importance score that earned
		// the message its eviction. Compressed bytes for a drop is 0
		// (the message is replaced by a marker — marker bytes count
		// against the post-drop total separately when collapseDropped
		// runs).
		stats.Events = append(stats.Events, Event{
			Mechanism:       "drop",
			OriginalBytes:   work[idx].ByteLen,
			CompressedBytes: 0,
			MsgIndex:        idx,
			ImportanceScore: work[idx].Score,
		})
	}

	final := collapseDropped(work, dropped, &stats)
	stats.MessageCount = len(final)
	return BudgetResult{Messages: final, Stats: stats}
}

// compressMessage runs the per-type compressor matching the message's
// text and replaces Text + ByteLen in place when the compressor shrinks
// the body. Raw is kept pointing at the original bytes — the proxy's
// re-serializer compares the new Text against the parsed original to
// decide whether a tool_result block needs rewriting. Returns the new
// byte length, the content type that fired (for event capture), and
// ok=true when a compressor ran.
func compressMessage(m *Message, r *Registry, allow map[types.ContentType]bool) (int, types.ContentType, bool) {
	if m.Text == "" {
		return m.ByteLen, types.Unknown, false
	}
	ct := types.Detect([]byte(m.Text), "")
	if ct == types.Unknown {
		return m.ByteLen, types.Unknown, false
	}
	if allow != nil && !allow[ct] {
		return m.ByteLen, ct, false
	}
	c, ok := r.Get(ct)
	if !ok {
		return m.ByteLen, ct, false
	}
	out := c.Compress([]byte(m.Text))
	if len(out) >= len(m.Text) {
		return m.ByteLen, ct, false
	}
	// ByteLen tracks serialized size; the compressed Text is the primary
	// size driver for a tool_result, so use the new text length as a
	// conservative proxy.
	m.Text = string(out)
	m.ByteLen = len(out)
	return m.ByteLen, ct, true
}

// dropCandidates returns indexes of messages eligible for dropping,
// ordered by ascending score (lowest first). Preserved messages are
// never included. ModeCache excludes the first half of the conversation
// from drop eligibility.
func dropCandidates(work []ScoredMessage, mode string) []int {
	minIdx := 0
	if mode == ModeCache {
		minIdx = len(work) / 2
	}
	idx := make([]int, 0, len(work))
	for i, m := range work {
		if i < minIdx {
			continue
		}
		if m.Preserved {
			continue
		}
		idx = append(idx, i)
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return work[idx[a]].Score < work[idx[b]].Score
	})
	return idx
}

// collapseDropped replaces each maximal run of consecutive dropped
// indexes with a single marker message. Non-dropped messages pass
// through unchanged.
func collapseDropped(work []ScoredMessage, dropped map[int]bool, stats *BudgetStats) []Message {
	if len(dropped) == 0 {
		return finalize(work)
	}
	out := make([]Message, 0, len(work))
	i := 0
	for i < len(work) {
		if !dropped[i] {
			out = append(out, work[i].Message)
			i++
			continue
		}
		runStart := i
		for i < len(work) && dropped[i] {
			i++
		}
		count := i - runStart
		marker := MarkerMessage(count)
		out = append(out, marker)
		stats.MarkerCount++
		stats.CompressedBytes += marker.ByteLen
	}
	return out
}

// MarkerMessage builds the user-role placeholder that replaces a run of
// dropped messages. The text points the assistant at the MCP tool that
// owns the underlying tool outputs so it can search instead of re-running
// them.
func MarkerMessage(count int) Message {
	text := fmt.Sprintf("[%d messages compressed — use search_past_outputs]", count)
	return Message{
		Role:    RoleUser,
		Text:    text,
		ByteLen: len(text),
	}
}

// finalize returns the raw Message slice from a ScoredMessage slice, in
// the same order.
func finalize(work []ScoredMessage) []Message {
	out := make([]Message, len(work))
	for i, m := range work {
		out[i] = m.Message
	}
	return out
}

// finalStats fills in fields that depend on the final message set.
func finalStats(stats BudgetStats, work []ScoredMessage) BudgetStats {
	stats.MessageCount = len(work)
	return stats
}

// sumBytes totals the ByteLen of a work slice. Used to reflect per-type
// compression into the running CompressedBytes counter.
func sumBytes(work []ScoredMessage) int {
	n := 0
	for _, m := range work {
		n += m.ByteLen
	}
	return n
}
