package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/hook"
	"github.com/marmutapp/superbased-observer/internal/mcp"
)

// newInitCmd implements `observer init` — registers hooks and the
// MCP server with each selected AI coding tool. A single `init` writes
// settings.json/hooks.json hook entries AND mcp.json/.claude.json/config.toml
// MCP server entries for every supported tool.
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
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Register hooks + MCP server with AI coding tools",
		RunE: func(cmd *cobra.Command, args []string) error {
			binary, err := absoluteBinaryPath()
			if err != nil {
				return err
			}
			hookReg, err := hook.NewRegistry(hook.Options{
				BinaryPath: binary,
				DryRun:     flagDryRun,
				Force:      flagForce,
			})
			if err != nil {
				return err
			}
			mcpReg, err := mcp.NewRegistrar(mcp.RegisterOptions{
				BinaryPath: binary,
				DryRun:     flagDryRun,
				Force:      flagForce,
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

			out := cmd.OutOrStdout()
			registeredClaudeCode := false
			for _, t := range tools {
				if !flagSkipHooks {
					if hookSupported(t) {
						printHookResult(out, t, hookReg.Register(t), flagDryRun)
					}
				}
				if !flagSkipMCP {
					if mcpSupported(t) {
						printMCPResult(out, t, mcpReg.Register(t), flagDryRun)
					}
				}
				if t == "claude-code" {
					registeredClaudeCode = true
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
				printProxyRoutingHint(out)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagClaudeCode, "claude-code", false, "Register hooks + MCP for Claude Code")
	cmd.Flags().BoolVar(&flagCodex, "codex", false, "Register MCP for OpenAI Codex (no hooks — Codex hook system is experimental)")
	cmd.Flags().BoolVar(&flagCursor, "cursor", false, "Register hooks + MCP for Cursor")
	cmd.Flags().BoolVar(&flagCline, "cline", false, "Register for Cline / Roo Code (no hooks; captured via file watcher)")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Select every detected tool")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Print intended changes without writing any files")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Overwrite existing non-observer hook / MCP entries")
	cmd.Flags().BoolVar(&flagSkipHooks, "skip-hooks", false, "Only register MCP, leave hooks alone")
	cmd.Flags().BoolVar(&flagSkipMCP, "skip-mcp", false, "Only register hooks, leave MCP alone")
	return cmd
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
func printProxyRoutingHint(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "next: route Claude Code through the observer proxy for accurate token capture.")
	fmt.Fprintln(out, "  start the proxy:    observer proxy start")
	fmt.Fprintln(out, "  point Claude Code:  export ANTHROPIC_BASE_URL=http://127.0.0.1:8820")
	fmt.Fprintln(out, "  or persist via ~/.claude/settings.json:")
	fmt.Fprintln(out, `      "env": { "ANTHROPIC_BASE_URL": "http://127.0.0.1:8820" }`)
	fmt.Fprintln(out, "  see docs/proxy-routing.md for verification + per-shell setup.")
}

func hookSupported(tool string) bool {
	switch tool {
	case "claude-code", "cursor":
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
	supported := map[string]bool{"claude-code": true, "cursor": true, "codex": true}
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
