//go:build windows

package codexipc

import (
	"context"
	"fmt"
	"os/exec"
)

// detect on Windows runs a PowerShell CIM query against Win32_Process
// for codex.exe / Codex.exe entries, pipes the result through
// ConvertTo-Csv, and hands the bytes to parseTasklistCSV.
//
// PowerShell is reused from the existing pattern in
// internal/adapter/antigravity/bridge.go (same -NoProfile
// -NonInteractive flags). `tasklist /v /fo csv` would be marginally
// faster but does NOT include CommandLine — we need it to filter
// `app-server` from `exec` / renderer children, and the v5 doc's
// reproducer at docs/observer-platform-issues-v5.md §V5-1 uses the
// same CIM query shape.
const tasklistPSScript = `Get-CimInstance Win32_Process -Filter "Name = 'codex.exe' OR Name = 'Codex.exe'" | ` +
	`Select-Object ProcessId,Name,Path,CommandLine | ` +
	`ConvertTo-Csv -NoTypeInformation`

func detect(ctx context.Context) ([]Process, error) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return nil, fmt.Errorf("codexipc.detect: powershell.exe not on PATH: %w", err)
	}
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command", tasklistPSScript)
	out, err := cmd.Output()
	if err != nil {
		// CIM query failure is rare but non-fatal — return no
		// processes + the underlying error so the caller can decide
		// whether to surface it. A clean host returns zero rows
		// (just the header), not an error.
		return nil, fmt.Errorf("codexipc.detect: powershell CIM query: %w", err)
	}
	return parseTasklistCSV(out), nil
}
