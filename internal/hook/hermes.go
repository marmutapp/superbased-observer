package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/hook/hermesplugin"
)

// HermesOptions parameterise RegisterHermes / UnregisterHermes /
// RegisterHermesMCP / UnregisterHermesMCP. All fields are optional;
// zero values fall back to the canonical Hermes home discovery
// (~/.hermes on Linux/macOS/WSL, %LOCALAPPDATA%\hermes on Windows
// native, with HERMES_HOME overriding either).
type HermesOptions struct {
	// HermesHome overrides Hermes home discovery. Useful for tests and
	// for operators using `hermes -p <name>` profiles where each
	// profile lives in a separate home.
	HermesHome string
	// BinaryPath is the absolute path to the observer binary. When
	// set, RegisterHermes writes it into the plugin's OBSERVER_BIN
	// environment so the plugin doesn't need PATH lookup. When empty
	// the plugin defaults to "observer" from $PATH.
	BinaryPath string
	// ConfigPath is the absolute path to the observer config.toml.
	// When set, it's written into the plugin's OBSERVER_CONFIG env
	// so the plugin can pass --config to observer's hook subcommand.
	ConfigPath string
	// DryRun: log intended writes but don't touch the filesystem.
	DryRun bool
}

const (
	// HermesPluginDirName is the leaf directory name we write under
	// the user's <hermes-home>/plugins/. Matches the manifest
	// `name:` key so Hermes's loader picks it up cleanly.
	HermesPluginDirName = "superbased-observer"
	// HermesConfigFile is the per-home Hermes config file. We read +
	// merge it for MCP registration; we never write hooks into it
	// (hooks live in the plugin directory).
	HermesConfigFile = "config.yaml"
)

// resolveHermesHome returns the resolved Hermes home directory for
// the running platform. Precedence: explicit HermesOptions.HermesHome
// > HERMES_HOME env > platform-default (~/.hermes or
// %LOCALAPPDATA%\hermes).
func resolveHermesHome(opts HermesOptions) (string, error) {
	if opts.HermesHome != "" {
		return opts.HermesHome, nil
	}
	if env := strings.TrimSpace(os.Getenv("HERMES_HOME")); env != "" {
		return env, nil
	}
	if runtime.GOOS == "windows" {
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
			return filepath.Join(local, "hermes"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("hook.resolveHermesHome: %w", err)
	}
	return filepath.Join(home, ".hermes"), nil
}

// hermesPluginPath returns the full path to the
// `<hermes-home>/plugins/superbased-observer/` directory we write into.
func hermesPluginPath(opts HermesOptions) (string, error) {
	home, err := resolveHermesHome(opts)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "plugins", HermesPluginDirName), nil
}

// RegisterHermes installs the SuperBased Observer Python plugin into
// the user's Hermes plugins directory. Idempotent: re-running against
// an existing install overwrites the plugin files with the embedded
// copies — useful after an `npm i -g @superbased/observer` upgrade
// since the plugin sources may have been refreshed.
//
// Returns the absolute plugin path written and any error. On DryRun
// the path is returned but no filesystem changes happen.
//
// The rendered plugin defaults `OBSERVER_BIN` to opts.BinaryPath so
// the hook subprocess doesn't depend on PATH resolution at runtime
// (validation 2026-06-06 caught this on Windows: the npm shim on
// PATH is broken when node isn't installed, and the plugin's
// fire-and-forget subprocess silently no-op'd against it). When
// BinaryPath is empty the placeholder falls back to the bare
// "observer" PATH lookup — mostly to keep unit tests that don't
// resolve a real executable working; production callers should
// always pass an absolute path.
func RegisterHermes(opts HermesOptions) (string, error) {
	pluginDir, err := hermesPluginPath(opts)
	if err != nil {
		return "", err
	}
	if opts.DryRun {
		return pluginDir, nil
	}
	if err := hermesplugin.WritePlugin(pluginDir, opts.BinaryPath); err != nil {
		return pluginDir, fmt.Errorf("hook.RegisterHermes: %w", err)
	}
	return pluginDir, nil
}

// UnregisterHermes removes the SuperBased Observer plugin directory.
// Safe against an already-uninstalled state (returns nil when the
// directory doesn't exist).
//
// Refuses to remove a directory whose name doesn't match
// HermesPluginDirName — defends against the operator passing a
// mis-configured HermesHome that points at a real Hermes plugin
// they actually want to keep.
func UnregisterHermes(opts HermesOptions) (string, error) {
	pluginDir, err := hermesPluginPath(opts)
	if err != nil {
		return "", err
	}
	if filepath.Base(pluginDir) != HermesPluginDirName {
		return pluginDir, fmt.Errorf("hook.UnregisterHermes: refusing to remove %s — basename %q != %q", pluginDir, filepath.Base(pluginDir), HermesPluginDirName)
	}
	if opts.DryRun {
		return pluginDir, nil
	}
	if _, err := os.Stat(pluginDir); os.IsNotExist(err) {
		return pluginDir, nil
	} else if err != nil {
		return pluginDir, fmt.Errorf("hook.UnregisterHermes: stat: %w", err)
	}
	if err := os.RemoveAll(pluginDir); err != nil {
		return pluginDir, fmt.Errorf("hook.UnregisterHermes: %w", err)
	}
	return pluginDir, nil
}
