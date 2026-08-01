package oneshot

import (
	"strings"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// syntheticCaps builds one Capability row per TokenTier.Best value the
// registry actually uses (plan §0's grounded list), plus a Gap-carrying
// row and a no-gap row, so notes_test exercises every branch Notes has to
// decide on.
func syntheticCaps() []integration.Capability {
	return []integration.Capability{
		{Tool: "claude-code", TokenTier: integration.TokenTier{Best: "proxy"}},
		{Tool: "codex", TokenTier: integration.TokenTier{Best: "proxy"}},
		{Tool: "cursor", TokenTier: integration.TokenTier{Best: "sqlite"}},
		{Tool: "aider", TokenTier: integration.TokenTier{Best: "transcript", Gap: "prose tokens are gross, netted against cache reads"}},
		{Tool: "copilot-cli", TokenTier: integration.TokenTier{Best: "events_jsonl", Gap: "per-turn input/cache attribution needs --log-level debug"}},
		{Tool: "gemini-cli", TokenTier: integration.TokenTier{Best: "jsonl"}},
		{Tool: "antigravity", TokenTier: integration.TokenTier{Best: "proto", Gap: "desktop/.pb path still decrypt-gated"}},
		{Tool: "chatgpt-web", TokenTier: integration.TokenTier{Best: "browser_extension"}},
		{Tool: "qoder", TokenTier: integration.TokenTier{Best: "none", Gap: "usage server-side only — local logs carry zero tokens and no model; no base-URL knob"}},
	}
}

func TestNotesNoLocalTokenSource(t *testing.T) {
	notes := Notes(syntheticCaps(), []string{"qoder", "claude-code"}, State{})

	found := false
	for _, n := range notes {
		if n.Code == "no_local_token_source" {
			found = true
			if len(n.Tools) != 1 || n.Tools[0] != "qoder" {
				t.Errorf("no_local_token_source note Tools = %v, want [qoder]", n.Tools)
			}
			if !strings.Contains(n.Text, "qoder") {
				t.Errorf("note text %q does not mention qoder", n.Text)
			}
			if !strings.Contains(n.Text, "usage server-side only") {
				t.Errorf("note text %q does not carry the registry's Gap verbatim", n.Text)
			}
		}
		if n.Code == "no_local_token_source" && n.Tools[0] == "claude-code" {
			t.Errorf("claude-code (TokenTier.Best=proxy) must never get a no_local_token_source note")
		}
	}
	if !found {
		t.Fatal("expected a no_local_token_source note for qoder, got none")
	}
}

func TestNotesGapCappedAtTwo(t *testing.T) {
	seen := []string{"aider", "copilot-cli", "antigravity"}
	notes := Notes(syntheticCaps(), seen, State{})

	gapCount := 0
	for _, n := range notes {
		if n.Code == "gap" {
			gapCount++
		}
	}
	if gapCount != maxGapNotes {
		t.Errorf("gap note count = %d, want %d (maxGapNotes)", gapCount, maxGapNotes)
	}
}

func TestNotesGapSkipsNoneAndNoGapTools(t *testing.T) {
	// qoder (Best=none) must not ALSO produce a "gap" note (it already got
	// the stronger no_local_token_source note); cursor/gemini-cli/claude-code
	// carry no Gap at all and must produce neither note type.
	notes := Notes(syntheticCaps(), []string{"qoder", "cursor", "gemini-cli", "claude-code"}, State{})
	for _, n := range notes {
		if n.Code == "gap" {
			t.Errorf("unexpected gap note for a tool with no TokenTier.Gap or Best==none: %+v", n)
		}
	}
}

func TestNotesUnpricedModels(t *testing.T) {
	t.Run("zero suppressed", func(t *testing.T) {
		notes := Notes(nil, nil, State{UnknownModelCount: 0})
		for _, n := range notes {
			if n.Code == "unpriced_models" {
				t.Errorf("unpriced_models note present with UnknownModelCount=0")
			}
		}
	})

	t.Run("singular", func(t *testing.T) {
		notes := Notes(nil, nil, State{UnknownModelCount: 1})
		text := findNote(t, notes, "unpriced_models").Text
		if !strings.Contains(text, "1 model has") {
			t.Errorf("singular unpriced_models text = %q", text)
		}
	})

	t.Run("plural", func(t *testing.T) {
		notes := Notes(nil, nil, State{UnknownModelCount: 3})
		text := findNote(t, notes, "unpriced_models").Text
		if !strings.Contains(text, "3 models have") {
			t.Errorf("plural unpriced_models text = %q", text)
		}
	})
}

func TestNotesPartial(t *testing.T) {
	notes := Notes(nil, nil, State{Partial: &PartialScan{Budget: "30s", FilesWalked: 12, FilesTotal: 40}})
	text := findNote(t, notes, "partial").Text
	for _, want := range []string{"30s", "12", "40"} {
		if !strings.Contains(text, want) {
			t.Errorf("partial note text %q missing %q", text, want)
		}
	}
}

func TestNotesEmptyState(t *testing.T) {
	notes := Notes(nil, nil, State{Empty: &EmptyCorpus{Home: "$HOME", Looked: []string{"~/.claude/projects", "~/.codex/sessions"}, AdapterCount: 29}})
	text := findNote(t, notes, "empty_state").Text
	for _, want := range []string{"$HOME", "29"} {
		if !strings.Contains(text, want) {
			t.Errorf("empty_state note text %q missing %q", text, want)
		}
	}
}

func TestNotesUnknownToolSkippedSilently(t *testing.T) {
	// A tool seen in the scan but absent from the registry (typo, or a
	// brand-new adapter not yet registered) must never panic and must
	// never fabricate a note.
	notes := Notes(syntheticCaps(), []string{"not-a-real-tool"}, State{})
	for _, n := range notes {
		if len(n.Tools) == 1 && n.Tools[0] == "not-a-real-tool" {
			t.Errorf("fabricated a note for an unregistered tool: %+v", n)
		}
	}
}

func findNote(t *testing.T, notes []Note, code string) Note {
	t.Helper()
	for _, n := range notes {
		if n.Code == code {
			return n
		}
	}
	t.Fatalf("no note with code %q in %+v", code, notes)
	return Note{}
}
