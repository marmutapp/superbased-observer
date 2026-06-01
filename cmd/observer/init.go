package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
	"github.com/marmutapp/superbased-observer/internal/proxyroute"
)

// newInitCmd implements `observer init` — the explicit one-shot
// registration entry point for every supported AI coding tool. A
// single `init` writes settings.json/hooks.json hook entries AND
// mcp.json/.claude.json/config.toml MCP server entries AND (codex
// only) the proxy `base_url` for every selected tool.
//
// Default scope: hooks + MCP + proxy-route are all ON. Opt out
// per-side with `--skip-hooks`, `--skip-mcp`, `--skip-proxy-route`.
// MCP-supported tools are claude-code, cursor, codex (cline is
// hook-and-watcher only; windows variants are hook-only). See
// [mcpSupported] for the whitelist.
//
// Init vs start (frequently confused):
//   - `observer init` writes per-client config and is the only path
//     to MCP registration / codex proxy routing.
//   - `observer start` runs the daemon AND idempotently
//     auto-registers HOOKS (only) for any detected AI tool. MCP and
//     codex proxy-route are deliberately NOT auto-wired on start —
//     they treat per-client config as explicit user opt-in.
func newInitCmd() *cobra.Command {
	var (
		flagClaudeCode bool
		flagCodex      bool
		flagCursor     bool
		flagCline      bool
		flagAll        bool
		flagDryRun     bool
		flagForce      bool
		flagSkipHooks  bool
		flagSkipMCP    bool
		flagSkipProxy  bool
		flagProxyPort  int
		flagConfigPath string
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Register hooks + MCP server with AI coding tools",
		RunE: func(cmd *cobra.Command, args []string) error {
			binary, err := absoluteBinaryPath()
			if err != nil {
				return err
			}
			resolvedConfig, err := resolveInitConfigPath(flagConfigPath)
			if err != nil {
				return err
			}
			hookReg, err := hook.NewRegistry(hook.Options{
				BinaryPath: binary,
				DryRun:     flagDryRun,
				Force:      flagForce,
				ConfigPath: resolvedConfig,
			})
			if err != nil {
				return err
			}
			mcpReg, err := mcp.NewRegistrar(mcp.RegisterOptions{
				BinaryPath: binary,
				DryRun:     flagDryRun,
				Force:      flagForce,
				ConfigPath: resolvedConfig,
			})
			if err != nil {
				return err
			}

			// Union of installed tools across both registries — covers
			// codex (MCP only) which the hook registry doesn't enumerate.
			installed := unionStrings(hookReg.Installed(), mcpReg.Installed())
			tools := selectTools(flagAll, flagClaudeCode, flagCodex, flagCursor, flagCline, installed)
			if len(tools) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no tools selected and none auto-detected — pass --claude-code / --cursor / --codex / --all")
				return nil
			}

			var routeReg *proxyroute.Registrar
			if !flagSkipProxy {
				routeReg, err = proxyroute.NewRegistrar(proxyroute.RegisterOptions{
					ProxyPort: flagProxyPort,
					DryRun:    flagDryRun,
					Force:     flagForce,
				})
				if err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()
			registeredClaudeCode := false
			registeredCodex := false
			registeredCodexHooks := false
			for _, t := range tools {
				if !flagSkipHooks {
					if hookSupported(t) {
						res := hookReg.Register(t)
						printHookResult(out, t, res, flagDryRun)
						if t == "codex" && res.Error == nil && len(res.HooksAdded) > 0 {
							registeredCodexHooks = true
						}
					}
				}
				if !flagSkipMCP {
					if mcpSupported(t) {
						printMCPResult(out, t, mcpReg.Register(t), flagDryRun)
					}
				}
				if !flagSkipProxy && routeSupported(t) {
					if t == "codex" {
						printProxyRouteResult(out, t, routeReg.RegisterCodex(), flagDryRun)
					}
				}
				if t == "claude-code" {
					registeredClaudeCode = true
				}
				if t == "codex" {
					registeredCodex = true
				}
			}
			// Hooks + MCP capture the JSONL adapter side and on-demand
			// queries, but the proxy stream — the only accurate token
			// source per spec §24 — only engages when the AI tool routes
			// API traffic through it. We've seen real installs where
			// Claude Code keeps calling api.anthropic.com directly because
			// the env var was never set, leaving cost analytics dependent
			// on the unreliable JSONL stream. See audit item B1.
			if registeredClaudeCode && !flagDryRun {
				printProxyRoutingHint(out, flagProxyPort)
			}
			if registeredCodex && flagSkipProxy {
				fmt.Fprintln(out)
				fmt.Fprint(out, proxyroute.CodexHint(flagProxyPort))
			}
			if registeredCodexHooks && !flagDryRun {
				fmt.Fprintln(out)
				fmt.Fprintln(out, "next: codex requires per-hook trust approval (security feature).")
				fmt.Fprintln(out, "  open codex once and run /hooks to mark all 6 entries trusted.")
				fmt.Fprintln(out, "  one-time setup; trust persists in ~/.codex/config.toml [hooks.state].")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagClaudeCode, "claude-code", false, "Register hooks + MCP for Claude Code")
	cmd.Flags().BoolVar(&flagCodex, "codex", false, "Register MCP + hooks for OpenAI Codex (sets [features].hooks=true; per-hook trust approval still required via codex /hooks one-time)")
	cmd.Flags().BoolVar(&flagCursor, "cursor", false, "Register hooks + MCP for Cursor")
	cmd.Flags().BoolVar(&flagCline, "cline", false, "Register for Cline / Roo Code (no hooks; captured via file watcher)")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Select every detected tool")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Print intended changes without writing any files")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite existing non-observer hook / MCP entries")
	cmd.Flags().BoolVar(&flagSkipHooks, "skip-hooks", false, "Only register MCP, leave hooks alone")
	cmd.Flags().BoolVar(&flagSkipMCP, "skip-mcp", false, "Only register hooks, leave MCP alone")
	cmd.Flags().BoolVar(&flagSkipProxy, "skip-proxy-route", false, "Skip writing per-tool proxy routing config (e.g. codex base_url) — print a hint instead")
	cmd.Flags().IntVar(&flagProxyPort, "proxy-port", 8820, "Observer proxy port to wire into per-tool routing config")
	cmd.Flags().StringVar(&flagConfigPath, "config", "", "Path to observer config.toml — when set, registered hook + MCP commands include --config so they read the same config as the proxy you'll run against this install")
	return cmd
}

// resolveInitConfigPath validates and absolutizes the --config flag value
// for `observer init`. Empty input means "no flag passed" — returns ""
// so registrations omit --config entirely (legacy behaviour). Non-empty
// input must point at an existing file; we absolutize the path so the
// registered hook/MCP commands keep working when invoked from any CWD.
func resolveInitConfigPath(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve --config %q: %w", raw, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("--config %q: %w", abs, err)
	}
	return abs, nil
}

