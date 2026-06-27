// gemini.go — `observer gemini` launcher subcommand.

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newGeminiCmd implements `observer gemini` — sets GOOGLE_GEMINI_BASE_URL to
// the observer proxy root and execs `gemini` (Google Gemini CLI) so its
// generateContent traffic flows through the proxy for token capture.
//
// Unlike the OpenAI-compatible launchers, the env var points at the proxy
// ROOT (no /v1 suffix): gemini-cli appends the full
// /v1beta/models/<model>:generateContent path itself. The proxy's Phase-E
// Gemini bridge (internal/proxy: providerForPath → ProviderGoogle, the
// generativelanguage upstream, parseGeminiResponse) recognizes that path,
// forwards to generativelanguage.googleapis.com, and parses usageMetadata.
//
// GROUNDED (2026-06-27, live install): gemini-cli honors GOOGLE_GEMINI_BASE_URL
// (confirmed in the @google/gemini-cli module). NEVER touches the API key —
// it rides the request (header / ?key=) untouched.
func newGeminiCmd() *cobra.Command {
	var (
		configPath string
		proxyURL   string
		binPath    string
	)
	cmd := &cobra.Command{
		Use:   "gemini [-- gemini-args...]",
		Short: "Launch Gemini CLI with traffic routed through the observer proxy (probe)",
		Long: "Wraps `gemini` (Google Gemini CLI) with GOOGLE_GEMINI_BASE_URL\n" +
			"pointed at the observer proxy root. The proxy's Gemini bridge\n" +
			"recognizes the generateContent path, forwards to Google, and\n" +
			"captures usageMetadata as accurate token counts.\n\n" +
			"PROBE: confirm a turn lands an api_turns row (provider=google).\n\n" +
			"All arguments after the subcommand are forwarded to gemini. Use\n" +
			"`--` to separate observer flags from gemini flags. NEVER touches\n" +
			"the API key. Requires a running observer proxy (`observer start`).",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved := resolveProxyURL(cfg.Proxy.Port, proxyURL)
			bin, err := resolveToolBin("gemini", binPath, "--gemini-path")
			if err != nil {
				return err
			}
			return runEnvLauncher(envLauncherSpec{
				tool:     "gemini",
				bin:      bin,
				args:     args,
				proxyURL: resolved,
				// Gemini base URL is the host ROOT (no /v1) — the CLI appends
				// the /v1beta/models/<model>:generateContent path itself.
				env:    map[string]string{"GOOGLE_GEMINI_BASE_URL": strings.TrimRight(resolved, "/")},
				stderr: cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&binPath, "gemini-path", "", "Path to the gemini binary (default: resolve `gemini` on PATH)")
	cmd.Flags().SetInterspersed(false)
	return cmd
}
