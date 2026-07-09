package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/intelligence/patterns"
)

// newPatternsCmd wires `observer patterns` — derives project_patterns rows
// that the get_project_patterns MCP tool surfaces (spec §15, §6.2).
func newPatternsCmd() *cobra.Command {
	var (
		configPath  string
		projectRoot string
		topN        int
		jsonOut     bool
	)
	cmd := &cobra.Command{
		Use:   "patterns",
		Short: "Derive hot files, co-change, common commands, edit→test, onboarding sequences",
		Long: "Batch-derives project_patterns rows of five managed types. Run after\n" +
			"scans or on a cron. Decay is applied at read time by MCP clients; the\n" +
			"rows written here carry raw confidence and a per-type half-life.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			res, err := patterns.New(database).Derive(cmd.Context(), patterns.Options{
				ProjectRoot: projectRoot,
				TopN:        topN,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(res, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"derived %d patterns across %d project(s) in %dms:\n",
				res.PatternsWritten, res.ProjectsProcessed, res.DurationMs)
			for _, t := range []string{
				patterns.TypeHotFile, patterns.TypeCoChange, patterns.TypeCommonCommand,
				patterns.TypeEditTestPair, patterns.TypeOnboardingSeq,
			} {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-22s %d\n", t, res.ByType[t])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().StringVar(&projectRoot, "project", "", "Restrict to a single project root")
	cmd.Flags().IntVar(&topN, "top", 25, "Max rows written per (project, pattern_type)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}
