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
		configPath string
		proxyURL   string
		binPath    string
		model      string
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
			return runEnvLauncher(envLauncherSpec{
				tool:     "copilot-cli",
				bin:      bin,
				args:     args,
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
	cmd.Flags().SetInterspersed(false)
	return cmd
}
