// qwen.go — `observer qwen` launcher subcommand.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// newQwenCmd implements `observer qwen` — launches Qwen Code (`qwen`). Its
// primary purpose is `--continue-from`: distill a handover from a source
// session and seed it via `qwen -i "<handover>"` (`-i` ≡
// `--prompt-interactive`: "execute the provided prompt and continue in
// interactive mode"), so a migration INTO Qwen Code opens the interactive
// session pre-loaded with the mission.
//
// NON-PROXIED on purpose. Qwen Code documents an OpenAI-compatible base-URL
// lane (OPENAI_BASE_URL), but no observer-routed turn has confirmed
// api_turns capture (RouteStatusProbeRequired in the integration registry) —
// and overriding OPENAI_BASE_URL would silently redirect an operator's
// already-configured provider through the proxy's DEFAULT upstream, breaking
// non-OpenAI providers. So the launcher execs `qwen` with the caller's own
// environment; token capture happens via observer's local qwen-code
// transcript adapter (in-transcript ui_telemetry records), not the proxy.
// It never sets an API key or a base URL.
func newQwenCmd() *cobra.Command {
	var (
		configPath   string
		binPath      string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
		attach       *bool
		noAttach     *bool
	)
	cmd := &cobra.Command{
		Use:   "qwen [-- qwen-args...]",
		Short: "Launch Qwen Code; with --continue-from, seed a handover via qwen -i",
		Long: "Wraps `qwen` (Qwen Code). This launcher is NON-PROXIED — Qwen Code's\n" +
			"OpenAI-compatible base-URL lane is unprobed, and overriding it would\n" +
			"redirect an already-configured provider. Token capture happens via\n" +
			"observer's local qwen-code transcript adapter, not the proxy.\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it via qwen's -i/--prompt-interactive\n" +
			"flag (delivery=inject_prompt), so Qwen Code opens pre-loaded with\n" +
			"the mission. See docs/session-handoff.md.\n\n" +
			"All arguments after the subcommand are forwarded to qwen. Use `--`\n" +
			"to separate observer flags from qwen flags. NEVER touches API keys.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Attach gate (attach-all-launchers): default-on attach hands the PTY
			// to the daemon. Seed-only spec (qwen is launched non-proxied — no
			// proxy env, no escape-hatch flag); incompatible when a handoff fork
			// is engaged or a headless -p/--prompt one-shot is forwarded.
			outcome, aErr := launcherAttach(cmd.Context(), launcherAttachSpec{
				tool:         "qwen-code",
				configPath:   configPath,
				flagAttach:   *attach,
				flagNoAttach: *noAttach,
				incompatible: continueFamilyEngaged(continueFrom, carry, fromMessage, fromTime) ||
					argsContainHeadlessFlag(args, "-p", "--prompt"),
				passthrough: qwenAttachPassthrough(binPath),
				toolArgs:    args,
				stderr:      cmd.ErrOrStderr(),
			})
			if outcome.handled {
				return aErr
			}

			bin, err := resolveToolBin("qwen-code", binPath, "--qwen-path", configPath, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "qwen-code",
					label:       "qwen",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// qwen -i/--prompt-interactive takes the initial prompt as
					// its value; -p/--prompt is the headless one-shot form —
					// flag all four as conflicts. Qwen Code is a Gemini-CLI
					// fork whose bare positional is also an initial prompt, so
					// a forwarded positional competes with the seed and must
					// trip the collision check.
					inject: promptInjection{
						Kind:                   injectFlagValue,
						Flag:                   "-i",
						ConflictFlags:          []string{"-i", "--prompt-interactive", "-p", "--prompt"},
						BarePositionalIsPrompt: true,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer qwen: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}

			return runSeedOnlyLaunch("qwen", bin, args, continueDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml); used to resolve the source session for --continue-from")
	cmd.Flags().StringVar(&binPath, "qwen-path", "", "Path to the qwen binary (default: resolve `qwen` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it via qwen's -i flag (delivery=inject_prompt, launched non-proxied). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	attach, noAttach = registerAttachFlags(cmd, "qwen-code")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// qwenAttachPassthrough forwards the --qwen-path wrapper flag to the
// daemon-spawned inner `observer qwen` launcher when set (nil otherwise).
func qwenAttachPassthrough(qwenPath string) []string {
	if qwenPath != "" {
		return []string{"--qwen-path", qwenPath}
	}
	return nil
}

// runSeedOnlyLaunch execs a tool NON-PROXIED (child inherits os.Environ()
// with no base-URL redirect), forwarding stdio and the exit code — the same
// shape as `observer run`. It is the shared exec tail for the pure seeding
// launchers (antigravity-cli, qwen, kiro) whose token capture happens via a
// local adapter rather than the proxy. dir ("" inherits the caller's cwd) is
// set by --continue-from to the source session's translated project root so
// a cross-OS continuation lands in the real project folder.
func runSeedOnlyLaunch(label, bin string, args []string, dir string) error {
	child := exec.Command(bin, args...) //nolint:gosec // user-launched tool; argv is the seeded handover + forwarded args
	child.Env = os.Environ()
	child.Dir = dir
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitErr(ee.ExitCode())
		}
		return fmt.Errorf("exec %s: %w", label, err)
	}
	return nil
}
