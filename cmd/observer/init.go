package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
		flagHermes     bool
		flagUninstall  bool
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
			// Hermes lives on a separate install path (~/.hermes/plugins/) so
			// it isn't enumerated by hook.Registry / mcp.Registrar today;
			// handled out-of-band below. --all opts it in too.
			runHermes := flagHermes || flagAll
			// When the operator passes ONLY --hermes (no other per-tool flag,
			// no --all), suppress the auto-detect fallback selectTools applies
			// — they explicitly asked for hermes, not for "init everything
			// detected plus hermes". Without this guard, `observer init
			// --hermes` re-registers every other detected tool's hooks/MCP
			// too, which has bitten the operator at least once during local
			// smoke testing.
			anyClassicFlag := flagClaudeCode || flagCodex || flagCursor || flagCline
			if flagHermes && !anyClassicFlag && !flagAll {
				tools = nil
			}
			if len(tools) == 0 && !runHermes {
				fmt.Fprintln(cmd.OutOrStdout(), "no tools selected and none auto-detected — pass --claude-code / --cursor / --codex / --hermes / --all")
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

			if runHermes {
				if err := runHermesInit(out, hook.HermesOptions{
					BinaryPath: binary,
					ConfigPath: resolvedConfig,
					DryRun:     flagDryRun,
				}, flagSkipHooks, flagSkipMCP, flagUninstall); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagClaudeCode, "claude-code", false, "Register hooks + MCP for Claude Code")
	cmd.Flags().BoolVar(&flagCodex, "codex", false, "Register MCP + hooks for OpenAI Codex (sets [features].hooks=true; per-hook trust approval still required via codex /hooks one-time)")
	cmd.Flags().BoolVar(&flagCursor, "cursor", false, "Register hooks + MCP for Cursor")
	cmd.Flags().BoolVar(&flagCline, "cline", false, "Register for Cline / Roo Code (no hooks; captured via file watcher)")
	cmd.Flags().BoolVar(&flagHermes, "hermes", false, "Register Python plugin + MCP entry for Nous Research's Hermes Agent (~/.hermes/plugins/superbased-observer/ + ~/.hermes/config.yaml)")
	cmd.Flags().BoolVar(&flagUninstall, "uninstall", false, "Uninstall instead of install — currently only honoured for --hermes")
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

// runHermesInit installs (or uninstalls when uninstall=true) the
// SuperBased Observer Python plugin into ~/.hermes/plugins/ AND the
// MCP entry in ~/.hermes/config.yaml. Lives out-of-band from
// wireAIClients because Hermes's plugin install path is
// fundamentally different from Claude Code / cursor / codex (a
// per-plugin directory with a Python __init__.py, not entries in a
// settings.json).
//
// Logs one line per file touched (or "would write" under DryRun).
// Errors propagate to the cobra RunE — the caller exits 1 on
// failure, matching the existing init behaviour.
func runHermesInit(out io.Writer, opts hook.HermesOptions, skipHooks, skipMCP, uninstall bool) error {
	verb := "wrote"
	if opts.DryRun {
		verb = "would write"
	}
	if uninstall {
		if !skipHooks {
			path, err := hook.UnregisterHermes(opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "hermes: removed plugin dir %s\n", path)
			cfgPath, err := hook.UnregisterHermesPluginEnabled(opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "hermes: removed plugins.enabled entry from %s\n", cfgPath)
		}
		if !skipMCP {
			path, err := hook.UnregisterHermesMCP(opts)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "hermes: removed mcp entry from %s\n", path)
		}
		return nil
	}
	if !skipHooks {
		path, err := hook.RegisterHermes(opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "hermes: %s plugin to %s\n", verb, path)
		// Plugin discovery picks up the dropped files but Hermes
		// skips loading anything not in `plugins.enabled` (verified
		// against hermes_cli/plugins.py at validation time). Write
		// the allow-list entry too. Same config.yaml as the MCP
		// merge below; both writes are idempotent.
		cfgPath, err := hook.RegisterHermesPluginEnabled(opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "hermes: %s plugins.enabled entry to %s\n", verb, cfgPath)
	}
	if !skipMCP {
		path, err := hook.RegisterHermesMCP(opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "hermes: %s mcp entry to %s\n", verb, path)
	}
	return nil
}

// WireAIClientsOptions parameterises wireAIClients — the reusable
// AI-client integration step used by both `observer init` and the
// post-enrol auto-wire path in `observer enroll` (M4.2 of the v1.8.0
// teams remediation). Zero value = "install hooks + MCP + proxy
// routing for every detected tool".
type WireAIClientsOptions struct {
	ConfigPath string // absolute path to observer config.toml; "" = legacy mode
	ProxyPort  int    // observer proxy port to wire into per-tool routing (0 = 8820 default)
	DryRun     bool
	Force      bool
	SkipHooks  bool
	SkipMCP    bool
	SkipProxy  bool
	// Restrict the wire to a specific subset. Empty = every detected tool.
	OnlyClaudeCode, OnlyCodex, OnlyCursor, OnlyCline bool
	// All is a convenience flag matching --all on `observer init`.
	All bool
}

