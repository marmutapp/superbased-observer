package locate

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestLocations pins the locator table — these paths are the contract
// both mcp.Registrar and guard/mcpsec program against; a change here
// is a change to where observer believes each client keeps its MCP
// config and must be deliberate.
func TestLocations(t *testing.T) {
	t.Parallel()
	home := filepath.Join("h", "ome")
	want := []Location{
		{Client: "claude-code", Path: filepath.Join(home, ".claude.json"), Format: FormatMCPServersJSON},
		{Client: "cursor", Path: filepath.Join(home, ".cursor", "mcp.json"), Format: FormatMCPServersJSON},
		{Client: "codex", Path: filepath.Join(home, ".codex", "config.toml"), Format: FormatCodexTOML},
		{Client: "opencode", Path: filepath.Join(home, ".config", "opencode", "opencode.json"), Format: FormatOpenCodeJSON},
		{Client: "cline", Path: ClineSettingsPath(home, runtime.GOOS), Format: FormatMCPServersJSON},
	}
	got := Locations(home)
	if len(got) != len(want) {
		t.Fatalf("Locations: got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Locations[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestForClient covers the lookup and the unknown-client miss.
func TestForClient(t *testing.T) {
	t.Parallel()
	loc, ok := ForClient("cursor", "h")
	if !ok || loc.Path != filepath.Join("h", ".cursor", "mcp.json") || loc.Format != FormatMCPServersJSON {
		t.Errorf("ForClient(cursor) = %+v, %v", loc, ok)
	}
	if _, ok := ForClient("not-a-client", "h"); ok {
		t.Error("ForClient(not-a-client) reported ok")
	}
}
