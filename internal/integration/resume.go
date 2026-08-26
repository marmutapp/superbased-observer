package integration

import (
	"fmt"
	"strings"
)

// knownResumeMechanisms is the fixed vocabulary of ResumeSpec.IDMechanism
// values the native-resume composer understands. It is DATA (CLAUDE.md #5:
// a table walked, not a nested switch): each key is a way the UNDERLYING
// tool takes the prior session id on its own command line, for which a
// grounded `observer <sub>` launcher provides the tool-specific translation.
// The composer emits the SAME uniform observer-launcher tail
// (`--resume <id>`) for every one of them — the launcher owns the
// translation to the tool's native argv (claude `--resume <id>`, codex
// `resume <id>`) — so the dashboard/CLI caller never branches on tool name
// (CLAUDE.md #3). Adding a new mechanism here is the one edit a future
// launcher-translation needs.
var knownResumeMechanisms = map[string]bool{
	"flag:--resume":     true, // tool takes `--resume <id>` (claude-code, gemini-cli, qwen-code, grok, devin, hermes, qoder, prime-agent, zcode, mistral-code)
	"subcommand:resume": true, // tool takes `resume <id>` (codex)
	"positional":        true, // tool takes a bare `<id>` positional
	// The 2026-07-24 live-verified native-resume wave (15 launchers). Each key
	// is one tool's native id-carrying resume flag; the observer launcher owns
	// the translation from the uniform `--resume <id>` tail to it (see
	// cmd/observer/resume_launcher.go's resumeTranslations table).
	"flag:--session":      true, // `--session <id>` (opencode, kilo-code-cli, pi, kimi-code)
	"flag:--session-id":   true, // `--session-id <id>` (copilot-cli; goose under its `session` subcommand)
	"flag:--id":           true, // `--id <id>` (cline-cli)
	"flag:--conversation": true, // `--conversation <id>` (antigravity-cli)
	"flag:--resume-id":    true, // `--resume-id <id>` (kiro-cli, under its `chat` subcommand)
	// 2026-08 wave (zcode / mistral-code / freebuff).
	"flag:--continue": true, // `--continue=<id>` (freebuff, joined `=` spelling — an optional-value commander.js flag)
}

// ResumeArgs composes the argv TAIL appended after `observer <spec.Subcommand>`
// to natively resume a CLOSED session by id — i.e. the caller runs
// `observer <spec.Subcommand> <ResumeArgs...>` (the dashboard Resume affordance
// spawns exactly that into a fresh terminal; a CLI user types it). It is the
// single PURE seam that builds the resume command from grounded ResumeSpec
// DATA, so no consumer branches on tool name (CLAUDE.md #3).
//
// The tail is uniform — `["--resume", sessionID]` — because every observer
// launcher that carries native resume exposes a `--resume <id>` flag and
// translates it to the tool's own resume mechanism internally. ResumeSpec.
// IDMechanism records HOW the underlying tool takes the id (the grounding
// evidence, surfaced verbatim by nativeResumeHint) and is validated here
// against knownResumeMechanisms so an ungrounded mechanism cannot silently
// produce a command.
//
// It errors rather than fabricate a command when:
//   - spec.Kind != ResumeNative      → no grounded native-resume contract;
//   - spec.Subcommand == ""          → no launcher verb to run;
//   - sessionID is empty             → nothing to resume;
//   - sessionID begins with '-'      → unsafe as an argv token (flag injection);
//   - spec.IDMechanism is unknown    → not a grounded resume mechanism.
func ResumeArgs(spec ResumeSpec, sessionID string) ([]string, error) {
	if spec.Kind != ResumeNative {
		return nil, fmt.Errorf("integration.ResumeArgs: not a native-resume contract (Kind=%q); use the handoff-fork resume instead", spec.Kind)
	}
	if spec.Subcommand == "" {
		return nil, fmt.Errorf("integration.ResumeArgs: ResumeNative spec has no launcher Subcommand")
	}
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return nil, fmt.Errorf("integration.ResumeArgs: empty session id")
	}
	if strings.HasPrefix(id, "-") {
		// Defensive: a session id must never be mistaken for a flag once it
		// lands in the launcher/tool argv.
		return nil, fmt.Errorf("integration.ResumeArgs: session id %q must not begin with '-'", id)
	}
	if !knownResumeMechanisms[spec.IDMechanism] {
		return nil, fmt.Errorf("integration.ResumeArgs: unknown IDMechanism %q for %q", spec.IDMechanism, spec.Subcommand)
	}
	return []string{"--resume", id}, nil
}
