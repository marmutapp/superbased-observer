package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newConfigCmd is the `observer config` group (P3.3): a generic
// dotted-key setter over the global config.toml and repo-local
// project override files. Same write owner as the dashboard
// (config.WriteToml's .bak + atomic-rename path); global writes poke
// a running daemon so hot-reloadable consumers (the compression
// profile router) re-read immediately, and project-file writes apply
// to new sessions automatically via the daemon's mtime cache.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Read and edit observer configuration from the command line",
	}
	cmd.AddCommand(newConfigSetCmd())
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	var (
		configPath  string
		projectRoot string
	)
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Set one config key (dotted TOML path) in config.toml or a project override file",
		Long: "Sets a dotted TOML key, e.g.:\n" +
			"  observer config set compression.conversation.target_ratio 0.7\n" +
			"  observer config set profiles.by_provider.openai codex-variant\n" +
			"  observer config set profiles.by_tool.cline codex-safe\n" +
			"  observer config set --project /path/to/repo compression.conversation.enabled false\n\n" +
			"Without --project the key lands in the global config.toml (the same\n" +
			"atomic write+backup path dashboard saves use) and a running daemon is\n" +
			"poked so profile/assignment changes apply to NEW sessions immediately.\n" +
			"With --project the key lands in <root>/.observer/config.toml — the\n" +
			"repo-local override file, which accepts ONLY profiles.* and\n" +
			"compression.* keys (it is world-authored; daemon-level keys are\n" +
			"refused). Project-file edits need no poke: the daemon notices the\n" +
			"file change on the next session.\n\n" +
			"List values are comma-separated; booleans true/false.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]

			if projectRoot != "" {
				if err := config.UpdateProjectOverlay(projectRoot, key, value); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "set %s = %s in %s/%s\n",
					key, value, projectRoot, config.ProjectOverlayFilename)
				fmt.Fprintln(cmd.OutOrStdout(), "applies to this project's NEW sessions automatically (no restart)")
				return nil
			}

			resolvedPath, err := config.ResolveGlobalPath(configPath)
			if err != nil {
				return fmt.Errorf("resolve config path: %w", err)
			}
			cfg, err := config.Load(config.LoadOptions{GlobalPath: resolvedPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if err := config.SetConfigKey(&cfg, key, value); err != nil {
				return err
			}
			// Validate the resulting config before persisting — a value
			// that parses but violates an invariant (port range, mode
			// names) must not land on disk.
			if err := config.Validate(cfg); err != nil {
				return fmt.Errorf("refusing to save: %w", err)
			}
			if err := config.WriteToml(resolvedPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "set %s = %s (saved to %s)\n", key, value, resolvedPath)
			if pokeReload() {
				fmt.Fprintln(cmd.OutOrStdout(), "daemon reloaded — hot-reloadable settings apply to new sessions now")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "no running daemon detected on the dashboard port — applies on next start (or dashboard save)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&projectRoot, "project", "", "Write to <root>/.observer/config.toml instead of the global config (profiles.* and compression.* keys only)")
	return cmd
}
