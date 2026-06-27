package diag

import (
	"os"
	"os/exec"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// checkCopilotTeams verifies the GitHub Copilot native-console surface on the node
// (native-console instance #3). Copilot is DIFFERENT from CC/Codex: its
// governance is entirely server-side (GitHub.com policy UI + a hosted MCP registry
// + REST set-points) with NO node-local managed-config file to verify (learning
// L10). The only node-local Copilot native-console surface is Rail A — Copilot's
// OTLP redirect (`COPILOT_OTEL_ENABLED` / `github.copilot.chat.otel.*`) pointed at
// Observer's receiver. So this check self-gates on COPILOT_OTEL_ENABLED: when the
// operator has turned the redirect on, it confirms the receiver is reachable + the
// MCP pin can bootstrap; otherwise it returns one informational OK noting that
// Copilot policy/MCP-registry governance is server-side and not node-verifiable.
// Gaps are WARN, never FAIL.
func checkCopilotTeams(cfg config.Config) Check {
	const name = "copilot-teams"

	if !copilotOTelRequested() {
		return Check{Name: name, Status: StatusOK, Message: "copilot native-console is server-side (GitHub policy UI + MCP registry); no node-local config to verify — set COPILOT_OTEL_ENABLED to redirect Copilot OTel here (Rail A)"}
	}

	var details []string
	worst := StatusOK
	note := func(status Status, msg string) {
		details = append(details, msg)
		if status > worst {
			worst = status
		}
	}

	// (a) OTLP receiver reachable — the COPILOT_OTEL redirect targets it.
	if cfg.Ingest.OTel.Enabled {
		for _, addr := range []string{cfg.Ingest.OTel.GRPCAddr, cfg.Ingest.OTel.HTTPAddr} {
			if addr == "" {
				continue
			}
			if dialable(addr) {
				note(StatusOK, "OTLP receiver reachable at "+addr+" (Copilot Rail A redirect target)")
			} else {
				note(StatusWarn, "OTLP receiver NOT reachable at "+addr+" — is `observer start` running with [ingest.otel].enabled?")
			}
		}
	} else {
		note(StatusWarn, "COPILOT_OTEL_ENABLED is set but [ingest.otel].enabled is false — the redirected Copilot telemetry has no receiver")
	}

	// (b) MCP pin bootstrap: the superbased-observer MCP server runs via
	// `npx -y @superbased/observer …`, so npx must be on PATH.
	if _, err := exec.LookPath("npx"); err == nil {
		note(StatusOK, "npx present — the superbased-observer MCP pin can bootstrap")
	} else {
		note(StatusWarn, "npx not on PATH — the `npx -y @superbased/observer` MCP pin will not start")
	}

	msg := "copilot OTel redirect requested — checks passed"
	if worst != StatusOK {
		msg = "copilot OTel redirect requested — see warnings below"
	}
	return Check{Name: name, Status: worst, Message: msg, Details: details}
}

// copilotOTelRequested reports whether the operator has turned on Copilot's OTLP
// redirect (the only Copilot-specific node signal; COPILOT_OTEL_ENABLED is set to
// a truthy value).
func copilotOTelRequested() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("COPILOT_OTEL_ENABLED")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
