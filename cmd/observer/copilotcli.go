// copilotcli.go — `observer copilot-cli` launcher subcommand.

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newCopilotCLICmd implements `observer copilot-cli` — points GitHub Copilot
// CLI's BYOK (bring-your-own-key) custom provider at the observer proxy's
// OpenAI-compatible endpoint via the COPILOT_PROVIDER_* env vars, then execs
// `copilot`.
//
// BYOK only: GitHub's native hosted-model routing is NOT proxyable (that
// traffic stays exempt). This launcher sets COPILOT_PROVIDER_TYPE,
// COPILOT_PROVIDER_BASE_URL and (optionally) COPILOT_MODEL. It deliberately
// does NOT set COPILOT_PROVIDER_API_KEY — that secret must already be in the
// environment (implementation rule: a launcher writes only base-URL fields,
// never keys). A pre-flight warns when the key is absent so the BYOK turn
// doesn't silently fail.
//
// PROBE STATUS (2026-06-26): integration registry has copilot-cli at
// Routability=probe_required. Confirm a live turn lands an api_turns row
// before the matrix PROXY cell is flipped on.
func newCopilotCLICmd() *cobra.Command {
	var (
		configPath   string
		proxyURL     string
		binPath      string
		model        string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:   "copilot-cli [-- copilot-args...]",
		Short: "Launch GitHub Copilot CLI BYOK traffic through the observer proxy (probe)",
		Long: "Wraps `copilot` (GitHub Copilot CLI) in BYOK mode: sets\n" +
			"COPILOT_PROVIDER_TYPE=openai and COPILOT_PROVIDER_BASE_URL to the\n" +
			"observer proxy's OpenAI-compatible endpoint (…/v1), plus\n" +
			"COPILOT_MODEL when --model is given.\n\n" +
			"BYOK ONLY — native GitHub-hosted Copilot routing is not proxyable\n" +
			"and stays exempt. Your COPILOT_PROVIDER_API_KEY must already be in\n" +
			"the environment; this launcher never sets or moves it.\n\n" +
			"PROBE: confirm a turn lands an api_turns row before trusting it.\n\n" +
			"All arguments after the subcommand are forwarded to copilot. Use\n" +
			"`--` to separate observer flags from copilot flags. Requires a\n" +
			"running observer proxy (`observer start`).",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved := resolveProxyURL(cfg.Proxy.Port, proxyURL)
			bin, err := resolveToolBin("copilot", binPath, "--copilot-path")
			if err != nil {
				return err
			}

			// Pre-flight: BYOK needs a provider key, which we never set.
			if os.Getenv("COPILOT_PROVIDER_API_KEY") == "" {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"observer copilot-cli: warning — COPILOT_PROVIDER_API_KEY is not set. BYOK mode needs it; "+
						"export your provider key before launching (this launcher never sets it).")
			}

			env := map[string]string{
				"COPILOT_PROVIDER_TYPE":     "openai",
				"COPILOT_PROVIDER_BASE_URL": strings.TrimRight(resolved, "/") + "/v1",
			}
			if model != "" {
				env["COPILOT_MODEL"] = model
			}
			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "copilot-cli",
					label:       "copilot",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// copilot -i/--interactive <prompt>: "Start interactive
					// mode and automatically execute this prompt." -p/--prompt
					// is the headless non-interactive form — flag both as
					// conflicts. The prompt rides as the -i VALUE, so no bare
					// positional handling is needed.
					inject: promptInjection{
						Kind:          injectFlagValue,
						Flag:          "-i",
						ConflictFlags: []string{"-i", "--interactive", "-p", "--prompt"},
						Subcommands:   copilotSubcommands,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer copilot: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}
			return runEnvLauncher(envLauncherSpec{
				tool:     "copilot-cli",
				bin:      bin,
				args:     args,
				dir:      continueDir,
				proxyURL: resolved,
				env:      env,
				stderr:   cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&binPath, "copilot-path", "", "Path to the copilot binary (default: resolve `copilot` on PATH)")
	cmd.Flags().StringVar(&model, "model", "", "Set COPILOT_MODEL for the BYOK provider (optional)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it via copilot's -i/--interactive flag (delivery=inject_prompt). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// copilotSubcommands are the copilot argv tokens that are subcommands, not a
// prompt — so continue-from's injection detector does not mistake them for a
// forwarded prompt. Sourced from `copilot --help`.
var copilotSubcommands = map[string]bool{
	"completion": true,
	"help":       true,
	"init":       true,
	"login":      true,
	"logout":     true,
	"mcp":        true,
	"plugin":     true,
	"skill":      true,
	"update":     true,
	"version":    true,
}
