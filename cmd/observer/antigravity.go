// antigravity.go — `observer antigravity` launcher subcommand.

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newAntigravityCmd implements `observer antigravity-cli` — launches Google's
// Antigravity CLI (`agy`). Its primary purpose is `--continue-from`: distill a
// handover from a source session and seed it via `agy -i "<handover>"`
// (`-i` ≡ `--prompt-interactive`: "execute the provided prompt and continue in
// interactive mode"), so a migration INTO Antigravity opens the interactive
// session pre-loaded with the mission.
//
// NON-PROXIED on purpose. Antigravity routes to its own backend with no
// base-URL knob (a grounded negative in the integration registry —
// Proxy: nil / RouteStatusNativeExempt); there is nothing to redirect, and
// token capture happens via observer's local Antigravity `.db` adapter
// (clidb.go reads the newer CLI's plaintext-protobuf SQLite store), not the
// proxy. So the launcher execs `agy` with the caller's own environment — it
// never sets an API key or a base URL.
func newAntigravityCmd() *cobra.Command {
	var (
		configPath   string
		binPath      string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:     "antigravity-cli [-- agy-args...]",
		Aliases: []string{"antigravity", "agy"},
		Short:   "Launch Antigravity CLI (agy); with --continue-from, seed a handover via agy -i",
		Long: "Wraps Google's Antigravity CLI (`agy`). Antigravity has no base-URL\n" +
			"knob, so this launcher is NON-PROXIED — token capture happens via\n" +
			"observer's local Antigravity `.db` adapter, not the proxy.\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it via agy's -i/--prompt-interactive\n" +
			"flag (delivery=inject_prompt), so Antigravity opens pre-loaded with\n" +
			"the mission. See docs/session-handoff.md.\n\n" +
			"All arguments after the subcommand are forwarded to agy. Use `--`\n" +
			"to separate observer flags from agy flags. NEVER touches API keys.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := resolveToolBin("antigravity-cli", binPath, "--agy-path", configPath, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "antigravity-cli",
					label:       "antigravity-cli",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// agy -i/--prompt-interactive takes the initial prompt as
					// its value; flag both spellings as conflicts. agy is a
					// Gemini-family CLI whose bare positional is also an initial
					// prompt, so a forwarded positional competes with the seed
					// and must trip the collision check.
					inject: promptInjection{
						Kind:                   injectFlagValue,
						Flag:                   "-i",
						ConflictFlags:          []string{"-i", "--prompt-interactive"},
						BarePositionalIsPrompt: true,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer antigravity-cli: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}

			return runSeedOnlyLaunch("agy", bin, args, continueDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml); used to resolve the source session for --continue-from")
	cmd.Flags().StringVar(&binPath, "agy-path", "", "Path to the agy binary (default: resolve `agy` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it via agy's -i flag (delivery=inject_prompt, launched non-proxied). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}
