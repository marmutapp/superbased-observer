package main

import (
	"os"
	"os/exec"
	"runtime"
)

// openBrowser opens url in the user's default browser, best-effort:
// every failure path is silent (P1 — a convenience must never block or
// noise up the daemon). Used by `observer start` to open the dashboard
// on interactive launches (usability arc P1.14; opt out with
// --no-open).
//
// Platform notes:
//   - windows: `cmd /c start "" <url>` (the empty "" is start's title
//     argument — without it a quoted URL is mistaken for the title).
//   - darwin: `open`.
//   - linux: `xdg-open`, falling back to WSL-interop paths when the
//     daemon runs inside WSL but the browser lives on the Windows
//     side (`wslview` from wslu, then `explorer.exe` which hands the
//     URL to the default Windows browser).
func openBrowser(url string) {
	var candidates [][]string
	switch runtime.GOOS {
	case "windows":
		candidates = [][]string{{"cmd", "/c", "start", "", url}}
	case "darwin":
		candidates = [][]string{{"open", url}}
	default:
		candidates = [][]string{{"xdg-open", url}}
		if os.Getenv("WSL_DISTRO_NAME") != "" {
			candidates = append(
				candidates,
				[]string{"wslview", url},
				[]string{"explorer.exe", url},
			)
		}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if err := exec.Command(c[0], c[1:]...).Start(); err == nil { //nolint:gosec // G204: fixed per-OS open-browser command table; the URL is the only variable.
			return
		}
	}
}

// stdoutIsTerminal reports whether stdout is an interactive console —
// the gate for auto-opening the dashboard. A daemonized `observer
// start` (setsid, systemd, output redirected to a log file) is not a
// char device, so headless launches never trigger a browser.
func stdoutIsTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
