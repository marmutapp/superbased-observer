package integration

import (
	"strings"
	"testing"
)

// TestHeadlessGroundedSet pins the exact set of registry rows carrying a
// live-verified headless one-shot contract (Capability.Headless). A row is
// added here ONLY after a real drive proves the argv — the same grounding
// rule LaunchSpec rows follow. Adding a tool to the arena means adding it to
// this pin AND landing the drive evidence, so the set can never grow by
// accident.
func TestHeadlessGroundedSet(t *testing.T) {
	want := map[string]bool{
		"claude-code": true,
		"codex":       true,
		"grok":        true, // 2026-08-22: -p + --output-format json --always-approve (file edit + usage envelope)
		"opencode":    true, // 2026-08-22: run <prompt> --format json (file edit + sessionID events)
		"aider":       true, // 2026-08-22: --message --yes --no-auto-commits (file edit + Tokens/Cost prose)
	}
	for _, c := range Capabilities() {
		if want[c.Tool] && c.Headless == nil {
			t.Errorf("tool %q: expected grounded Headless row, found nil", c.Tool)
			continue
		}
		if !want[c.Tool] && c.Headless != nil {
			t.Errorf("tool %q: Headless declared but not in the grounded pin — verify a live one-shot drive first, then extend this test", c.Tool)
		}
		if c.Headless == nil {
			continue
		}
		// Shape invariants: a grounded headless row must have binary
		// resolution (the arena drives the real binary directly, not a
		// launcher) and must name a result extraction contract.
		if c.Binary == nil {
			t.Errorf("tool %q: Headless without Binary resolution", c.Tool)
		}
		if len(c.Headless.OutputArgs) == 0 {
			t.Errorf("tool %q: headless row without OutputArgs (every grounded lane needs its machine-readable/approval flags)", c.Tool)
		}
		if c.Headless.PromptFlag == "" && len(c.Headless.Lead) == 0 {
			t.Errorf("tool %q: headless spec has neither PromptFlag nor Lead — prompt delivery undefined", c.Tool)
		}
		switch c.Headless.Result {
		case HeadlessResultStdoutJSON, HeadlessResultGrokJSON,
			HeadlessResultOpenCodeEvents, HeadlessResultStdoutText:
			// extraction contracts implemented in internal/arena/driver.go
		case HeadlessResultOutputFile:
			if c.Headless.ResultFlag == "" {
				t.Errorf("tool %q: output_file result requires ResultFlag", c.Tool)
			}
		default:
			t.Errorf("tool %q: Headless.Result unset or unknown (%q)", c.Tool, c.Headless.Result)
		}
		switch c.Headless.ContextMode {
		case HeadlessContextNone:
			if c.Tool == "aider" {
				t.Errorf("tool %q: aider must declare its grounded positional context-file argv", c.Tool)
			}
		case HeadlessContextPositional:
			if c.Tool != "aider" {
				t.Errorf("tool %q: positional context-file argv is grounded only for aider", c.Tool)
			}
		default:
			t.Errorf("tool %q: unknown Headless.ContextMode %q", c.Tool, c.Headless.ContextMode)
		}
		if c.Headless.ProxyModelPrefix != "" {
			if c.Headless.ProxyDefaultModel == "" {
				t.Errorf("tool %q: ProxyModelPrefix requires ProxyDefaultModel", c.Tool)
			} else if !strings.HasPrefix(c.Headless.ProxyDefaultModel, c.Headless.ProxyModelPrefix) {
				t.Errorf("tool %q: proxy default model %q does not match prefix %q", c.Tool, c.Headless.ProxyDefaultModel, c.Headless.ProxyModelPrefix)
			}
		}
		if c.Tool == "opencode" {
			if c.Headless.ProxyModelPrefix != "openrouter/" || c.Headless.ProxyDefaultModel != "openrouter/stealth/ox-alpha" {
				t.Errorf("tool %q: routed model contract drifted: prefix=%q default=%q", c.Tool, c.Headless.ProxyModelPrefix, c.Headless.ProxyDefaultModel)
			}
		} else if c.Headless.ProxyModelPrefix != "" || c.Headless.ProxyDefaultModel != "" {
			t.Errorf("tool %q: provider-scoped proxy model is grounded only for opencode", c.Tool)
		}
	}
}
