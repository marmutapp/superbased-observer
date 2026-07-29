package codex

import (
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/scrub"
)

// NewOpenInterpreter returns an Adapter instance retagged for Open
// Interpreter (models.ToolOpenInterpreter), a rebrand of the OpenAI
// Codex CLI Rust codebase installed under ~/.openinterpreter instead
// of ~/.codex — see the models.ToolOpenInterpreter doc comment and
// docs/openinterpreter-adapter.md for the full evidence trail.
//
// The wire format (rollout-*.jsonl under sessions/YYYY/MM/DD/,
// session_meta / event_msg / response_item / turn_context /
// world_state event types, token_count field names) is byte-identical
// to codex's — this instance reuses the entire codex parser
// unmodified (parseSessionFile) and only differs in:
//   - WatchPaths: ".openinterpreter/sessions" instead of
//     ".codex/sessions" under every cross-mount-resolved $HOME, and
//     INTERPRETER_HOME instead of CODEX_HOME for a single explicit
//     override root.
//   - Tool tagging: every ToolEvent/TokenEvent this instance emits is
//     retagged models.ToolOpenInterpreter by the ParseSessionFile
//     wrapper's single retag seam (see adapter.go).
//
// INTERPRETER_HOME is UNVERIFIED as an actual environment-variable
// read (docs/plans/openinterpreter-adapter-plan-2026-07-29.md §12.1):
// `strings` on the interpreter binary shows the token exists
// alongside ".openinterpreter", but no live smoke test has confirmed
// the fork reads it from the environment the way CODEX_HOME is read
// by codex. It's wired here on the CODEX_HOME precedent (fail-open —
// an operator who sets it and is wrong just gets the default
// ~/.openinterpreter root back, same as an unset env var); flag this
// comment if a future session confirms or refutes it.
func NewOpenInterpreter() *Adapter {
	return &Adapter{
		scrubber:    scrub.New(),
		name:        models.ToolOpenInterpreter,
		homeEnvVar:  "INTERPRETER_HOME",
		homeDirName: ".openinterpreter",
	}
}

// NewOpenInterpreterWithOptions customizes the scrubber and/or watch
// root for the Open Interpreter variant (test/backfill use — mirrors
// codex's NewWithOptions). Pass "" watchRoot for platform-default
// cross-mount discovery under ".openinterpreter".
func NewOpenInterpreterWithOptions(s *scrub.Scrubber, watchRoot string) *Adapter {
	if s == nil {
		s = scrub.New()
	}
	return &Adapter{
		scrubber:    s,
		watchRoot:   watchRoot,
		name:        models.ToolOpenInterpreter,
		homeEnvVar:  "INTERPRETER_HOME",
		homeDirName: ".openinterpreter",
	}
}
