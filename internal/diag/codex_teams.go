package diag

import (
	"os"
	"os/exec"

	"github.com/BurntSushi/toml"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// codexManagedPaths are the system-channel locations the Codex managed-config
// artifacts deploy to (native-console Workstream B). requirements.toml is the
// enforced layer; managed_config.toml is the defaults layer.
var codexManagedPaths = []string{
	"/etc/codex/requirements.toml",
	"/etc/codex/managed_config.toml",
}

// checkCodexTeams verifies the Codex native-console managed-config deployment
// (native-console template §4.3, instance #2). Unlike the Claude Code check it
// has no [ingest.otel] gate to key off — Codex's rails are config-distributed —
// so it self-gates on the PRESENCE of a system-channel managed artifact: with
// none deployed it returns one informational OK; with one present it reports
// sub-checks as Details (artifacts parseable, MCP pin resolvable, OTLP endpoint
// reachable when the receiver is configured). Gaps are WARN, never FAIL.
func checkCodexTeams(cfg config.Config) Check {
	const name = "codex-teams"

	present := presentCodexManagedPaths()
	if len(present) == 0 {
		return Check{Name: name, Status: StatusOK, Message: "codex managed-config not deployed (no /etc/codex/*.toml — run `observer org emit-managed-settings --provider codex`)"}
	}

	var details []string
	worst := StatusOK
	note := func(status Status, msg string) {
		details = append(details, msg)
		if status > worst {
			worst = status
		}
	}

	// (a) Managed artifacts present + parseable TOML.
	for _, p := range present {
		if err := parseableTOML(p); err != nil {
			note(StatusWarn, "codex managed artifact "+p+" is not valid TOML: "+err.Error())
		} else {
			note(StatusOK, "codex managed artifact present + parseable ("+p+")")
		}
	}

	// (b) MCP pin resolvable: the [mcp_servers.superbased-observer] pin runs
	// `npx -y @superbased/observer …`, so npx must be on PATH to start it.
	if _, err := exec.LookPath("npx"); err == nil {
		note(StatusOK, "npx present — the [mcp_servers.superbased-observer] pin can bootstrap the MCP server")
	} else {
		note(StatusWarn, "npx not on PATH — the [mcp_servers.superbased-observer] `npx -y @superbased/observer` pin will not start")
	}

	// (c) When the OTLP receiver is configured (Rail A enabled), confirm it is
	// reachable — a managed_config.toml [otel] redirect targets it.
	if cfg.Ingest.OTel.Enabled {
		for _, addr := range []string{cfg.Ingest.OTel.GRPCAddr, cfg.Ingest.OTel.HTTPAddr} {
			if addr == "" {
				continue
			}
			if dialable(addr) {
				note(StatusOK, "OTLP receiver reachable at "+addr+" (Rail A redirect target)")
			} else {
				note(StatusWarn, "OTLP receiver NOT reachable at "+addr+" — is `observer start` running with [ingest.otel].enabled?")
			}
		}
	}

	msg := "codex managed-config deployed — all checks passed"
	if worst != StatusOK {
		msg = "codex managed-config deployed — see warnings below"
	}
	return Check{Name: name, Status: worst, Message: msg, Details: details}
}

// presentCodexManagedPaths returns the subset of codexManagedPaths that exist.
func presentCodexManagedPaths() []string {
	var out []string
	for _, p := range codexManagedPaths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// parseableTOML confirms a file holds syntactically valid TOML.
func parseableTOML(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v map[string]any
	return toml.Unmarshal(b, &v)
}
