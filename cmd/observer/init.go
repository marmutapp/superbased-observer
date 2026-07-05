package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/guard/compile"
	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/mcp"
	"github.com/marmutapp/superbased-observer/internal/proxyroute"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// newInitCmd implements `observer init` — the explicit one-shot
// registration entry point for every supported AI coding tool. A
// single `init` writes settings.json/hooks.json hook entries AND
// mcp.json/.claude.json/config.toml MCP server entries AND proxy
// routing (codex config.toml `base_url`; claude-code settings.json
// `env.ANTHROPIC_BASE_URL`) for every selected tool. Batch mode
// delegates the whole flow to wireAIClients — the same registration
// step `observer enroll` runs (D18 first brought the old inline loop
// up to wireAIClients parity; the dedup then collapsed it into the
// call). newInitCmd keeps only what wireAIClients deliberately
// doesn't own: the interactive checklist, the hermes install path,
// `--uninstall` (hermes-only), the hermes-only flag-suppression
// guard, and the "no tools selected" message.
//
// With ZERO flags on a real terminal, init runs the interactive
// checklist instead (P6.10; see init_interactive.go) — one consent
// per write, the dashboard wizard's semantics.
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
		flagClaudeCode       bool
		flagCodex            bool
		flagCursor           bool
		flagCline            bool
		flagHermes           bool
		flagUninstall        bool
		flagAll              bool
		flagDryRun           bool
		flagForce            bool
		flagSkipHooks        bool
		flagSkipMCP          bool
		flagSkipProxy        bool
		flagGuard            bool
		flagSkipGuardDialect bool
		flagProxyPort        int
		flagConfigPath       string
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
			// P6.10: zero flags + a human on both ends of the pipe →
			// the interactive checklist (one consent per write, the
			// wizard's semantics). Any flag, or any redirection, keeps
			// the classic batch behaviour for scripts and muscle
			// memory.
			if cmd.Flags().NFlag() == 0 && stdinIsTerminal() && stdoutIsTerminal() {
				return runInteractiveInit(cmd.OutOrStdout(), cmd.InOrStdin(), interactiveInitOptions{
					BinaryPath: binary,
					ConfigPath: resolvedConfig,
					ProxyPort:  flagProxyPort,
				})
			}
			out := cmd.OutOrStdout()
			// Hermes lives on a separate install path (~/.hermes/plugins/) so
			// it isn't enumerated by hook.Registry / mcp.Registrar today;
			// handled out-of-band below. --all opts it in too.
			runHermes := flagHermes || flagAll
			// When the operator passes ONLY --hermes (no other per-tool flag,
			// no --all), skip the classic wire entirely — they explicitly
			// asked for hermes, not for "init everything detected plus
			// hermes". Without this guard, `observer init --hermes`
			// re-registers every other detected tool's hooks/MCP too, which
			// has bitten the operator at least once during local smoke
			// testing.
			anyClassicFlag := flagClaudeCode || flagCodex || flagCursor || flagCline
			hermesOnly := flagHermes && !anyClassicFlag && !flagAll
			if !hermesOnly {
				lines, claudeHint, codexHint, codexHooksHint, err := wireAIClients(WireAIClientsOptions{
					ConfigPath:     resolvedConfig,
					ProxyPort:      flagProxyPort,
					DryRun:         flagDryRun,
					Force:          flagForce,
					SkipHooks:      flagSkipHooks,
					SkipMCP:        flagSkipMCP,
					SkipProxy:      flagSkipProxy,
					OnlyClaudeCode: flagClaudeCode,
					OnlyCodex:      flagCodex,
					OnlyCursor:     flagCursor,
					OnlyCline:      flagCline,
					All:            flagAll,
				})
				if err != nil {
					return err
				}
				// nil lines ⇔ wireAIClients selected no tools (it returns
				// silently); the CLI keeps its explicit message. A single
				// empty-string line means tools WERE selected but every
				// registration was skipped or silent — print nothing,
				// matching the pre-dedup inline loop.
				if lines == nil && !runHermes {
					fmt.Fprintln(out, "no tools selected and none auto-detected — pass --claude-code / --cursor / --codex / --hermes / --all")
					return nil
				}
				if len(lines) != 1 || lines[0] != "" {
					for _, line := range lines {
						fmt.Fprintln(out, line)
					}
				}
				// Hooks + MCP capture the JSONL adapter side and on-demand
				// queries, but the proxy stream — the only accurate token
				// source per spec §24 — only engages when the AI tool routes
				// API traffic through it. The hints fire only when the
				// operator explicitly skipped the route write (D18: the
				// write now happens by default — a redundant "next: export
				// ANTHROPIC_BASE_URL" right after writing it would mislead).
				if claudeHint != "" {
					fmt.Fprint(out, claudeHint)
				}
				if codexHint != "" {
					fmt.Fprintln(out)
					fmt.Fprint(out, codexHint)
				}
				if codexHooksHint {
					printCodexTrustHint(out)
				}
			}
			// Guard native-dialect compilation (spec §13.2): init is
			// the default-on application point — selected tools with an
			// implemented dialect get the effective policy compiled
			// into their native permission rules. Opt out per-run with
			// --skip-guard-dialect / --guard=false, or persistently via
			// [guard.dialects] compile=false. The selection is
			// recomputed (detection-only) because wireAIClients owns it
			// internally now; hermes-only runs skip — the operator
			// asked for hermes, not "compile every detected tool".
			// The interactive path returns before this point and does
			// not compile dialects (P6.10 integration is a recorded
			// follow-up; `observer guard compile` covers it).
			if flagGuard && !flagSkipGuardDialect && !flagUninstall && !hermesOnly {
				initGuardDialects(cmd.Context(), out,
					initSelectedTools(binary, resolvedConfig, flagAll, flagClaudeCode, flagCodex, flagCursor, flagCline),
					resolvedConfig, flagDryRun)
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
	cmd.Flags().BoolVar(&flagGuard, "guard", true, "Compile guard policy into each selected tool's native permission rules (guard spec §13.2); --guard=false skips the guard side of init")
	cmd.Flags().BoolVar(&flagSkipGuardDialect, "skip-guard-dialect", false, "Skip writing native guard permission rules into tool configs (narrower than --guard=false)")
	cmd.Flags().IntVar(&flagProxyPort, "proxy-port", 8820, "Observer proxy port to wire into per-tool routing config")
	cmd.Flags().StringVar(&flagConfigPath, "config", "", "Path to observer config.toml — when set, registered hook + MCP commands include --config so they read the same config as the proxy you'll run against this install")
	return cmd
}

