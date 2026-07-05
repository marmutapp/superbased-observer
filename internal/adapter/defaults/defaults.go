// Package defaults exposes the canonical set of session-file adapters
// the observer registers in production. Pulling this out of
// cmd/observer/main.go lets tests in internal/adapter/ and
// internal/watcher/ assemble the same adapter set the production
// binary uses — needed for the all-adapters IsSessionFile invariant
// test and the multi-adapter watcher regression test that pins the
// poller's dispatch rule.
//
// The sub-package shape is intentional: putting this list in
// internal/adapter directly would create an import cycle (each
// adapter package imports internal/adapter for the Adapter
// interface).
package defaults

import (
	"github.com/marmutapp/superbased-observer/internal/adapter"
	"github.com/marmutapp/superbased-observer/internal/adapter/antigravity"
	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/adapter/cline"
	"github.com/marmutapp/superbased-observer/internal/adapter/clinecli"
	"github.com/marmutapp/superbased-observer/internal/adapter/codex"
	"github.com/marmutapp/superbased-observer/internal/adapter/copilot"
	"github.com/marmutapp/superbased-observer/internal/adapter/copilotcli"
	"github.com/marmutapp/superbased-observer/internal/adapter/cowork"
	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/adapter/gemini"
	"github.com/marmutapp/superbased-observer/internal/adapter/hermes"
	"github.com/marmutapp/superbased-observer/internal/adapter/kilocode"
	"github.com/marmutapp/superbased-observer/internal/adapter/openclaw"
	"github.com/marmutapp/superbased-observer/internal/adapter/opencode"
	"github.com/marmutapp/superbased-observer/internal/adapter/pi"
)

// Adapters returns the canonical set of session-file adapters with
// zero-value defaults. Callers that need runtime config (e.g.
// antigravity.WithNetworkRecovery, cursor.WithSessionHookChecker)
// apply it after this returns by type-asserting individual adapters.
//
// Used by:
//   - cmd/observer/main.go::buildWatcher  (registers each into the
//     watcher Registry)
//   - cmd/observer/main.go::recognizesSessionFile  (composes a single
//     IsSessionFile predicate for the dashboard's health endpoint
//     orphan filter)
//   - internal/adapter/defaults/defaults_test.go  (invariants — every
//     adapter must require under-WatchPaths in its IsSessionFile, and
//     no two adapters' watch roots share a prefix)
func Adapters() []adapter.Adapter {
	return []adapter.Adapter{
		claudecode.New(),
		codex.New(),
		cline.New(),
		clinecli.New(),
		copilot.New(),
		copilotcli.New(),
		cowork.New(),
		cursor.New(),
		openclaw.New(),
		opencode.New(),
		pi.New(),
		gemini.New(),
		antigravity.New(),
		hermes.New(),
		kilocode.NewLegacy(),
		kilocode.NewCLI(),
	}
}
