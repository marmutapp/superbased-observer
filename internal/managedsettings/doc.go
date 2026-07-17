// Package managedsettings generates the Anthropic managed-settings artifacts an
// admin deploys to point a fleet of Claude Code nodes at this Observer install
// (native-console integration, Workstream B / Phase 4).
//
// It produces two distinct artifacts that do different jobs and must both ship
// (template §4.2):
//
//   - managed-mcp.json — pins Observer's MCP server fleet-wide (in-session tool
//     presence). It does NOT deliver telemetry.
//   - managed-settings env block — points Claude Code's native OTel exporter at
//     Observer's OTLP receiver. THIS is what delivers telemetry.
//
// The package is pure: given options it returns marshaled JSON and a human
// README explaining the MCP-vs-telemetry split. The CLI command
// (observer org emit-managed-settings) injects the endpoint from config and
// handles file/stdout I/O.
package managedsettings
