package conversation

import (
	"fmt"
	"sort"
)

// Aggressive code-collapse (codeintel Phase 2 / Tier A). This is the
// OPT-IN, exact-span-gated counterpart to the default content-preserving
// [CodeCompressor]: it replaces a function/method/type BODY with its
// declaration line plus a retrieval marker, stashing the original so the
// model can fetch it via retrieve_stashed.
//
// ADR-0005 is enforced here and nowhere else: a body is collapsed ONLY
// when its span came from an exact-span backend (CompressorSymbol.Exact).
// A heuristic/approximate span is never collapsed — a wrong span gives
// the model a misleading first view even though the stash makes the
// original retrievable (the v1.4.38/39 regression class). The planning
// step is a pure function so this guarantee is unit-testable in
// isolation.

// defaultCollapseMinLines is the smallest symbol span (inclusive line
// count) worth collapsing. Below this the decl-line + marker is rarely
// smaller than the body, and the loss of a short, glanceable function
// isn't worth a retrieval round-trip.
const defaultCollapseMinLines = 8

// collapseOptions tunes the planner.
type collapseOptions struct {
	// MinLines is the minimum inclusive span length to collapse
	// (defaults to defaultCollapseMinLines when <= 0).
	MinLines int
}

// plannedCollapse is one symbol body the planner decided to collapse.
// StartLine/EndLine are 1-based inclusive (the symbol span). Original is
// the exact byte slice of the full span (decl through close) that gets
// stashed; DeclLine is the symbol's first line, kept inline above the
// marker.
type plannedCollapse struct {
	StartLine int
	EndLine   int
	DeclLine  string
	Original  []byte
}

// planCodeCollapse decides which symbol bodies to collapse in body. It
// is PURE and deterministic: same (body, syms, opts) -> identical plan.
//
// Rules:
//   - only Exact symbols (ADR-0005);
//   - only spans with a real end (EndLine > StartLine) of at least
//     MinLines inclusive lines;
//   - non-overlapping: sorted by start line, a symbol nested inside an
//     already-planned span is skipped (keep the outermost);
//   - spans must fall within the body's line count.
//
// Returns the plans in start-line order. nil when nothing qualifies.
func planCodeCollapse(body []byte, syms []CompressorSymbol, opts collapseOptions) []plannedCollapse {
	minLines := opts.MinLines
	if minLines <= 0 {
		minLines = defaultCollapseMinLines
	}
	if len(body) == 0 || len(syms) == 0 {
		return nil
	}
	lines := splitLines(body)
	n := len(lines)

	// Sort a copy by start line (then end line desc so the outermost of
	// two same-start spans wins) for deterministic, overlap-free output.
	cand := make([]CompressorSymbol, 0, len(syms))
	for _, s := range syms {
		if !s.Exact {
			continue // ADR-0005: never collapse an approximate span
		}
		if s.StartLine < 1 || s.EndLine <= s.StartLine || s.EndLine > n {
			continue
		}
		if s.EndLine-s.StartLine+1 < minLines {
			continue
		}
		cand = append(cand, s)
	}
	if len(cand) == 0 {
		return nil
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].StartLine != cand[j].StartLine {
			return cand[i].StartLine < cand[j].StartLine
		}
		return cand[i].EndLine > cand[j].EndLine
	})

	var plans []plannedCollapse
	coveredThrough := 0 // highest end-line already planned (1-based)
	for _, s := range cand {
		if s.StartLine <= coveredThrough {
			continue // nested inside / overlapping an existing plan
		}
		original := joinLineRange(lines, s.StartLine, s.EndLine, endsWithNewline(body))
		plans = append(plans, plannedCollapse{
			StartLine: s.StartLine,
			EndLine:   s.EndLine,
			DeclLine:  lines[s.StartLine-1],
			Original:  original,
		})
		coveredThrough = s.EndLine
	}
	return plans
}

