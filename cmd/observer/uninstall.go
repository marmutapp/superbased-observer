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

// newUninstallCmd implements `observer uninstall` — reverse of `init`.
// Removes hook entries and the MCP server registration from each selected
// AI coding tool's config, preserving any user-authored entries. Checksum
// tracking from `init` is consulted to refuse changes to files that have
// drifted (unless --force is given). Optionally --purge also deletes
// ~/.observer/ (DB + config + checksums registry).
func newUninstallCmd() *cobra.Command {
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
		flagPurge      bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove observer hooks + MCP server from AI coding tools",
		Long: `Remove observer's hook entries and MCP server registration from each
selected AI coding tool's config. Only entries whose command matches the
observer binary are removed; user-authored hooks are preserved.

By default the tool data dir (~/.observer/) is kept so a later re-install
retains history. Pass --purge to delete it.`,
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

			installed := unionStrings(hookReg.Installed(), mcpReg.Installed())
			tools := selectTools(flagAll, flagClaudeCode, flagCodex, flagCursor, flagCline, installed)
			if len(tools) == 0 && !flagPurge {
				fmt.Fprintln(cmd.OutOrStdout(),
					"no tools selected and none auto-detected — pass --claude-code / --cursor / --codex / --all / --purge")
				return nil
			}

			out := cmd.OutOrStdout()
			anyError := false
			for _, t := range tools {
				if !flagSkipHooks && hookSupported(t) {
					res := hookReg.Unregister(t)
					printHookUnregisterResult(out, t, res, flagDryRun)
					if res.Error != nil {
						anyError = true
					}
				}
				if !flagSkipMCP && mcpSupported(t) {
					res := mcpReg.Unregister(t)
					printMCPUnregisterResult(out, t, res, flagDryRun)
					if res.Error != nil {
						anyError = true
					}
				}
			}

			if flagPurge {
				if err := purgeDataDir(out, flagDryRun); err != nil {
					fmt.Fprintf(out, "%-12s data ✗ %v\n", "observer", err)
					anyError = true
				}
			}

			if anyError {
				return fmt.Errorf("uninstall completed with errors; see above")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&flagClaudeCode, "claude-code", false, "Unregister from Claude Code")
	cmd.Flags().BoolVar(&flagCodex, "codex", false, "Unregister from OpenAI Codex")
	cmd.Flags().BoolVar(&flagCursor, "cursor", false, "Unregister from Cursor")
	cmd.Flags().BoolVar(&flagCline, "cline", false, "Unregister from Cline / Roo Code")
	cmd.Flags().BoolVar(&flagAll, "all", false, "Select every detected tool")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "Print intended changes without writing any files")
	cmd.Flags().BoolVar(&flagForce, "force", false, "Bypass checksum mismatch; remove observer entries even if the config file was modified since install")
	cmd.Flags().BoolVar(&flagSkipHooks, "skip-hooks", false, "Only unregister MCP, leave hooks alone")
	cmd.Flags().BoolVar(&flagSkipMCP, "skip-mcp", false, "Only unregister hooks, leave MCP alone")
	cmd.Flags().BoolVar(&flagPurge, "purge", false, "Also delete ~/.observer/ (observer.db, config.toml, hook_checksums.json)")
	return cmd
}

func printHookUnregisterResult(out io.Writer, tool string, res hook.UnregistrationResult, dryRun bool) {
	if res.Error != nil {
		fmt.Fprintf(out, "%-12s hook ✗ %v\n", tool, res.Error)
		return
	}
	if res.Skipped {
		fmt.Fprintf(out, "%-12s hook no-op (no observer entries in %s)\n", tool, res.ConfigPath)
		return
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	if len(res.HooksRemoved) > 0 {
		fmt.Fprintf(out, "%-12s hook %s %d hook(s) from %s: %v\n",
			tool, verb, len(res.HooksRemoved), res.ConfigPath, res.HooksRemoved)
	} else {
		fmt.Fprintf(out, "%-12s hook no observer entries found in %s\n", tool, res.ConfigPath)
	}
	if len(res.HooksKept) > 0 {
		fmt.Fprintf(out, "%-12s hook preserved user entries in: %v\n", tool, res.HooksKept)
	}
}

func printMCPUnregisterResult(out io.Writer, tool string, res mcp.UnregistrationResult, dryRun bool) {
	if res.Error != nil {
		fmt.Fprintf(out, "%-12s mcp  ✗ %v\n", tool, res.Error)
		return
	}
	if res.Skipped {
		fmt.Fprintf(out, "%-12s mcp  no-op (no observer entry in %s)\n", tool, res.ConfigPath)
		return
	}
	verb := "removed"
	if dryRun {
		verb = "would remove"
	}
	if res.Removed {
		fmt.Fprintf(out, "%-12s mcp  %s observer entry from %s\n", tool, verb, res.ConfigPath)
	}
}

// purgeDataDir removes ~/.observer/ after --purge. Best-effort: missing
// dir is not an error; residual lock-holding processes surface as errors.
func purgeDataDir(out io.Writer, dryRun bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home: %w", err)
	}
	dir := filepath.Join(home, ".observer")
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "%-12s data no-op (%s not found)\n", "observer", dir)
			return nil
		}
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	verb := "deleted"
	if dryRun {
		verb = "would delete"
	}
	fmt.Fprintf(out, "%-12s data %s %s\n", "observer", verb, dir)
	if dryRun {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove %s: %w", dir, err)
	}
	return nil
}
