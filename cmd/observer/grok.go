// grok.go — `observer grok` launcher subcommand.

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newGrokCmd implements `observer grok` — launches xAI's Grok Build terminal
// agent (binary `grok`). Its primary purpose is `--continue-from`: distill a
// handover from a source session and seed it as grok's trailing positional
// prompt (`grok "<handover>"` opens an interactive session pre-loaded with
// the mission — seed lane verified live 2026-07-09).
//
// NON-PROXIED on purpose. Grok speaks OpenAI-Responses-shaped traffic to a
// non-default host (cli-chat-proxy.grok.com) — a /up/<id> upstream-seam
// candidate (RouteStatusAfterUpstream in the integration registry) — but no
// observer-routed turn has confirmed api_turns capture, so the launcher
// execs `grok` with the caller's own environment. Token capture happens via
// observer's local grok adapter (the sid-correlated ~/.grok/logs/
// unified.jsonl splits), not the proxy. It never sets an API key or a base
// URL.
func newGrokCmd() *cobra.Command {
	var (
		configPath   string
		binPath      string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:   "grok [-- grok-args...]",
		Short: "Launch Grok Build; with --continue-from, seed a handover as grok's positional prompt",
		Long: "Wraps xAI's Grok Build terminal agent (`grok`). This launcher is\n" +
			"NON-PROXIED — grok's upstream is a non-default host whose /up/<id>\n" +
			"proxy lane is unprobed. Token capture happens via observer's local\n" +
			"grok adapter (session bundles + the sid-correlated unified.jsonl\n" +
			"token log), not the proxy.\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it as grok's trailing positional prompt\n" +
			"(delivery=inject_prompt), so Grok opens an interactive session\n" +
			"pre-loaded with the mission. See docs/session-handoff.md.\n\n" +
			"All arguments after the subcommand are forwarded to grok. Use `--`\n" +
			"to separate observer flags from grok flags. NEVER touches API keys.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := resolveToolBin("grok", binPath, "--grok-path")
			if err != nil {
				return err
			}

			var continueDir string
			if continueFrom != "" {
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "grok",
					label:       "grok",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// grok takes the initial prompt as a bare positional
					// (verified live: `grok "<seed>"` opened a seeded
					// interactive session); -p/--prompt is the headless
					// one-shot form and would collide with the seed.
					inject: promptInjection{
						Kind:          injectTrailingPositional,
						ConflictFlags: []string{"-p", "--prompt"},
						Subcommands:   grokSubcommands,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer grok: continue-from failed: %v\n", cerr)
					return cerr
				}
				args = seeded
				continueDir = cwd
			}

			return runSeedOnlyLaunch("grok", bin, args, continueDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml); used to resolve the source session for --continue-from")
	cmd.Flags().StringVar(&binPath, "grok-path", "", "Path to the grok binary (default: resolve `grok` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it as grok's positional prompt (delivery=inject_prompt, launched non-proxied). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// grokSubcommands are the grok argv tokens that are subcommands, not a
// prompt — so forwardedPromptConflict does not misread a forwarded verb
// (e.g. `grok inspect`) as a competing positional prompt.
var grokSubcommands = map[string]bool{
	"inspect": true, "login": true, "logout": true, "mcp": true,
	"sessions": true, "help": true, "version": true, "update": true,
}
