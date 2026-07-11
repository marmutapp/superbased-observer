package managedsettings

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerateClaudeCode_BothArtifacts(t *testing.T) {
	got, err := GenerateClaudeCode(ClaudeCodeOptions{
		OTelGRPCEndpoint: "http://127.0.0.1:4317",
		IncludeMCP:       true,
		IncludeTelemetry: true,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// managed-settings env block points OTel at the receiver.
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(got.ManagedSettingsJSON, &settings); err != nil {
		t.Fatalf("settings json: %v", err)
	}
	if settings.Env["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" ||
		settings.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:4317" ||
		settings.Env["OTEL_EXPORTER_OTLP_PROTOCOL"] != "grpc" {
		t.Fatalf("env block wrong: %+v", settings.Env)
	}

	// managed-mcp pins the server with the mandatory -y npx form.
	var mcp struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got.ManagedMCPJSON, &mcp); err != nil {
		t.Fatalf("mcp json: %v", err)
	}
	srv, ok := mcp.MCPServers[MCPServerName]
	if !ok {
		t.Fatalf("mcp server %q missing: %+v", MCPServerName, mcp.MCPServers)
	}
	if srv.Command != "npx" || len(srv.Args) < 2 || srv.Args[0] != "-y" {
		t.Fatalf("mcp pin not self-bootstrapping with -y: %+v", srv)
	}

	// README must teach the MCP-vs-telemetry split.
	if !strings.Contains(got.Readme, "NOT") || !strings.Contains(strings.ToLower(got.Readme), "telemetry") {
		t.Fatalf("readme missing the MCP-vs-telemetry distinction")
	}
}

func TestGenerateClaudeCode_TelemetryRequiresEndpoint(t *testing.T) {
	_, err := GenerateClaudeCode(ClaudeCodeOptions{IncludeTelemetry: true})
	if err == nil {
		t.Fatal("expected error when telemetry requested without endpoint")
	}
}

func TestGenerateClaudeCode_NothingSelected(t *testing.T) {
	if _, err := GenerateClaudeCode(ClaudeCodeOptions{}); err == nil {
		t.Fatal("expected error when no artifact selected")
	}
}

func TestGenerateClaudeCode_MCPOnly(t *testing.T) {
	got, err := GenerateClaudeCode(ClaudeCodeOptions{IncludeMCP: true, MCPPackage: "@superbased/observer@1.8.4"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got.ManagedSettingsJSON != nil {
		t.Fatalf("telemetry artifact should be nil when not requested")
	}
	if !strings.Contains(string(got.ManagedMCPJSON), "@superbased/observer@1.8.4") {
		t.Fatalf("pinned package not honored: %s", got.ManagedMCPJSON)
	}
}