func printHookResult(out io.Writer, tool string, res hook.RegistrationResult, dryRun bool) {
	if res.Error != nil {
		fmt.Fprintf(out, "%-12s hook ✗ %v\n", tool, res.Error)
		return
	}
	verb := "registered"
	if dryRun {
		verb = "would register"
	}
	if len(res.HooksAdded) > 0 {
		fmt.Fprintf(out, "%-12s hook %s %d hook(s) in %s: %v\n",
			tool, verb, len(res.HooksAdded), res.ConfigPath, res.HooksAdded)
	}
	if len(res.AlreadySet) > 0 {
		fmt.Fprintf(out, "%-12s hook already set: %v\n", tool, res.AlreadySet)
	}
}

func printMCPResult(out io.Writer, tool string, res mcp.RegistrationResult, dryRun bool) {
	if res.Error != nil {
		fmt.Fprintf(out, "%-12s mcp  ✗ %v\n", tool, res.Error)
		return
	}
	verb := "registered"
	if dryRun {
		verb = "would register"
	}
	if res.AlreadySet {
		fmt.Fprintf(out, "%-12s mcp  already set in %s\n", tool, res.ConfigPath)
		return
	}
	if res.Added {
		fmt.Fprintf(out, "%-12s mcp  %s in %s\n", tool, verb, res.ConfigPath)
	}
}

