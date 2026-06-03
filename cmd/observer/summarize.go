package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/intelligence/summary"
)

func newSummarizeCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "summarize",
		Short: "Generate AI-compressed summaries for unsummarized sessions",
		Long: "Calls the Anthropic Messages API (Haiku by default) to produce a 2-4\n" +
			"sentence summary for each session that lacks one. The summary is\n" +
			"scrubbed (spec §8) and stored in sessions.summary_md. Requires\n" +
			"ANTHROPIC_API_KEY in the environment (or intelligence.api_key_env\n" +
			"in config).",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			s, err := summary.New(summary.Options{
				DB:        database,
				APIKeyEnv: cfg.Intelligence.APIKeyEnv,
				Model:     cfg.Intelligence.SummaryModel,
			})
			if err != nil {
				return err
			}
			res, err := s.SummarizeAll(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"summarized %d session(s), skipped %d, errors %d (%dms)\n",
				res.SessionsProcessed, res.SessionsSkipped, res.Errors, res.DurationMs)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit machine-readable JSON")
	return cmd
}