// wireAIClients runs the same hook + MCP + proxy-route registration
// flow as `observer init`. Returns a list of human-readable lines
// summarising what was registered (for the caller to print), and
// whether the user should be told to set ANTHROPIC_BASE_URL manually
// (Claude Code's proxy hint).
//
// Used by both newInitCmd and newEnrollCmd. Refactor seam from M4.2;
// the legacy newInitCmd body could be migrated to call this directly
// in a follow-up.
func wireAIClients(opts WireAIClientsOptions) (lines []string, claudeProxyHint, codexProxyHint string, codexHooksHint bool, err error) {
	binary, err := absoluteBinaryPath()
	if err != nil {
		return nil, "", "", false, err
	}
	port := opts.ProxyPort
	if port == 0 {
		port = 8820
	}
	hookReg, err := hook.NewRegistry(hook.Options{
		BinaryPath: binary,
		DryRun:     opts.DryRun,
		Force:      opts.Force,
		ConfigPath: opts.ConfigPath,
	})
	if err != nil {
		return nil, "", "", false, err
	}
	mcpReg, err := mcp.NewRegistrar(mcp.RegisterOptions{
		BinaryPath: binary,
		DryRun:     opts.DryRun,
		Force:      opts.Force,
		ConfigPath: opts.ConfigPath,
	})
	if err != nil {
		return nil, "", "", false, err
	}
	installed := unionStrings(hookReg.Installed(), mcpReg.Installed())
	tools := selectTools(opts.All, opts.OnlyClaudeCode, opts.OnlyCodex, opts.OnlyCursor, opts.OnlyCline, installed)
	if len(tools) == 0 {
		return nil, "", "", false, nil
	}

	var routeReg *proxyroute.Registrar
	if !opts.SkipProxy {
		routeReg, err = proxyroute.NewRegistrar(proxyroute.RegisterOptions{
			ProxyPort: port,
			DryRun:    opts.DryRun,
			Force:     opts.Force,
		})
		if err != nil {
			return nil, "", "", false, err
		}
	}

	var buf strings.Builder
	registeredClaudeCode := false
	registeredCodex := false
	registeredCodexHooks := false
	for _, t := range tools {
		if !opts.SkipHooks && hookSupported(t) {
			res := hookReg.Register(t)
			printHookResult(&buf, t, res, opts.DryRun)
			if t == "codex" && res.Error == nil && len(res.HooksAdded) > 0 {
				registeredCodexHooks = true
			}
		}
		if !opts.SkipMCP && mcpSupported(t) {
			printMCPResult(&buf, t, mcpReg.Register(t), opts.DryRun)
		}
		if !opts.SkipProxy && routeSupported(t) {
			switch t {
			case "codex":
				printProxyRouteResult(&buf, t, routeReg.RegisterCodex(), opts.DryRun)
			case "claude-code":
				printProxyRouteResult(&buf, t, routeReg.RegisterClaudeCode(), opts.DryRun)
			}
		}
		if t == "claude-code" {
			registeredClaudeCode = true
		}
		if t == "codex" {
			registeredCodex = true
		}
	}

	// Claude Code proxy hint is now only emitted when the operator
	// explicitly skipped proxy-routing — otherwise the write above
	// did the work and a redundant "next: export ANTHROPIC_BASE_URL"
	// would be misleading. Pre-v1.8.2 the env var was print-only,
	// which is N4 in docs/teams-test-regression-2026-06-03.md.
	if registeredClaudeCode && !opts.DryRun && opts.SkipProxy {
		var hint strings.Builder
		printProxyRoutingHint(&hint, port)
		claudeProxyHint = hint.String()
	}
	if registeredCodex && opts.SkipProxy {
		codexProxyHint = proxyroute.CodexHint(port)
	}
	codexHooksHint = registeredCodexHooks && !opts.DryRun

	lines = strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	return lines, claudeProxyHint, codexProxyHint, codexHooksHint, nil
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
// can write proxy-routing into. Claude Code persists env in
// ~/.claude/settings.json (`"env": { "ANTHROPIC_BASE_URL": ... }`), so
// it joins codex on the "writable" side as of v1.8.2. Cursor remains
// hint-only — its config doesn't carry a persistent OPENAI_BASE_URL.
func routeSupported(tool string) bool {
	return tool == "codex" || tool == "claude-code"
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