// printProxyRoutingHint reminds the user that hook + MCP installation
// alone won't engage the proxy — Claude Code needs ANTHROPIC_BASE_URL
// pointed at the proxy's listen address, otherwise traffic flies past
// to api.anthropic.com directly and the only token-count source left is
// the unreliable JSONL stream.
func printProxyRoutingHint(out io.Writer, port int) {
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "next: route Claude Code through the observer proxy for accurate token capture.")
	fmt.Fprintln(out, "  start the proxy:    observer proxy start")
	fmt.Fprintf(out, "  point Claude Code:  export ANTHROPIC_BASE_URL=%s\n", url)
	fmt.Fprintln(out, "  or persist via ~/.claude/settings.json:")
	fmt.Fprintf(out, "      \"env\": { \"ANTHROPIC_BASE_URL\": %q }\n", url)
	fmt.Fprintln(out, "  see docs/proxy-routing.md for verification + per-shell setup.")
}

// printProxyRouteResult prints the outcome of a proxyroute registration
// in the same single-line shape as MCP/hook results.
func printProxyRouteResult(out io.Writer, tool string, res proxyroute.RegistrationResult, dryRun bool) {
	if res.Error != nil {
		fmt.Fprintf(out, "%-12s route ✗ %v\n", tool, res.Error)
		return
	}
	verb := "registered"
	if dryRun {
		verb = "would register"
	}
	if res.AlreadySet {
		fmt.Fprintf(out, "%-12s route already set in %s → %s\n", tool, res.ConfigPath, res.BaseURL)
		return
	}
	if res.Added {
		fmt.Fprintf(out, "%-12s route %s in %s → %s\n", tool, verb, res.ConfigPath, res.BaseURL)
	}
}

func hookSupported(tool string) bool {
	switch tool {
	case "claude-code", "claude-code-windows", "cursor", "cursor-windows", "codex":
		return true
	}
	return false
}

func mcpSupported(tool string) bool {
	switch tool {
	case "claude-code", "cursor", "codex":
		return true
	}
	return false
}

// routeSupported reports whether the tool has a per-tool config file we
// can write proxy-routing into. Claude Code and Cursor route via env
// vars (ANTHROPIC_BASE_URL / OPENAI_BASE_URL) — they remain hint-only.
func routeSupported(tool string) bool {
	return tool == "codex"
}

func unionStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func selectTools(all, cc, codex, cursor, cline bool, installed []string) []string {
	requested := map[string]bool{}
	if all {
		for _, t := range installed {
			requested[t] = true
		}
	}
	if cc {
		requested["claude-code"] = true
	}
	if codex {
		requested["codex"] = true
	}
	if cursor {
		requested["cursor"] = true
	}
	if cline {
		requested["cline"] = true
	}
	if len(requested) == 0 && !all {
		for _, t := range installed {
			requested[t] = true
		}
	}
	supported := map[string]bool{
		"claude-code": true, "claude-code-windows": true,
		"cursor": true, "cursor-windows": true,
		"codex": true,
	}
	var out []string
	for t := range requested {
		if supported[t] {
			out = append(out, t)
		}
	}
	return out
}

// absoluteBinaryPath returns the absolute path of the running binary so that
// hook commands written into settings files are stable across shells and
// $PATH changes.
func absoluteBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve binary path: %w", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}