// initSelectedTools recomputes the wireAIClients tool selection for
// the guard-dialect step (the P6.x init refactor moved selection
// inside wireAIClients). Detection-only: dry-run registries, zero
// writes; any construction failure selects nothing — a detection
// problem must never fail the init (the initGuardDialects contract).
func initSelectedTools(binary, configPath string, all, cc, codex, cursor, cline bool) []string {
	hookReg, err := hook.NewRegistry(hook.Options{BinaryPath: binary, DryRun: true, ConfigPath: configPath})
	if err != nil {
		return nil
	}
	mcpReg, err := mcp.NewRegistrar(mcp.RegisterOptions{BinaryPath: binary, DryRun: true, ConfigPath: configPath})
	if err != nil {
		return nil
	}
	return selectTools(all, cc, codex, cursor, cline, unionStrings(hookReg.Installed(), mcpReg.Installed()))
}

// initGuardDialects compiles the effective guard policy into the
// selected tools' native permission dialects (guard spec §13.2 —
// `observer init` is the default-on application point). Only tools
// with an IMPLEMENTED dialect compile (claude-code today; opencode is
// not an init tool and joins via `observer guard compile`); the
// [guard.dialects].targets allow-list is honoured when set. Every
// failure path prints and returns — the hooks/MCP sides already
// registered, and a dialect problem must never fail the init.
func initGuardDialects(ctx context.Context, out io.Writer, tools []string, configPath string, dryRun bool) {
	var names []string
	for _, t := range tools {
		if tgt, ok := compile.TargetFor(t); ok && tgt.Implemented {
			names = append(names, string(tgt.Dialect))
		}
	}
	if len(names) == 0 {
		return
	}
	cfg, g, err := buildCLIGuard(configPath)
	if err != nil {
		fmt.Fprintf(out, "guard dialects: skipped (%v)\n", err)
		return
	}
	if !cfg.Guard.Enabled || cfg.Guard.Mode == "off" || !cfg.Guard.Dialects.Compile {
		fmt.Fprintln(out, "guard dialects: skipped (disabled via [guard] / [guard.dialects])")
		return
	}
	if len(cfg.Guard.Dialects.Targets) > 0 {
		allow := map[string]bool{}
		for _, t := range cfg.Guard.Dialects.Targets {
			allow[t] = true
		}
		kept := names[:0]
		for _, n := range names {
			if allow[n] {
				kept = append(kept, n)
			}
		}
		if names = kept; len(names) == 0 {
			return
		}
	}
	var st *store.Store
	if !dryRun {
		if database, derr := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath}); derr == nil {
			defer database.Close()
			st = store.New(database)
		} else {
			fmt.Fprintf(out, "guard dialects: WARN observer DB unavailable (%v) — compiling without pinning\n", derr)
		}
	}
	r := newDialectRunner(configGuardDialects{
		Compile: true, Targets: cfg.Guard.Dialects.Targets,
	}, st, g, newLogger(cfg.Observer.LogLevel))
	if r == nil {
		return
	}
	// Explicit tool selection creates the config file when absent
	// (requireExisting=false) — the same posture as hook registration.
	reports := r.CompileTargets(ctx, names, !dryRun, false)
	for _, rep := range reports {
		switch {
		case rep.Target.Dialect == "":
			// candidate-resolution issues only (printed below).
		case dryRun:
			fmt.Fprintf(out, "guard dialect %s: would compile %d native entr%s (%d to add, %d to retire) into %s\n",
				rep.Target.Dialect, rep.Entries, pluralIES(rep.Entries), len(rep.Added), len(rep.Removed), rep.Path)
		case rep.Wrote:
			fmt.Fprintf(out, "guard dialect %s: wrote %d native entr%s (%d added, %d retired) → %s\n",
				rep.Target.Dialect, rep.Entries, pluralIES(rep.Entries), len(rep.Added), len(rep.Removed), rep.Path)
		default:
			fmt.Fprintf(out, "guard dialect %s: already in sync (%d native entr%s) — %s\n",
				rep.Target.Dialect, rep.Entries, pluralIES(rep.Entries), rep.Path)
		}
		for _, issue := range rep.Issues {
			fmt.Fprintf(out, "guard dialect: ISSUE: %s\n", issue)
		}
	}
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
	// HomeDir overrides every registry's home resolution (tests only —
	// the same seam interactiveInitOptions carries). "" = real home.
	HomeDir string
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
// Used by both newInitCmd (batch mode delegates here — the M4.2
// follow-up landed with the post-D18 dedup) and newEnrollCmd.
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
		HomeDir:    opts.HomeDir,
	})
	if err != nil {
		return nil, "", "", false, err
	}
	mcpReg, err := mcp.NewRegistrar(mcp.RegisterOptions{
		BinaryPath: binary,
		DryRun:     opts.DryRun,
		Force:      opts.Force,
		ConfigPath: opts.ConfigPath,
		HomeDir:    opts.HomeDir,
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
			HomeDir:   opts.HomeDir,
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
			// Dispatch on the route's capability KIND, not tool name
			// (CLAUDE.md #3). proxyroute.Registrar holds the per-client
			// writers; the registry says which kind each tool needs.
			// routeSupported guarantees a non-nil Proxy.
			ic, _ := integration.For(t)
			switch ic.Proxy.Kind {
			case integration.RouteConfigFile:
				printProxyRouteResult(&buf, t, routeReg.RegisterCodex(), opts.DryRun)
			case integration.RouteEnvSettings:
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

// hookSupported reports whether `observer init`'s hook.Registry can write
// hooks for tool. Registry-driven (CLAUDE.md #3/#5): it dispatches on the
// tool's HookMechanism, not its name, and treats the cross-OS "-windows"
// variant as the CrossOSBridge capability rather than a separate tool.
// The hook.Registry handles the settings.json / cursor / codex.toml
// mechanisms; Hermes' embedded plugin is AutoWired but written by
// runHermesInit (separate `observer init --hermes` path), and cline-cli's
// hooks.jsonl is a tailer the operator wires manually (AutoWired=false) —
// both are excluded here by mechanism, not by a tool branch. A new
// hook-capable client is a registry row + (if a new mechanism) one writer.
func hookSupported(tool string) bool {
	base, isWindows := strings.CutSuffix(tool, "-windows")
	c, ok := integration.For(base)
	if !ok || !c.Hook.AutoWired {
		return false
	}
	switch c.Hook.Mechanism {
	case integration.HookClaudeSettings, integration.HookCursor, integration.HookCodexConfig:
		// handled by hook.Registry.Register
	default:
		return false // HermesPlugin → runHermesInit; ClineCLIJSONL → manual tailer
	}
	if isWindows {
		return c.Hook.CrossOSBridge
	}
	return true
}

// mcpSupported reports whether `observer init` can write MCP server config
// for tool via the mcp.Registrar. It is registry-driven (CLAUDE.md #3/#5):
// the answer is the SHAPE of the tool's MCP capability, not its name. The
// registrar handles the JSON {"mcpServers":{…}} object, Codex's
// [mcp_servers] TOML table, and OpenCode's "mcp" object; Hermes' YAML
// target is Implemented but written by its own runHermesInit path, so it's
// excluded here by format, not by a tool branch. A new MCP-capable client
// becomes a registry row + (if a new format) one writer — no edit to this
// predicate.
func mcpSupported(tool string) bool {
	// Resolve the cross-OS "-windows" virtual target to its base adapter and
	// gate on the registry's MCP.CrossOSBridge flag (mirrors hookSupported).
	base, isWindows := strings.CutSuffix(tool, "-windows")
	c, ok := integration.For(base)
	if !ok || c.MCP == nil || !c.MCP.Implemented {
		return false
	}
	if isWindows && !c.MCP.CrossOSBridge {
		return false
	}
	switch c.MCP.Format {
	case integration.MCPServersJSON, integration.MCPCodexTOML, integration.MCPOpenCodeJSON:
		return true
	default:
		return false
	}
}

// routeSupported reports whether `observer init` can PERSIST a proxy route
// into a per-tool config file. Registry-driven (CLAUDE.md #3/#5): true only
// for the persisted route kinds the proxyroute.Registrar writes —
// RouteEnvSettings (claude-code → ~/.claude/settings.json "env") and
// RouteConfigFile (codex → ~/.codex/config.toml openai_base_url).
// RouteLauncher tools (opencode) are routed at exec time by their
// `observer <x>` launcher, not persisted by init, so they're false here.
// Proxy-exempt tools (Proxy == nil) are likewise false. A new routable
// client is a registry row + (if a new kind) one writer.
func routeSupported(tool string) bool {
	c, ok := integration.For(tool)
	if !ok || c.Proxy == nil {
		return false
	}
	switch c.Proxy.Kind {
	case integration.RouteEnvSettings, integration.RouteConfigFile:
		return true
	default:
		return false
	}
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
		"codex":    true,
		"opencode": true,
		"cline":    true, "cline-windows": true,
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
