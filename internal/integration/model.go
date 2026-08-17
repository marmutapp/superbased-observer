package integration

import (
	"fmt"
	"strings"
)

// ModelLaunch composes the argv/env ADDITIONS the dashboard's New Terminal
// model picker (B5) appends to `observer <spec's launcher subcommand>` to
// seed a fresh session with a chosen model — i.e. the caller runs
// `observer <sub> <args...>` with env set from the returned pairs (the
// dashboard fresh-launch execs exactly that; a CLI user could type the
// argv form). It is the single PURE seam that builds the delivery from
// grounded ModelSpec DATA, so no consumer branches on tool name
// (CLAUDE.md #3) — it mirrors ResumeArgs' composition discipline.
//
// Every observer launcher forwards non-owned argv to the wrapped tool via
// the B6 flag-passthrough (DisableFlagParsing + launcherArgsOrDone), so a
// ModelArg args return value reaches the tool unmodified; a ModelEnv
// return value is set in the child process environment before exec.
//
// It errors rather than fabricate a launch when:
//   - spec.Kind == ModelNone        → no grounded seed-time model mechanism;
//   - model is empty                → nothing to select;
//   - model begins with '-'         → unsafe as an argv token (flag injection);
//   - model contains whitespace or a control character → unsafe as a single
//     argv token / env value;
//   - spec.Kind == ModelEnv and spec.EnvVar == "" → no env var to set.
//
// Model values legitimately contain characters beyond alphanumerics —
// "anthropic/claude-sonnet-4.6", "sonnet:high",
// "claude-opus-4-8[context=1m]" are all real, grounded examples — so only
// leading-dash, whitespace, and control characters are rejected; '/', ':',
// '.', '[', ']' and similar are accepted.
func ModelLaunch(spec ModelSpec, model string) (args []string, env []string, err error) {
	if spec.Kind == ModelNone {
		return nil, nil, fmt.Errorf("integration.ModelLaunch: no model mechanism (Kind=%q)", spec.Kind)
	}
	m := strings.TrimSpace(model)
	if m == "" {
		return nil, nil, fmt.Errorf("integration.ModelLaunch: empty model")
	}
	if strings.HasPrefix(m, "-") {
		// Defensive: a model value must never be mistaken for a flag once it
		// lands in the launcher/tool argv.
		return nil, nil, fmt.Errorf("integration.ModelLaunch: model %q must not begin with '-'", m)
	}
	for _, r := range m {
		if r <= ' ' || r == 0x7f {
			// Whitespace or a control character breaks the single-argv-token /
			// single-env-value contract (and could smuggle a second flag past a
			// naive space-split forwarder).
			return nil, nil, fmt.Errorf("integration.ModelLaunch: model %q contains whitespace or a control character", m)
		}
	}

	switch spec.Kind {
	case ModelArg:
		flag := spec.Flag
		if flag == "" {
			flag = "--model"
		}
		out := append([]string{}, spec.Lead...)
		out = append(out, flag, m)
		return out, nil, nil
	case ModelEnv:
		if spec.EnvVar == "" {
			return nil, nil, fmt.Errorf("integration.ModelLaunch: ModelEnv spec has no EnvVar")
		}
		var out []string
		out = append(out, spec.Lead...)
		return out, []string{spec.EnvVar + "=" + m}, nil
	default:
		return nil, nil, fmt.Errorf("integration.ModelLaunch: unknown ModelKind %q", spec.Kind)
	}
}
