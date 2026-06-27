// openclaw.go — `observer openclaw` launcher subcommand.

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newOpenclawCmd implements `observer openclaw` — sets OPENAI_BASE_URL to the
// observer proxy's OpenAI-compatible endpoint and execs `openclaw` so its
// `openai`-provider model traffic flows through the proxy.
//
// GROUNDED (2026-06-26, live WSL install): OpenClaw's bundled `openai` plugin
// reads OPENAI_BASE_URL / OPENAI_API_BASE
// (plugin-runtime-deps/.../extensions/openai), and the operator's default
// model is on the `openai` provider — so the env redirect routes real
// traffic. The `openai-codex` provider is OAuth (ChatGPT-backed) and is NOT
// affected by this env. OpenClaw also fronts model calls with its own local
// gateway; confirm a live turn lands an api_turns row before relying on it,
// hence the PROBE label.
func newOpenclawCmd() *cobra.Command {
	var (
		configPath string
		proxyURL   string
		binPath    string
	)
	cmd := &cobra.Command{
		Use:   "openclaw [-- openclaw-args...]",
		Short: "Launch OpenClaw with openai-provider traffic routed through the observer proxy (probe)",
		Long: "Wraps `openclaw` with OPENAI_BASE_URL pointed at the observer\n" +
			"proxy's OpenAI-compatible endpoint (…/v1). OpenClaw's `openai`\n" +
			"plugin honors OPENAI_BASE_URL, so traffic on the `openai` provider\n" +
			"routes through the proxy. The `openai-codex` (OAuth) provider is\n" +
			"unaffected.\n\n" +
			"PROBE: confirm a turn lands an api_turns row before relying on it\n" +
			"(OpenClaw fronts calls with its own local gateway).\n\n" +
			"All arguments after the subcommand are forwarded to openclaw. Use\n" +
			"`--` to separate observer flags from openclaw flags. NEVER touches\n" +
			"API keys. Requires a running observer proxy (`observer start`).",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved := resolveProxyURL(cfg.Proxy.Port, proxyURL)
			bin, err := resolveToolBin("openclaw", binPath, "--openclaw-path")
			if err != nil {
				return err
			}
			return runEnvLauncher(envLauncherSpec{
				tool:     "openclaw",
				bin:      bin,
				args:     args,
				proxyURL: resolved,
				env:      map[string]string{"OPENAI_BASE_URL": strings.TrimRight(resolved, "/") + "/v1"},
				stderr:   cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&binPath, "openclaw-path", "", "Path to the openclaw binary (default: resolve `openclaw` on PATH)")
	cmd.Flags().SetInterspersed(false)
	return cmd
}
