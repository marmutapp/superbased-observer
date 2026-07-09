package diag

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// checkClaudeCodeTeams verifies the native-console (Claude Code Teams) ingest
// path end to end (native-console template §4.3). It self-gates on
// [ingest.otel].enabled — like checkOrgEnrolment — so a non-native install gets
// a single informational OK rather than spurious warnings. When enabled it
// reports four sub-checks as Details: receiver listening, managed-settings
// present + parseable, the MCP pin resolvable, and the OTLP endpoint reachable.
// Gaps are WARN (deploy/runtime conditions the operator fixes), never FAIL.
func checkClaudeCodeTeams(cfg config.Config) Check {
	const name = "claude-code-teams"
	if !cfg.Ingest.OTel.Enabled {
		return Check{Name: name, Status: StatusOK, Message: "native-console ingest disabled ([ingest.otel].enabled=false)"}
	}

	var details []string
	worst := StatusOK
	note := func(status Status, msg string) {
		details = append(details, msg)
		if status > worst {
			worst = status
		}
	}

	// (a/d) Receiver listening / OTLP endpoint reachable. The daemon owns the
	// listener, so a dial failure usually means `observer start` isn't running.
	for _, addr := range []string{cfg.Ingest.OTel.GRPCAddr, cfg.Ingest.OTel.HTTPAddr} {
		if addr == "" {
			continue
		}
		if dialable(addr) {
			note(StatusOK, fmt.Sprintf("OTLP receiver reachable at %s", addr))
		} else {
			note(StatusWarn, fmt.Sprintf("OTLP receiver NOT reachable at %s — is `observer start` running with [ingest.otel].enabled?", addr))
		}
	}

	// (b) Managed-settings present + parseable.
	if path, ok := managedSettingsPath(); ok {
		if err := parseableJSON(path); err != nil {
			note(StatusWarn, fmt.Sprintf("managed-settings at %s is not valid JSON: %v", path, err))
		} else {
			note(StatusOK, fmt.Sprintf("Claude Code managed-settings present + parseable (%s)", path))
		}
	} else {
		note(StatusWarn, "Claude Code managed-settings not found — deploy with `observer org emit-managed-settings` (file channel: /etc/claude-code/managed-settings.json)")
	}

	// (c) MCP pin resolvable: the managed-mcp.json pin runs `npx -y
	// @superbased/observer …`, so npx must be on PATH for the pin to start.
	if _, err := exec.LookPath("npx"); err == nil {
		note(StatusOK, "npx present — the managed-mcp.json pin can bootstrap the MCP server")
	} else {
		note(StatusWarn, "npx not on PATH — the managed-mcp.json `npx -y @superbased/observer` pin will not start")
	}

	msg := "native-console ingest enabled — all checks passed"
	if worst != StatusOK {
		msg = "native-console ingest enabled — see warnings below"
	}
	return Check{Name: name, Status: worst, Message: msg, Details: details}
}

// dialable reports whether a TCP connection to addr succeeds within a short
// timeout (a host:port like 127.0.0.1:4317).
func dialable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// managedSettingsPath returns the OS-conventional Claude Code managed-settings
// path if it exists. Linux/WSL use /etc/claude-code/; macOS uses the system
// Application Support dir.
func managedSettingsPath() (string, bool) {
	candidates := []string{"/etc/claude-code/managed-settings.json"}
	if runtime.GOOS == "darwin" {
		candidates = append(candidates, "/Library/Application Support/ClaudeCode/managed-settings.json")
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// parseableJSON confirms a file holds syntactically valid JSON.
func parseableJSON(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v any
	return json.Unmarshal(b, &v)
}
