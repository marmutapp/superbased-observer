// hermes.go — `observer hermes` launcher subcommand.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// hermesProviderName is the user-config provider entry the launcher ensures
// in ~/.hermes/config.yaml. A distinct name so it never collides with
// hermes' built-in providers (openrouter/nous/custom — see
// docs/proxy-routing-blockers.md). Selected per-invocation via hermes'
// top-level `--provider` flag, which accepts a name from the config's
// `providers:` section (hermes_cli/_parser.py).
const hermesProviderName = "observer"

// hermesDefaultUpstream is the `[proxy.upstreams]` id the observer provider's
// base URL routes through by default. Hermes' canonical aggregator is
// OpenRouter (the operator's live default), and the proxy forwards
// /up/openrouter/api/v1 → https://openrouter.ai (config.toml
// [proxy.upstreams] openrouter). Override with --upstream for a different
// upstream you have wired in observer's config.
const hermesDefaultUpstream = "openrouter"

// hermesDefaultKeyEnv is the env var NAME the observer provider's `key_env`
// points at. hermes reads the actual credential from this env var at
// runtime — the launcher NEVER writes a key to disk. OpenRouter-bound hermes
// reads its key from OPENROUTER_API_KEY by convention; override with
// --key-env to name a different env var.
const hermesDefaultKeyEnv = "OPENROUTER_API_KEY"

// newHermesCmd implements `observer hermes` — routes Hermes Agent (Nous
// Research) through the observer proxy.
//
// Unlike the env-based launchers (opencode / copilot-cli), hermes' NAMED
// providers (openrouter / nous) hardcode their endpoint and IGNORE
// model.base_url / OPENAI_BASE_URL — confirmed live 2026-06-27. hermes'
// routable mechanism is a user-config provider in ~/.hermes/config.yaml's
// `providers:` section carrying an explicit `base_url`, selected per
// invocation via hermes' `--provider` flag. This launcher ensures an
// "observer" provider whose base URL is the proxy's /up/<upstream>/api/v1
// endpoint (so OpenRouter-bound, OpenAI-shaped traffic is captured +
// compressed), then execs `hermes --provider observer`. A live turn through
// the equivalent config landed an api_turns row (verified 2026-06-27,
// provider=openai, nvidia/nemotron-…:free, HTTP 200).
//
// NEVER writes a secret: [REDACTED] provider's `key_env` is the NAME of an env
// var (default OPENROUTER_API_KEY), which hermes resolves at runtime — the
// key stays in your environment. The write is ADDITIVE: only the
// `providers.observer` entry is touched, so your top-level `model:` block
// (default provider/model) is left exactly as-is.
func newHermesCmd() *cobra.Command {
	var (
		configPath string
		proxyURL   string
		binPath    string
		upstream   string
		keyEnv     string
	)
	cmd := &cobra.Command{
		Use:   "hermes [-- hermes-args...]",
		Short: "Launch Hermes Agent with traffic routed through the observer proxy",
		Long: "Wraps `hermes` (Hermes Agent, Nous Research) so its model traffic\n" +
			"flows through the observer proxy. hermes' named providers ignore\n" +
			"model.base_url / OPENAI_BASE_URL, so observer routes it via a\n" +
			"user-config provider in ~/.hermes/config.yaml (an `observer` provider\n" +
			"whose base_url is the proxy's /up/<upstream>/api/v1 endpoint), then\n" +
			"runs `hermes --provider " + hermesProviderName + "`.\n\n" +
			"The provider entry is merged in ADDITIVELY — your top-level `model:`\n" +
			"block and other providers are preserved. NEVER writes a secret: [REDACTED] " +
			"provider's `key_env` is the NAME `" + hermesDefaultKeyEnv + "` (override\n" +
			"with --key-env), which hermes resolves from your environment at\n" +
			"runtime. Export your OpenRouter key under that name before running.\n\n" +
			"Routing requires a matching `[proxy.upstreams]` entry in observer's\n" +
			"config.toml (default upstream `" + hermesDefaultUpstream + "`). All\n" +
			"arguments after the subcommand are forwarded to hermes. Use `--` to\n" +
			"separate observer flags from hermes flags. Requires a running\n" +
			"observer proxy (`observer start`).",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			resolved := resolveProxyURL(cfg.Proxy.Port, proxyURL)
			bin, err := resolveToolBin("hermes", binPath, "--hermes-path")
			if err != nil {
				return err
			}
			providerBase := strings.TrimRight(resolved, "/") + "/up/" + upstream + "/api/v1"
			if err := ensureHermesObserverProvider(providerBase, keyEnv); err != nil {
				return fmt.Errorf("ensure hermes provider: %w", err)
			}
			// hermes' `--provider` is a top-level flag; prepend it so the
			// observer provider is selected. The user's args follow and may
			// add -z/--model/etc.
			forwarded := append([]string{"--provider", hermesProviderName}, args...)
			return runEnvLauncher(envLauncherSpec{
				tool:     "hermes",
				bin:      bin,
				args:     forwarded,
				proxyURL: resolved,
				env:      nil, // hermes routes via config.yaml, not an env var
				stderr:   cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to observer config.toml")
	cmd.Flags().StringVar(&proxyURL, "proxy-url", "", "override the proxy base URL (default from config)")
	cmd.Flags().StringVar(&binPath, "hermes-path", "", "path to the hermes binary (default: looked up on PATH)")
	cmd.Flags().StringVar(&upstream, "upstream", hermesDefaultUpstream, "the [proxy.upstreams] id to route through")
	cmd.Flags().StringVar(&keyEnv, "key-env", hermesDefaultKeyEnv, "env var NAME hermes reads the provider key from (never written to disk)")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// ensureHermesObserverProvider idempotently adds/merges the `observer`
// provider into ~/.hermes/config.yaml's `providers:` section with the given
// OpenAI-compatible base URL and key_env name. The edit is ADDITIVE — only
// the providers.observer entry is written; every other key (the top-level
// model block, other providers) is preserved, so repeated runs and a changed
// proxy port both converge without disturbing the operator's defaults. The
// config is reserialized via yaml.v3 (matching proxyroute.RegisterHermes's
// established whole-file rewrite), backed up to config.yaml.bak first. The
// key_env value is the env var NAME, never a literal key.
func ensureHermesObserverProvider(baseURL, keyEnv string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".hermes", "config.yaml")
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		// hermes must be configured first — we don't author a fresh config
		// (it would lack the operator's model/provider defaults).
		return fmt.Errorf("hermes config.yaml not found (%s) — run `hermes setup` once first: %w", path, rerr)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	providers, _ := doc["providers"].(map[string]any)
	if providers == nil {
		providers = map[string]any{}
	}
	providers[hermesProviderName] = map[string]any{
		"name":      "Observer Proxy",
		"base_url":  baseURL,
		"key_env":   keyEnv, // env var NAME — hermes resolves it; no secret on disk
		"transport": "openai_chat",
	}
	doc["providers"] = providers

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	// Back up before the rewrite (yaml reserialization is lossier than JSON;
	// mirrors proxyroute.RegisterHermes's .bak discipline) so the operator's
	// prior config is recoverable.
	if err := os.WriteFile(path+".bak", data, 0o600); err != nil {
		return fmt.Errorf("backup %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}
