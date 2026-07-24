// kilo.go — `observer kilo` launcher subcommand.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// newKiloCmd implements `observer kilo` — a PURE SEEDING wrapper around the
// `kilo` CLI (@kilocode/cli, an OpenCode fork). Unlike the OpenAI-compatible
// launchers (opencode / codex), kilo-code-cli is NATIVE-EXEMPT: @kilocode/cli
// talks to api.kilo.ai directly and ignores OPENAI_BASE_URL, so this launcher
// injects NO proxy env. It simply execs `kilo` with the (optionally seeded)
// args and forwards stdio + the exit code.
//
// Token capture for kilo-code-cli is via the watcher / SQLite adapter
// (internal/adapter/kilocode), NOT the proxy — so there is no proxy URL to
// resolve and no `--proxy` flag.
//
// With --continue-from <session-id> the launcher distills a handover from that
// session and seeds it via kilo's `--prompt` flag, which opens the interactive
// TUI seeded with that first prompt (the headless one-shot form is the `run`
// subcommand). See docs/session-handoff.md.
func newKiloCmd() *cobra.Command {
	var (
		configPath   string
		kiloPath     string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
		attach       *bool
		noAttach     *bool
	)
	cmd := &cobra.Command{
		Use:   "kilo [-- kilo-args...]",
		Short: "Launch the kilo CLI, optionally seeding a session handover",
		Long: "Wraps `kilo` (@kilocode/cli, an OpenCode fork) as a pure seeding\n" +
			"launcher. NO proxy routing is applied: kilo-code-cli talks to\n" +
			"api.kilo.ai directly and ignores OPENAI_BASE_URL, so this launcher\n" +
			"injects no proxy env. Token capture is via the watcher / SQLite\n" +
			"adapter, not the proxy.\n\n" +
			"All arguments after the subcommand are forwarded to kilo. Use `--`\n" +
			"to separate observer flags from kilo flags:\n" +
			"    observer kilo -- run \"summarize the diff\"\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it via kilo's --prompt flag, opening\n" +
			"the interactive TUI seeded with that first prompt (the headless\n" +
			"one-shot form is the `run` subcommand). See docs/session-handoff.md.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Attach gate (attach-all-launchers): default-on attach hands the PTY
			// to the daemon so the dashboard can drive this session. Seed-only
			// spec — kilo-code-cli is native-exempt, so NO proxy env, NO
			// escape-hatch flag; incompatible when a handoff fork is engaged or a
			// leading `run` subcommand is the headless one-shot form.
			outcome, aErr := launcherAttach(cmd.Context(), launcherAttachSpec{
				tool:         "kilo-code-cli",
				configPath:   configPath,
				flagAttach:   *attach,
				flagNoAttach: *noAttach,
				incompatible: continueFamilyEngaged(continueFrom, carry, fromMessage, fromTime) ||
					argsLeadWithSubcommand(args, kiloAttachHeadlessSubcommands),
				passthrough: kiloAttachPassthrough(kiloPath),
				toolArgs:    args,
				stderr:      cmd.ErrOrStderr(),
			})
			if outcome.handled {
				return aErr
			}

			bin, err := resolveToolBin("kilo-code-cli", kiloPath, "--kilo-path", configPath, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "kilo-code-cli",
					label:       "kilo",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// kilo --prompt seeds the interactive TUI's first prompt
					// (the headless one-shot form is the `run` subcommand).
					// kilo's bare positional is a PROJECT PATH, not a prompt,
					// so BarePositionalIsPrompt stays at its zero value — only
					// --prompt collides.
					inject: promptInjection{
						Kind:          injectFlagValue,
						Flag:          "--prompt",
						ConflictFlags: []string{"--prompt"},
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer kilo: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}
			return runKiloLauncher(bin, args, continueDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&kiloPath, "kilo-path", "", "Path to the kilo binary (default: resolve `kilo` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it via kilo's --prompt flag (delivery=inject_prompt). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	attach, noAttach = registerAttachFlags(cmd, "kilo-code-cli")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// kiloAttachHeadlessSubcommands are the leading kilo subcommands whose headless
// one-shot form cannot compose with attach (the `run` subcommand answers and
// exits, so an attach notice + daemon-owned PTY would be spam).
var kiloAttachHeadlessSubcommands = map[string]bool{"run": true}

// kiloAttachPassthrough forwards the --kilo-path wrapper flag to the
// daemon-spawned inner `observer kilo` launcher when set (nil otherwise).
func kiloAttachPassthrough(kiloPath string) []string {
	if kiloPath != "" {
		return []string{"--kilo-path", kiloPath}
	}
	return nil
}

// runKiloLauncher execs `kilo` with the (optionally seeded) argv, wiring the
// child's stdio to the parent's and forwarding the exit code via exitErr (the
// same shape as the other launchers). No proxy env is injected — kilo-code-cli
// is native-exempt.
func runKiloLauncher(bin string, args []string, dir string) error {
	child := exec.Command(bin, args...) //nolint:gosec // user-launched tool, args are theirs
	child.Dir = dir                     // "" inherits the caller's cwd; set by --continue-from to the source project root
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if rErr := child.Run(); rErr != nil {
		var ee *exec.ExitError
		if errors.As(rErr, &ee) {
			return exitErr(ee.ExitCode())
		}
		return fmt.Errorf("exec kilo: %w", rErr)
	}
	return nil
}
