// pi.go — `observer pi` launcher subcommand.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// piProviderName is the custom pi provider entry the launcher ensures in
// models.json. Distinct name so it never collides with pi's built-in
// "openai" provider (which hardcodes api.openai.com and ignores
// OPENAI_BASE_URL — see docs/proxy-routing-blockers.md).
const piProviderName = "observer"

// newPiCmd implements `observer pi` — routes pi (@mariozechner/pi-coding-agent)
// through the observer proxy.
//
// Unlike the env-based launchers (opencode / cline-cli), pi's built-in
// providers IGNORE OPENAI_BASE_URL — confirmed live 2026-06-27 (a dead-port
// base URL still reached OpenAI). pi's documented route is a CUSTOM PROVIDER
// in ~/.pi/agent/models.json carrying an explicit `baseUrl` (docs/models.md).
// This launcher ensures an "observer" provider pointed at the proxy's
// OpenAI-compatible endpoint, then execs `pi --provider observer`. A live
// turn through it lands an api_turns row (verified 2026-06-27, gpt-4o).
//
// NEVER writes a secret: the provider's `apiKey` is the NAME of an env var
// (`OPENAI_API_KEY`), which pi resolves at runtime — the key stays in your
// environment. Provide it via `OPENAI_API_KEY` or pi's `--api-key` flag.
func newPiCmd() *cobra.Command {
	var (
		configPath string
		proxyURL   string
		binPath    string
	)
	cmd := &cobra.Command{
		Use:   "pi [-- pi-args...]",
		Short: "Launch pi with traffic routed through the observer proxy",
		Long: "Wraps `pi` (@mariozechner/pi-coding-agent) so its model traffic\n" +
			"flows through the observer proxy. pi's built-in providers ignore\n" +
			"OPENAI_BASE_URL, so observer routes it via a custom provider in\n" +
			"~/.pi/agent/models.json (an `observer` provider whose baseUrl is the\n" +
			"proxy's OpenAI-compatible endpoint), then runs `pi --provider " + piProviderName + "`.\n\n" +
			"The provider entry is merged in idempotently — your other pi\n" +
			"providers are preserved. NEVER writes a secret: the provider's\n" +
			"apiKey is the NAME `OPENAI_API_KEY`, which pi resolves from your\n" +
			"environment at runtime. Provide your key via OPENAI_API_KEY or pi's\n" +
			"--api-key flag.\n\n" +
			"All arguments after the subcommand are forwarded to pi. Use `--` to\n" +
			"separate observer flags from pi flags. Requires a running observer\n" +
			"proxy (`observer start`).",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved := resolveProxyURL(cfg.Proxy.Port, proxyURL)
			bin, err := resolveToolBin("pi", binPath, "--pi-path")
			if err != nil {
				return err
			}
			if err := ensurePiObserverProvider(resolved + "/v1"); err != nil {
				return fmt.Errorf("ensure pi provider: %w", err)
			}
			// Prepend the provider selection; the user's args follow (and may
			// override --model / --api-key).
			forwarded := append([]string{"--provider", piProviderName}, args...)
			return runEnvLauncher(envLauncherSpec{
				tool:     "pi",
				bin:      bin,
				args:     forwarded,
				proxyURL: resolved,
				env:      nil, // pi routes via models.json, not an env var
				stderr:   os.Stderr,
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to observer config.toml")
	cmd.Flags().StringVar(&proxyURL, "proxy-url", "", "override the proxy base URL (default from config)")
	cmd.Flags().StringVar(&binPath, "pi-path", "", "path to the pi binary (default: looked up on PATH)")
	return cmd
}

// ensurePiObserverProvider idempotently writes/merges the `observer` provider
// into ~/.pi/agent/models.json with the given OpenAI-compatible base URL. Any
// existing providers are preserved; only the observer entry is (re)written, so
// repeated runs and a changed proxy port both converge. The apiKey is the env
// var NAME, never a literal key.
func ensurePiObserverProvider(baseURL string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".pi", "agent")
	path := filepath.Join(dir, "models.json")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	root := map[string]any{}
	if data, rerr := os.ReadFile(path); rerr == nil && len(data) > 0 {
		if jerr := json.Unmarshal(data, &root); jerr != nil {
			return fmt.Errorf("parse existing %s: %w", path, jerr)
		}
	} else if rerr != nil && !os.IsNotExist(rerr) {
		return rerr
	}

	providers, _ := root["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	providers[piProviderName] = map[string]any{
		"baseUrl": baseURL,
		"api":     "openai-completions",
		"apiKey":  "OPENAI_API_KEY", // env var NAME — pi resolves it; no secret on disk
		"models": []any{
			piModel("gpt-4o", "gpt-4o (observer proxy)"),
			piModel("gpt-4o-mini", "gpt-4o-mini (observer proxy)"),
		},
	}
	root["providers"] = providers

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	// Atomic-ish write: temp + rename so a crash can't truncate the file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// piModel builds one model entry for the observer provider's models list.
func piModel(id, name string) map[string]any {
	return map[string]any{
		"id":            id,
		"name":          name,
		"reasoning":     false,
		"input":         []any{"text"},
		"contextWindow": 128000,
		"maxTokens":     16384,
	}
}
