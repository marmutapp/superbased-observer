// goose.go — `observer goose` launcher subcommand.

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// gooseContinueSubcommand is the subcommand the `--continue-from` launch
// targets: `goose run -t "<text>" -s` executes the seeded prompt and then
// stays interactive (`-s/--interactive`, verified live 2026-07-09 on a
// keyed run — the seed landed as the session's first user message). The
// launcher ensures this leading subcommand so a seeded launch always opens
// the run lane rather than the top-level command.
const gooseContinueSubcommand = "run"

// newGooseCmd implements `observer goose` — launches Block's goose agent
// (binary `goose`). Its primary purpose is `--continue-from`: distill a
// handover from a source session and seed it via `goose run -t "<handover>"
// -s` (the run subcommand's --text value, kept interactive by -s).
//
// NON-PROXIED on purpose. Goose's override surface is OPENAI_HOST (plus
// per-provider host settings in config.yaml), NOT OPENAI_BASE_URL — and
// overriding it would redirect whatever provider the operator already
// configured (registry RouteStatusProbeRequired; same rationale as
// `observer qwen`). Token capture happens via observer's local goose
// adapter (session-level counts in sessions.db), not the proxy. The
// launcher never touches API keys or ~/.config/goose/secrets.yaml.
func newGooseCmd() *cobra.Command {
	var (
		configPath   string
		binPath      string
		continueFrom string
		carry        string
		fromMessage  int
		fromTime     string
	)
	cmd := &cobra.Command{
		Use:   "goose [-- goose-args...]",
		Short: "Launch goose; with --continue-from, seed a handover via goose run -t … -s",
		Long: "Wraps Block's goose agent (binary `goose`). This launcher is\n" +
			"NON-PROXIED — goose's env override is OPENAI_HOST (not\n" +
			"OPENAI_BASE_URL) and setting it would redirect an already-\n" +
			"configured provider, so the proxy lane stays a registry probe\n" +
			"item. Token capture happens via observer's local goose adapter\n" +
			"(session-level counts in sessions.db).\n\n" +
			"With --continue-from <session-id> the launcher distills a handover\n" +
			"from that session and seeds it via `goose run -t \"<handover>\" -s`\n" +
			"(delivery=inject_prompt), so goose executes the mission prompt and\n" +
			"stays interactive. See docs/session-handoff.md.\n\n" +
			"All arguments after the subcommand are forwarded to goose. Use\n" +
			"`--` to separate observer flags from goose flags. NEVER touches\n" +
			"API keys or ~/.config/goose/secrets.yaml.",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := resolveToolBin("goose", binPath, "--goose-path")
			if err != nil {
				return err
			}

			var continueDir string
			if continueFrom != "" {
				// A forwarded --no-session collides with the seeded launch: it
				// runs the prompt WITHOUT persisting a session, so observer's
				// goose adapter would capture nothing of the continued work.
				// Fail fast, before the (comparatively expensive) handoff
				// render.
				if forwardedFlagConflict(args, "--no-session") {
					err := fmt.Errorf("--continue-from opens a session-persisted seeded run, but you forwarded --no-session — drop it (observer's goose adapter captures via sessions.db)")
					fmt.Fprintf(cmd.ErrOrStderr(), "observer goose: %v\n", err)
					return err
				}
				seeded, cwd, cerr := continueFromArgs(cmd.Context(), continueFromParams{
					tool:        "goose",
					label:       "goose",
					configPath:  configPath,
					sessionID:   continueFrom,
					carry:       carry,
					fromMessage: fromMessage,
					fromTime:    fromTime,
					args:        args,
					// goose run -t/--text takes the seed as its value; a
					// forwarded -i/--instructions file or --recipe is a
					// competing instruction source. `goose run` takes NO bare
					// positional prompt, so a forwarded positional is not a
					// collision; the subcommand map keeps forwarded top-level
					// verbs legible to the conflict check regardless.
					inject: promptInjection{
						Kind:          injectFlagValue,
						Flag:          "-t",
						ConflictFlags: []string{"-t", "--text", "-i", "--instructions", "--recipe"},
						Subcommands:   gooseSubcommands,
					},
					stderr: cmd.ErrOrStderr(),
				})
				if cerr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "observer goose: continue-from failed: %v\n", cerr)
					return cerr
				}
				// Ensure the `run` subcommand leads the argv, and keep the
				// seeded run interactive (-s) unless the user already
				// forwarded the flag themselves.
				args = ensureLeadingSubcommand(seeded, gooseContinueSubcommand)
				if !forwardedFlagConflict(args, "-s", "--interactive") {
					args = append(args, "-s")
				}
				continueDir = cwd
			}

			return runSeedOnlyLaunch("goose", bin, args, continueDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml); used to resolve the source session for --continue-from")
	cmd.Flags().StringVar(&binPath, "goose-path", "", "Path to the goose binary (default: resolve `goose` on PATH)")
	cmd.Flags().StringVar(&continueFrom, "continue-from", "", "Session id to continue from: distill a handover from that session and seed it via `goose run -t \"<handover>\" -s` (delivery=inject_prompt, launched non-proxied). See docs/session-handoff.md.")
	cmd.Flags().StringVar(&carry, "carry", "", "Carry mode for --continue-from: metadata|distilled|distilled_tail|full|full_cache (default from [handoff] config)")
	cmd.Flags().IntVar(&fromMessage, "from-message", 0, "With --continue-from: fork after this 1-based transcript message (default: last message)")
	cmd.Flags().StringVar(&fromTime, "from-time", "", "With --continue-from: fork after the last message at or before this RFC3339 time")
	cmd.Flags().SetInterspersed(false)
	return cmd
}

// gooseSubcommands are the goose argv tokens that are subcommands, not a
// prompt — so forwardedPromptConflict does not misread a forwarded verb
// (e.g. `goose session`) as a competing positional prompt.
var gooseSubcommands = map[string]bool{
	"configure": true, "info": true, "doctor": true, "mcp": true,
	"acp": true, "serve": true, "session": true, "s": true,
	"project": true, "p": true, "projects": true, "ps": true,
	"run": true, "recipe": true, "skills": true, "plugin": true,
	"schedule": true, "sched": true, "gateway": true, "gw": true,
	"update": true, "term": true, "tui": true, "local-models": true,
	"lm": true, "completion": true, "review": true, "help": true,
}
