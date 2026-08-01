package oneshot

import (
	"fmt"
	"sort"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// maxGapNotes caps how many per-tool "known capture gap" notes Notes emits,
// so a corpus touching many tools with a documented Gap does not drown the
// footer in per-tool trivia (plan §1.4 keeps the footer short).
const maxGapNotes = 2

// State carries the scan/cost-engine facts Notes needs beyond the
// integration registry itself. It is the same information already present
// on Table (see table.go) — cmd/observer/usage.go builds one State
// alongside the Table it passes to Render, and passes both to Notes.
type State struct {
	// UnknownModelCount / UnpricedTokens mirror cost.Summary: how many
	// distinct models had no pricing entry, and how much token volume they
	// represent. UnknownModelCount == 0 suppresses the unpriced-models note.
	UnknownModelCount int
	UnpricedTokens    int64

	// Partial is non-nil when the scan was cut short by its wall-clock
	// budget — mirrors Table.Partial.
	Partial *PartialScan

	// Empty is non-nil when the scan found zero rows anywhere in the
	// window — mirrors Table.Empty.
	Empty *EmptyCorpus
}

// Notes derives the honesty-footer lines for a one-shot report. It looks at
// three things only: the registry rows for the tools actually seen in this
// scan (caps, pre-filtered or not — Notes matches by Capability.Tool
// against toolsSeen itself), the list of tools seen, and the scan/cost
// facts in s. Every note's text is either taken verbatim from a
// Capability field (never a hardcoded per-tool phrase — CLAUDE.md #3) or
// is a generic, tool-agnostic sentence driven by counts in s.
//
// Notes returned, in order:
//
//  1. One "no_local_token_source" note per seen tool whose registry row has
//     TokenTier.Best == "none" — the tool is confirmed to have no local
//     token/cost data at all.
//  2. Up to maxGapNotes "gap" notes, one per seen tool whose registry row
//     has a non-empty TokenTier.Gap (and Best != "none", already covered by
//     rule 1) — the tool has SOME local capture but a known hole in it.
//  3. An "unpriced_models" note when s.UnknownModelCount > 0.
//  4. A "partial" note when s.Partial != nil.
//  5. An "empty_state" note when s.Empty != nil.
//
// A tool in toolsSeen with no matching Capability row (not yet registered,
// or a typo'd name) is silently skipped — Notes never panics or fabricates
// a row for it.
func Notes(caps []integration.Capability, toolsSeen []string, s State) []Note {
	byTool := make(map[string]integration.Capability, len(caps))
	for _, c := range caps {
		byTool[c.Tool] = c
	}

	seen := make([]string, len(toolsSeen))
	copy(seen, toolsSeen)
	sort.Strings(seen)

	var notes []Note

	for _, tool := range seen {
		c, ok := byTool[tool]
		if !ok || c.TokenTier.Best != "none" {
			continue
		}
		notes = append(notes, Note{
			Code:  "no_local_token_source",
			Tools: []string{tool},
			Text:  fmt.Sprintf("%s: no local token source — %s", tool, c.TokenTier.Gap),
		})
	}

	gapCount := 0
	for _, tool := range seen {
		if gapCount >= maxGapNotes {
			break
		}
		c, ok := byTool[tool]
		if !ok || c.TokenTier.Best == "none" || c.TokenTier.Gap == "" {
			continue
		}
		notes = append(notes, Note{
			Code:  "gap",
			Tools: []string{tool},
			Text:  fmt.Sprintf("%s: %s", tool, c.TokenTier.Gap),
		})
		gapCount++
	}

	if s.UnknownModelCount > 0 {
		var text string
		if s.UnknownModelCount == 1 {
			text = "1 model has no pricing entry — its tokens are excluded from the dollar totals"
		} else {
			text = fmt.Sprintf("%d models have no pricing entry — their tokens are excluded from the dollar totals", s.UnknownModelCount)
		}
		notes = append(notes, Note{Code: "unpriced_models", Text: text})
	}

	if s.Partial != nil {
		notes = append(notes, Note{
			Code: "partial",
			Text: fmt.Sprintf("scan stopped at the %s budget — read %d of %d session files, so totals are a partial view",
				s.Partial.Budget, s.Partial.FilesWalked, s.Partial.FilesTotal),
		})
	}

	if s.Empty != nil {
		notes = append(notes, Note{
			Code: "empty_state",
			Text: fmt.Sprintf("no session activity found under %s across %d checked location(s) (%d adapters checked) — run an AI coding tool first, or widen --since",
				s.Empty.Home, len(s.Empty.Looked), s.Empty.AdapterCount),
		})
	}

	return notes
}