// applyCodeCollapse rebuilds body with each planned span replaced by its
// declaration line + a retrieval marker, stashing the original via write.
// Returns the collapsed body, the per-collapse events, and whether
// anything fired. write maps an original body slice -> stash sha. A
// write error skips that one collapse (best-effort, never fatal).
//
// The marker is a pure function of (hiddenLineCount, sha): same body
// bytes -> same sha -> byte-identical marker across turns, so the
// proxy's prefix cache stays intact.
func applyCodeCollapse(body []byte, plans []plannedCollapse, msgIndex int, write func([]byte) (string, error)) ([]byte, []Event, bool) {
	if len(plans) == 0 || write == nil {
		return body, nil, false
	}
	lines := splitLines(body)
	var out []string
	var events []Event
	fired := false
	cursor := 0 // 0-based index of the next not-yet-emitted line
	for _, p := range plans {
		start0 := p.StartLine - 1
		end0 := p.EndLine - 1
		if start0 < cursor || end0 >= len(lines) {
			continue // defensive: stale/overlapping plan
		}
		sha, err := write(p.Original)
		if err != nil {
			continue
		}
		hidden := p.EndLine - p.StartLine // interior lines hidden (decl kept)
		// A wired stash returns a sha pointing at the elided body; a
		// stash-less collapse (codex-safe) returns "" and gets the
		// re-read marker instead of a dangling retrieve_stashed pointer.
		marker := formatCollapseMarker(hidden, sha)
		if sha == "" {
			marker = formatCollapseMarkerNoStash(hidden)
		}
		if len(marker)+len(p.DeclLine) >= len(p.Original) {
			continue // not a win — leave the body intact for this span
		}
		// Emit untouched lines before the span, then decl + marker.
		out = append(out, lines[cursor:start0]...)
		out = append(out, p.DeclLine, marker)
		cursor = end0 + 1
		fired = true
		events = append(events, Event{
			Mechanism:       "code_collapse",
			OriginalBytes:   len(p.Original),
			CompressedBytes: len(p.DeclLine) + len(marker) + 2,
			MsgIndex:        msgIndex,
			BodyHash:        bodyHashHex(string(p.Original)),
		})
	}
	if !fired {
		return body, nil, false
	}
	out = append(out, lines[cursor:]...)
	return joinLines(out, endsWithNewline(body)), events, true
}

// previewCollapseEvents builds the telemetry events for a preview-only
// run: it reports what WOULD collapse (mechanism "code_collapse_preview")
// without stashing or changing any body. CompressedBytes is the marker-
// sized estimate so savings reports are representative.
func previewCollapseEvents(plans []plannedCollapse, msgIndex int) []Event {
	if len(plans) == 0 {
		return nil
	}
	events := make([]Event, 0, len(plans))
	for _, p := range plans {
		events = append(events, Event{
			Mechanism:       "code_collapse_preview",
			OriginalBytes:   len(p.Original),
			CompressedBytes: len(p.DeclLine) + 80, // approx decl + marker
			MsgIndex:        msgIndex,
			BodyHash:        bodyHashHex(string(p.Original)),
		})
	}
	return events
}

// formatCollapseMarker is the canonical collapsed-body marker. Directive
// phrasing + the explicit MCP tool name + sha argument, mirroring
// [formatStashMarker], so the model recognises it as an actionable
// retrieval template rather than a status note.
func formatCollapseMarker(hiddenLines int, sha string) string {
	if hiddenLines < 1 {
		hiddenLines = 1
	}
	return fmt.Sprintf(
		"    // [%d lines collapsed → observer://stash/%s — to view the full body, call mcp__observer__retrieve_stashed with sha=\"%s\"]",
		hiddenLines, sha, sha,
	)
}

// formatCollapseMarkerNoStash is the collapsed-body marker used when no
// stash is wired (e.g. the codex-safe profile, which pins stash off for
// OpenAI implicit-cache reasons). With no stash there is no sha to
// retrieve, so the marker tells the model to re-read the file instead of
// pointing at a dangling retrieve_stashed pointer. It is a pure function
// of the hidden line count, so it stays byte-identical across turns and
// never disturbs the provider's prefix cache.
func formatCollapseMarkerNoStash(hiddenLines int) string {
	if hiddenLines < 1 {
		hiddenLines = 1
	}
	return fmt.Sprintf(
		"    // [%d lines collapsed by observer — re-read this file to view the full body]",
		hiddenLines,
	)
}

// joinLineRange returns the exact bytes of lines [start..end] (1-based
// inclusive), preserving the body's newline convention so the stashed
// original round-trips faithfully.
func joinLineRange(lines []string, start, end int, trailingNewline bool) []byte {
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	seg := lines[start-1 : end]
	// Within-body ranges always end with a newline (a line precedes the
	// next); only honour trailingNewline when the range reaches the EOF.
	tn := true
	if end == len(lines) {
		tn = trailingNewline
	}
	return joinLines(seg, tn)
}
