// clinecli.go — `observer cline-cli` launcher subcommand.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// clineCompatProvider is the cline provider id whose base URL IS configurable
// (the native "openai" provider hardcodes api.openai.com and ignores
// OPENAI_BASE_URL — live-confirmed 2026-06-27; see docs/proxy-routing-blockers.md).
const clineCompatProvider = "openai-compatible"

// newClineCLICmd implements `observer cline-cli` — sets OPENAI_BASE_URL to
// the observer proxy's OpenAI-compatible endpoint and execs the user's
// `cline` (npm `cline` 3.x CLI) binary so its model traffic flows through
// the proxy.
//
// PROBE STATUS (2026-06-26): the live install's providers.json has no
// base-URL key and no config chokepoint to write — but a launch-time
// OPENAI_BASE_URL env MAY redirect it (integration registry: cline-cli
// Routability=probe_required). This launcher is the convenience that lets
// the operator run that probe in one command. Until a live turn confirms an
// api_turns row, the adapter matrix keeps cline-cli's PROXY cell empty
// (SURFACE=probe) — the launcher existing does NOT claim verified routing.
func newClineCLICmd() *cobra.Command {
	var (
		configPath string
		proxyURL   string
		binPath    string
	)
	cmd := &cobra.Command{
		Use:   "cline-cli [-- cline-args...]",
		Short: "Launch Cline CLI with traffic routed through the observer proxy (probe)",
		Long: "Wraps `cline` (the npm Cline CLI) with OPENAI_BASE_URL pointed at\n" +
			"the observer proxy's OpenAI-compatible endpoint (…/v1).\n\n" +
			"PROBE: cline-cli's honoring of OPENAI_BASE_URL is unconfirmed on a\n" +
			"live install. Run one turn through this launcher, then check that a\n" +
			"new api_turns row appeared (`observer doctor cline-cli` or the\n" +
			"dashboard). The adapter matrix keeps cline-cli's PROXY cell empty\n" +
			"until that verification.\n\n" +
			"All arguments after the subcommand are forwarded to cline. Use `--`\n" +
			"to separate observer flags from cline flags. NEVER touches API keys\n" +
			"— your provider credentials must already be in the environment.\n\n" +
			"Requires a running observer proxy (`observer start`).",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved := resolveProxyURL(cfg.Proxy.Port, proxyURL)
			bin, err := resolveToolBin("cline", binPath, "--cline-path")
			if err != nil {
				return err
			}
			if err := ensureClineCompatBaseURL(strings.TrimRight(resolved, "/") + "/v1"); err != nil {
				return err
			}
			// cline routes via the openai-compatible provider's persisted
			// baseUrl, NOT an env var — so select that provider and forward the
			// user's args. No env injection.
			return runEnvLauncher(envLauncherSpec{
				tool:     "cline-cli",
				bin:      bin,
				args:     append([]string{"-P", clineCompatProvider}, args...),
				proxyURL: resolved,
				env:      nil,
				stderr:   cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&binPath, "cline-path", "", "Path to the cline binary (default: resolve `cline` on PATH)")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// ensureClineCompatBaseURL points the openai-compatible provider's persisted
// baseUrl at the proxy in ~/.cline/data/settings/providers.json, preserving
// every other field — including the api key, which it NEVER writes. When the
// provider/key isn't set up yet, it returns guidance to run `cline auth` once,
// since the launcher must not supply a secret.
func ensureClineCompatBaseURL(baseURL string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".cline", "data", "settings", "providers.json")
	authHint := fmt.Sprintf("run `cline auth %s -k <key> -m <model> -b %s` once first", clineCompatProvider, baseURL)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("cline providers.json not found (%s) — %s", path, authHint)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	providers, _ := root["providers"].(map[string]any)
	entry, _ := providers[clineCompatProvider].(map[string]any)
	if entry == nil {
		return fmt.Errorf("the %q cline provider is not configured — %s", clineCompatProvider, authHint)
	}
	settings, _ := entry["settings"].(map[string]any)
	if settings == nil {
		settings = map[string]any{}
		entry["settings"] = settings
	}
	if settings["baseUrl"] == baseURL {
		return nil // already pointed at the proxy
	}
	settings["baseUrl"] = baseURL
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
