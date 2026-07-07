package guard

import (
	"testing"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/policy"
)

// TestConformanceMatrix pins the §6.5 table's structural invariants:
// every enabled adapter has a watcher row (full coverage, no silent
// gaps — F2 is made visible, never hidden), block-capable channels
// are exactly the documented ones (Q4: never assume deny semantics),
// and lookups behave.
func TestConformanceMatrix(t *testing.T) {
	t.Parallel()
	matrix := ConformanceMatrix()

	// Every default-enabled adapter must have a watcher row. The
	// EnabledAdapters default list is the coverage contract.
	for _, client := range config.Default().Observer.Watch.EnabledAdapters {
		caps, ok := CapabilitiesFor(client, ChannelWatcher)
		if !ok {
			t.Errorf("adapter %q has no watcher row — coverage gap would be silent", client)
			continue
		}
		if caps.PreExecution || caps.CanBlock || caps.CanAsk {
			t.Errorf("adapter %q watcher row claims pre-execution capabilities: %+v", client, caps)
		}
	}

	// Block-capable channels are EXACTLY the documented set.
	var blockers []string
	for _, e := range matrix {
		if e.Caps.CanBlock {
			blockers = append(blockers, e.Client+"/"+e.Channel)
		}
	}
	want := map[string]bool{
		models.ToolClaudeCode + "/hook:PreToolUse":       true,
		models.ToolCursor + "/hook:beforeShellExecution": true,
		models.ToolCursor + "/hook:beforeMCPExecution":   true,
		models.ToolCursor + "/hook:beforeReadFile":       true,
	}
	if len(blockers) != len(want) {
		t.Errorf("block-capable channels = %v, want exactly %d documented ones", blockers, len(want))
	}
	for _, b := range blockers {
		if !want[b] {
			t.Errorf("undocumented block-capable channel %q (Q4: deny semantics must be documented before CanBlock)", b)
		}
	}

	// Observe-only hook channels exist for codex + hermes (they have
	// receivers but no documented deny path).
	for _, c := range []struct{ client, channel string }{
		{models.ToolCodex, "hook:notify"},
		{models.ToolHermes, "hook:plugin"},
	} {
		caps, ok := CapabilitiesFor(c.client, c.channel)
		if !ok || caps.CanBlock {
			t.Errorf("%s %s = (%+v, %v), want observe-only row", c.client, c.channel, caps, ok)
		}
	}

	// Unknown lookups: zero caps, ok=false — callers treat unknown as
	// observe-only, never blockable.
	if caps, ok := CapabilitiesFor("mystery-tool", "hook:anything"); ok || caps.CanBlock {
		t.Errorf("unknown channel lookup = (%+v, %v), want zero/false", caps, ok)
	}

	// Notes are mandatory — the dashboard renders them; an empty note
	// is a row nobody can interpret.
	for _, e := range matrix {
		if e.Notes == "" {
			t.Errorf("%s/%s has no Notes", e.Client, e.Channel)
		}
	}
}

// TestClassifyActionType pins the exported boundary classification
// against the ingest seam's vocabulary.
func TestClassifyActionType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want policy.EventKind
		ok   bool
	}{
		{models.ActionRunCommand, policy.KindShellExec, true},
		{models.ActionReadFile, policy.KindFileAccess, true},
		{models.ActionMCPCall, policy.KindMCPCall, true},
		{models.ActionConfigChange, policy.KindConfigChange, true},
		{models.ActionWebFetch, policy.KindToolCall, true},
		{models.ActionUserPrompt, "", false},
		{models.ActionSessionEnd, "", false},
	}
	for _, tc := range cases {
		kind, ok := ClassifyActionType(tc.in)
		if kind != tc.want || ok != tc.ok {
			t.Errorf("ClassifyActionType(%s) = (%s, %v), want (%s, %v)", tc.in, kind, ok, tc.want, tc.ok)
		}
	}
}
