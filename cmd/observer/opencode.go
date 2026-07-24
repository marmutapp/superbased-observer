// opencode.go — `observer opencode` launcher subcommand.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newOpencodeCmd implements `observer opencode` — sets OPENAI_BASE_URL
// to the observer proxy's OpenAI-compatible endpoint and execs the
// user's `opencode` binary so its model traffic flows through the proxy
// for accurate token capture + compression.
//
// Unlike `observer codex` (which must inject `-c openai_base_url`
// because codex's inner app-server ignores the env var — the V6-2
// gotcha), OpenCode is a plain OpenAI-compatible client that honors
// OPENAI_BASE_URL directly (verified in docs/audits/vultr-teams-
// opencode.md). So this launcher is the simple env-injection shape.
func newOpencodeCmd() *cobra.Command {
	var (
		configPath   string
		proxyURL     string
		opencodePath string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:   "opencode [-- opencode-args...]",
		Short: "Launch OpenCode with traffic routed through the observer proxy",
		Long: "Wraps `opencode` with OPENAI_BASE_URL pointed at the observer\n" +
			"proxy's OpenAI-compatible endpoint (…/v1). OpenCode honors\n" +
			"OPENAI_BASE_URL directly, so no config-file injection is needed\n" +
			"(unlike `observer codex`).\n\n" +
			"All arguments after the subcommand are forwarded to opencode.\n" +
			"Use `--` to separate observer flags from opencode flags:\n" +
			"    observer opencode -- run \"summarize the diff\"\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it via opencode's --prompt flag\n" +
			"(delivery=inject_prompt). See docs/session-handoff.md.\n\n" +
			"Caveat: an Azure-direct provider configured in OpenCode may\n" +
			"bypass OPENAI_BASE_URL (observer then sees local activity but no\n" +
			"proxy-level api_turns). Run `observer doctor opencode` to check.\n\n" +
			"Requires a running observer proxy. Start one with `observer\n" +
			"start` or `observer proxy start` first.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --continue-from: distill a handover from the source session and
			// seed it via opencode's --prompt flag before the launcher builds
			// the child argv. The proxy env (OPENAI_BASE_URL) is injected
			// inside runOpencodeLauncher AFTER arg-build, so only `args`
			// changes here.
			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "opencode",
					label:       "opencode",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// opencode --prompt seeds the interactive TUI's first
					// prompt (the headless one-shot form is the `run`
					// subcommand). opencode's bare positional is a PROJECT
					// PATH, not a prompt, so BarePositionalIsPrompt stays at
					// its zero value — only --prompt collides.
					inject: promptInjection{
						Kind:          injectFlagValue,
						Flag:          "--prompt",
						ConflictFlags: []string{"--prompt"},
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer opencode: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}
			return runOpencodeLauncher(opencodeLauncherOptions{
				configPath:   configPath,
				proxyURL:     proxyURL,
				opencodePath: opencodePath,
				opencodeArgs: args,
				dir:          continueDir,
				stderr:       cmd.ErrOrStderr(),
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&proxyURL, "proxy", "", "Override the observer proxy URL (default: http://127.0.0.1:<cfg.proxy.port>)")
	cmd.Flags().StringVar(&opencodePath, "opencode-path", "", "Path to the opencode binary (default: resolve `opencode` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it via opencode's --prompt flag (delivery=inject_prompt). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

type opencodeLauncherOptions struct {
	configPath   string
	proxyURL     string
	opencodePath string
	opencodeArgs []string
	dir          string // child cwd; "" inherits. Set by --continue-from to the source project root.
	stderr       interface{ Write([]byte) (int, error) }
}

// runOpencodeLauncher resolves the proxy URL, injects OPENAI_BASE_URL,
// and execs opencode with the original argv. Exit code is forwarded via
// exitErr (same shape as the claude/codex launchers).
func runOpencodeLauncher(opts opencodeLauncherOptions) error {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: opts.configPath})
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	proxyURL := resolveProxyURL(cfg.Proxy.Port, opts.proxyURL)

	bin, err := resolveToolBin("opencode", opts.opencodePath, "--opencode-path", opts.configPath, opts.stderr)
	if err != nil {
		return err
	}

	env, baseURL, preset := prepareOpencodeEnv(os.Environ(), proxyURL)
	if preset {
		fmt.Fprintf(opts.stderr,
			"observer opencode: OPENAI_BASE_URL already set in env (%s); using yours.\n", baseURL)
	}

	if !proxyReachable(proxyURL, 250*time.Millisecond) {
		fmt.Fprintf(opts.stderr,
			"observer opencode: warning — proxy not reachable at %s (start it with `observer start`)\n", proxyURL)
	} else {
		fmt.Fprintf(opts.stderr, "observer opencode: routing via %s\n", baseURL)
	}

	child := exec.Command(bin, opts.opencodeArgs...) //nolint:gosec // user-launched tool, args are theirs
	child.Env = env
	child.Dir = opts.dir // "" inherits the caller's cwd; set by --continue-from to the source project root
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if rErr := child.Run(); rErr != nil {
		var ee *exec.ExitError
		if errors.As(rErr, &ee) {
			return exitErr(ee.ExitCode())
		}
		return fmt.Errorf("exec opencode: %w", rErr)
	}
	return nil
}

// prepareOpencodeEnv sets OPENAI_BASE_URL=<proxyURL>/v1 in the child env
// unless the user already exported one (theirs wins — the launcher never
// clobbers explicit env state). Returns the final env, the effective
// base URL, and whether a pre-existing value was kept.
func prepareOpencodeEnv(parent []string, proxyURL string) (env []string, baseURL string, preset bool) {
	target := strings.TrimRight(proxyURL, "/") + "/v1"
	out, _, presets := applyBaseURLEnv(parent, map[string]string{"OPENAI_BASE_URL": target})
	if len(presets) > 0 {
		return out, envValue(out, "OPENAI_BASE_URL"), true
	}
	return out, target, false
}
