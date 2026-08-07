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
	"github.com/marmutapp/superbased-observer/internal/adapter/aider"
	"github.com/marmutapp/superbased-observer/internal/adapter/antigravity"
	"github.com/marmutapp/superbased-observer/internal/adapter/browserchat"
	"github.com/marmutapp/superbased-observer/internal/adapter/claudecode"
	"github.com/marmutapp/superbased-observer/internal/adapter/cline"
	"github.com/marmutapp/superbased-observer/internal/adapter/clinecli"
	"github.com/marmutapp/superbased-observer/internal/adapter/codex"
	"github.com/marmutapp/superbased-observer/internal/adapter/commandcode"
	"github.com/marmutapp/superbased-observer/internal/adapter/copilot"
	"github.com/marmutapp/superbased-observer/internal/adapter/copilotcli"
	"github.com/marmutapp/superbased-observer/internal/adapter/cowork"
	"github.com/marmutapp/superbased-observer/internal/adapter/crush"
	"github.com/marmutapp/superbased-observer/internal/adapter/cursor"
	"github.com/marmutapp/superbased-observer/internal/adapter/devin"
	"github.com/marmutapp/superbased-observer/internal/adapter/droid"
	"github.com/marmutapp/superbased-observer/internal/adapter/gemini"
	"github.com/marmutapp/superbased-observer/internal/adapter/goose"
	"github.com/marmutapp/superbased-observer/internal/adapter/grok"
	"github.com/marmutapp/superbased-observer/internal/adapter/hermes"
	"github.com/marmutapp/superbased-observer/internal/adapter/kilocode"
	"github.com/marmutapp/superbased-observer/internal/adapter/kimicode"
	"github.com/marmutapp/superbased-observer/internal/adapter/kirocli"
	"github.com/marmutapp/superbased-observer/internal/adapter/muse"
	"github.com/marmutapp/superbased-observer/internal/adapter/openclaw"
	"github.com/marmutapp/superbased-observer/internal/adapter/opencode"
	"github.com/marmutapp/superbased-observer/internal/adapter/pi"
	"github.com/marmutapp/superbased-observer/internal/adapter/primeagent"
	"github.com/marmutapp/superbased-observer/internal/adapter/qoder"
	"github.com/marmutapp/superbased-observer/internal/adapter/qwencode"
	"github.com/marmutapp/superbased-observer/internal/models"
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
		antigravity.NewCLI(),
		hermes.New(),
		kilocode.NewLegacy(),
		kilocode.NewCLI(),
		qwencode.New(),
		kirocli.New(),
		crush.New(),
		kimicode.New(),
		grok.New(),
		devin.New(),
		qoder.New(),
		aider.New(),
		goose.New(),
		// 2026-07-29 wave. droid + command-code have their own parser
		// packages; open-interpreter is a rebadged Codex CLI Rust build,
		// so it reuses the codex parser and re-tags every emitted event
		// at the single boundary seam (the §2.1 variant-adapter pattern,
		// like antigravity.NewCLI) rather than forking a package.
		droid.New(),
		codex.NewOpenInterpreter(),
		commandcode.New(),
		// 2026-08-06. Meta's Muse Code CLI: an event-sourced session log
		// under a date-sharded sessions/YYYY/MM/DD/<uuid>/ tree (the same
		// shape codex uses), plus child-agent logs one level deeper that
		// carry tokens the parent log does not.
		muse.New(),
		// 2026-08-06. Prime Intellect's Prime Agent CLI: a hard fork of
		// the same pi-mono upstream pi.New() covers, but with its own data
		// home (~/.prime/agent), a FLAT sessions/<uuid>.jsonl layout, an
		// extended entry vocabulary (compaction / child_usage_attributed /
		// agent_status …) and a single built-in tool. Its own parser
		// rather than a §2.1 boundary retag — see
		// docs/plans/prime-agent-adapter-plan-2026-08-06.md §1.
		primeagent.New(),
		// Browser-chatbot rail. Hook-only: no WatchPaths (capture arrives
		// via the browser extension's native-messaging bridge / loopback
		// listener, not the file watcher). One adapter per *-web site; the
		// site is a DATA discriminator inside browserchat, so these share a
		// single package + normalizer. Registered here so the integration-
		// registry coverage guardrail is satisfied and each tool name is
		// declared.
		browserchat.New(), // chatgpt-web
		browserchat.NewFor(models.ToolClaudeWeb),
		browserchat.NewFor(models.ToolPerplexityWeb),
		browserchat.NewFor(models.ToolGeminiWeb),
		browserchat.NewFor(models.ToolCopilotWeb),
	}
}
