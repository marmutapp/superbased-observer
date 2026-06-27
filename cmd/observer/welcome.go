package main

import (
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// printWelcome renders the bare-`observer` greeting (usability arc
// P1.13): a short status-oriented welcome instead of the full cobra
// help wall — orientation first, `--help` one flag away. Probes the
// daemon's two ports with short dials so the first line answers the
// first question ("is it running?") without opening the DB.
func printWelcome(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()

	proxyPort := 8820
	if cfg, err := config.Load(config.LoadOptions{}); err == nil && cfg.Proxy.Port > 0 {
		proxyPort = cfg.Proxy.Port
	}
	const dashAddr = "127.0.0.1:8081"
	dashUp := portUp(dashAddr)
	proxyUp := portUp(fmt.Sprintf("127.0.0.1:%d", proxyPort))

	fmt.Fprintf(out, "SuperBased Observer %s\n\n", version)
	switch {
	case dashUp:
		fmt.Fprintf(out, "  daemon      running — dashboard at http://%s (proxy :%d)\n", dashAddr, proxyPort)
	case proxyUp:
		fmt.Fprintf(out, "  daemon      proxy up on :%d (dashboard not detected on %s)\n", proxyPort, dashAddr)
	default:
		fmt.Fprintf(out, "  daemon      not detected — start it with `observer start`\n")
	}
	fmt.Fprint(out, "\n")
	fmt.Fprintf(out, "  observer start     proxy + watcher + dashboard (http://%s)\n", dashAddr)
	fmt.Fprint(out, "  observer init      wire hooks / MCP / proxy routing into your AI tools\n")
	fmt.Fprint(out, "  observer doctor    health checks\n")
	fmt.Fprint(out, "  observer --help    every command\n")
	return nil
}

// portUp reports whether something is listening on a loopback addr.
// Short timeout — this runs on every bare `observer` invocation.
func portUp(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
