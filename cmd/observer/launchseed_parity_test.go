// launchseed_parity_test.go — pins the WS-SEED drift class from
// docs/plans/adapter-parity-audit-2026-08-25.md §1: every integration
// registry row that declares a LaunchSeeded or LaunchDocAssisted
// Handoff.Launch, AND whose launcher subcommand is actually registered on
// the real cobra tree, must expose a --continue-from flag on that
// subcommand. A row losing its flag (or a launcher silently regressing to
// the unseeded runSeedOnlyLaunch/no-continue-from shape) fails loud here
// instead of surfacing as a dashboard handoff tip that dead-ends on an
// unknown flag.

package main

import (
	"testing"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/integration"
)

// TestEveryLaunchSeededOrDocAssistedSubcommandHasContinueFrom walks
// integration.Tools()/integration.For(...) (the canonical, closed tool
// vocabulary) rather than integration.Capabilities() directly, so the table
// is driven off the same lookup surface a caller resolving a single tool
// would use.
func TestEveryLaunchSeededOrDocAssistedSubcommandHasContinueFrom(t *testing.T) {
	root := newRootCmd()
	byName := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		byName[c.Name()] = c
	}

	type row struct {
		tool string
		verb string
		mode integration.LaunchMode
		cmd  *cobra.Command
	}
	var rows []row
	for _, tool := range integration.Tools() {
		capab, ok := integration.For(tool)
		if !ok || !capab.Handoff.Launchable() {
			continue // no CLI launch surface declared for this tool at all
		}
		mode := capab.Handoff.Launch.Mode
		if mode != integration.LaunchSeeded && mode != integration.LaunchDocAssisted {
			// Some other launch shape (e.g. a future mode this drift class
			// doesn't cover yet) — out of scope for this test, skipped via
			// the capability shape, never a tool-name list.
			continue
		}
		verb := capab.Handoff.Launch.Subcommand
		cmd, ok := byName[verb]
		if !ok {
			// The registry row names a subcommand that isn't wired onto the
			// real cobra tree at all — e.g. a desktop-only tool with no CLI
			// launcher. That's a DIFFERENT drift class (covered by
			// TestEveryLaunchCapabilityIsRegisteredOnRoot in
			// attach_launchers_matrix_test.go for LaunchSeeded rows), so this
			// test skips it rather than re-litigating it here.
			continue
		}
		rows = append(rows, row{tool: tool, verb: verb, mode: mode, cmd: cmd})
	}

	if len(rows) == 0 {
		t.Fatal("no LaunchSeeded/LaunchDocAssisted subcommand rows resolved on the root tree — the test premise is unverifiable (registry or root wiring regressed)")
	}

	for _, r := range rows {
		r := r
		t.Run(r.verb, func(t *testing.T) {
			if got := r.cmd.Flags().Lookup("continue-from"); got == nil {
				t.Fatalf("tool %q (verb %q, LaunchMode %v) registers no --continue-from flag on its cobra command — the WS-SEED / doc-assisted drift class documented in docs/plans/adapter-parity-audit-2026-08-25.md §1", r.tool, r.verb, r.mode)
			}
		})
	}
}
