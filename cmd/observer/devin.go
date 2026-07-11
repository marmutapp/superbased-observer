// devin.go — `observer devin` launcher subcommand.

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newDevinCmd implements `observer devin` — launches Cognition's Devin
// terminal agent (binary `devin`). Its primary purpose is `--continue-from`:
// distill a handover from a source session and seed it as devin's trailing
// positional prompt after the mandatory `--` separator (`devin -- "<handover>"`
// opens an interactive session pre-loaded with the mission — the seed contract
// verified live on a real TTY 2026-07-09; a scripted pty had hung earlier).
//
// NON-PROXIED on purpose. Devin talks to Cognition's own Windsurf backend
// with no base-URL override (only an HTTP-proxy setting), so no observer-
// routed turn is possible today (registry RouteStatusNativeExempt). Token
// capture happens via observer's local devin adapter (per-message
// metadata.metrics in the message_nodes SQLite store), not the proxy. The
// launcher never touches API keys.
func newDevinCmd() *cobra.Command {
	var (
		configPath   string
		binPath      string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:   "devin [-- devin-args...]",
		Short: "Launch Devin; with --continue-from, seed a handover as devin's `--`-separated positional prompt",
		Long: "Wraps Cognition's Devin terminal agent (`devin`). This launcher is\n" +
			"NON-PROXIED — devin speaks to Cognition's own backend with no\n" +
			"base-URL knob, so the proxy lane is a registry native-exempt item.\n" +
			"Token capture happens via observer's local devin adapter\n" +
			"(per-message metrics in the message_nodes store), not the proxy.\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it as devin's trailing positional prompt\n" +
			"after the mandatory `--` separator (delivery=inject_prompt), so Devin\n" +
			"opens an interactive session pre-loaded with the mission. See\n" +
			"docs/session-handoff.md.\n\n" +
			"All arguments after the subcommand are forwarded to devin. Use `--`\n" +
			"to separate observer flags from devin flags. NEVER touches API keys.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := resolveToolBin("devin", binPath, "--devin-path")
			if err != nil {
				return err
			}

			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "devin",
					label:       "devin",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// devin takes the initial prompt as a trailing positional
					// that MUST follow a `--` separator (clap `[-- <PROMPT>...]`
					// last-only positional). -p/--print is the headless one-shot
					// form and --prompt-file loads the prompt from a file; both
					// would collide with the seeded handover.
					inject: promptInjection{
						Kind:          injectTrailingPositionalAfterDashDash,
						ConflictFlags: []string{"-p", "--print", "--prompt-file"},
						Subcommands:   devinSubcommands,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer devin: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}

			return runSeedOnlyLaunch("devin", bin, args, continueDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml); used to resolve the source session for --continue-from")
	cmd.Flags().StringVar(&binPath, "devin-path", "", "Path to the devin binary (default: resolve `devin` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it as devin's `--`-separated positional prompt (delivery=inject_prompt, launched non-proxied). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// devinSubcommands are the devin argv tokens that are subcommands, not a
// prompt — so forwardedPromptConflict does not misread a forwarded verb
// (e.g. `devin list`) as a competing positional prompt.
var devinSubcommands = map[string]bool{
	"auth": true, "mcp": true, "rules": true, "skills": true,
	"plugins": true, "cloud": true, "list": true, "ls": true,
	"update": true, "version": true, "sandbox": true, "setup": true,
	"uninstall": true, "acp": true, "shell": true, "help": true,
}
