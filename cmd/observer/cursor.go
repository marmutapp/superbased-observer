// cursor.go — `observer cursor` launcher subcommand.

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"
)

// cursorSubcommands are the cursor-agent argv tokens that are subcommands
// (from `cursor-agent --help` Commands:), not an initial prompt — so the
// two-prompt collision check does not misread `cursor-agent status` (and
// friends) as a forwarded positional prompt.
var cursorSubcommands = map[string]bool{
	"install-shell-integration":   true,
	"uninstall-shell-integration": true,
	"login":                       true,
	"logout":                      true,
	"mcp":                         true,
	"worker":                      true,
	"status":                      true,
	"whoami":                      true,
	"models":                      true,
	"about":                       true,
	"update":                      true,
	"create-chat":                 true,
	"generate-rule":               true,
	"rule":                        true,
	"agent":                       true,
	"ls":                          true,
	"resume":                      true,
	"help":                        true,
}

// newCursorCmd implements `observer cursor` — a PURE SEEDING wrapper around
// the `cursor-agent` CLI. It does NOT route traffic through the observer
// proxy: cursor talks to its own backend (registry Proxy:nil,
// RouteStatusProbeRequired), so the child inherits the ambient environment
// unchanged (child.Env = os.Environ()). Token capture for cursor is via the
// watcher + chats/store.db adapter + the cursor hook (registered by
// `observer init --cursor`), not the proxy — seeding is orthogonal to
// capture.
//
// With --continue-from <session-id> the launcher distills a handover from
// that session and seeds it as cursor-agent's FIRST interactive prompt via a
// trailing positional: `cursor-agent <args> "<handover>"`. Interactive is
// cursor-agent's default; the headless switch is -p/--print, which this
// launcher never emits, so a bare trailing positional starts a seeded
// interactive session (delivery=inject_prompt). See docs/session-handoff.md.
func newCursorCmd() *cobra.Command {
	var (
		configPath   string
		binPath      string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:   "cursor [-- cursor-agent-args...]",
		Short: "Launch cursor-agent, optionally seeded with a distilled handover",
		Long: "Wraps the `cursor-agent` CLI as a pure SEEDING launcher. NO proxy\n" +
			"routing — cursor talks to its own backend, so the child inherits\n" +
			"your environment unchanged. Token capture for cursor is via the\n" +
			"cursor hook + the chats/store.db watcher adapter (registered by\n" +
			"`observer init --cursor`), not the proxy.\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it as cursor-agent's first interactive\n" +
			"prompt via a trailing positional (delivery=inject_prompt). cursor's\n" +
			"interactive mode is the default; the headless -p/--print switch is\n" +
			"never emitted. See docs/session-handoff.md.\n\n" +
			"All arguments after the subcommand are forwarded to cursor-agent.\n" +
			"Use `--` to separate observer flags from cursor-agent flags.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := resolveToolBin("cursor-agent", binPath, "--cursor-agent-path")
			if err != nil {
				return err
			}
			if continueFrom != "" {
				seeded, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "cursor",
					label:       "cursor",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// cursor-agent `[prompt...]` seeds an interactive session;
					// the headless path is the explicit -p/--print flag, which
					// this launcher never emits.
					inject: promptInjection{
						Kind:        injectTrailingPositional,
						Subcommands: cursorSubcommands,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer cursor: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
			}

			child := exec.Command(bin, args...)
			// PURE SEEDING WRAPPER: no proxy env injection. cursor uses its
			// own backend, so the child inherits the ambient environment
			// unchanged.
			child.Env = os.Environ()
			child.Stdin = os.Stdin
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
			if runErr := child.Run(); runErr != nil {
				var ee *exec.ExitError
				if errors.As(runErr, &ee) {
					return exitErr(ee.ExitCode())
				}
				return fmt.Errorf("exec cursor-agent: %w", runErr)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&binPath, "cursor-agent-path", "", "Path to the cursor-agent binary (default: resolve `cursor-agent` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it as cursor-agent's first prompt (delivery=inject_prompt). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}
