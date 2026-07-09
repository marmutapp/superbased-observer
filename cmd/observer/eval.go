package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
)

// newEvalCmd is the CLI surface for the obs minimal eval plane (plan §8):
// build datasets from captured traces, then score them with deterministic code
// scorers (and an LLM judge once a host JudgeClient is wired). It funnels every
// obs access through the build-tagged wrappers in obs_wire.go, so this file
// imports no internal/obs package (the separability boundary, plan §2.3/§11).
// `observer eval run --fail-under` is the CI regression gate.
func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score captured trajectories — datasets + code/LLM-judge scorers (obs eval plane)",
		Long: "The minimal eval plane over captured trajectories (requires\n" +
			"[observability] enabled). Build a dataset from recent LLM spans, then\n" +
			"run scorers over it; `eval run --fail-under` is the CI regression gate.",
	}
	cmd.AddCommand(newEvalDatasetCmd(), newEvalRunCmd(), newEvalScorersCmd())
	return cmd
}

func newEvalDatasetCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dataset", Short: "Manage eval datasets"}
	cmd.AddCommand(newEvalDatasetCreateCmd(), newEvalDatasetListCmd())
	return cmd
}

func newEvalDatasetCreateCmd() *cobra.Command {
	var (
		configPath  string
		description string
		limit       int
	)
	cmd := &cobra.Command{
		Use:   "create-from-traces <name>",
		Short: "Snapshot recent LLM spans into a named dataset",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := requireObsEnabled(cfg); err != nil {
				return err
			}
			id, added, err := obsEvalCreateDatasetFromTraces(cmd.Context(), cfg, database, slog.Default(), args[0], description, limit)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dataset %q (id %d): %d new item(s) added\n", args[0], id, added)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&description, "description", "", "Optional dataset description")
	cmd.Flags().IntVar(&limit, "limit", 200, "Max recent LLM spans to snapshot")
	return cmd
}

func newEvalDatasetListCmd() *cobra.Command {
	var (
		configPath string
		jsonOut    bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List eval datasets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := requireObsEnabled(cfg); err != nil {
				return err
			}
			rows, err := obsEvalListDatasets(cmd.Context(), cfg, database, slog.Default())
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no datasets yet — create one with `observer eval dataset create-from-traces <name>`")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tITEMS\tCREATED")
			for _, d := range rows {
				fmt.Fprintf(tw, "%d\t%s\t%d\t%s\n", d.ID, d.Name, d.ItemCount, d.CreatedAt)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func newEvalRunCmd() *cobra.Command {
	var (
		configPath     string
		scorers        []string
		runName        string
		failUnder      float64
		jsonOut        bool
		judgePrompt    string
		judgeModel     string
		judgeThreshold float64
	)
	cmd := &cobra.Command{
		Use:   "run <dataset>",
		Short: "Score a dataset with one or more scorers",
		Long: "Runs the given scorers over every item in a dataset, persists the run\n" +
			"and per-item scores, and prints the pass rate. With --fail-under, exits\n" +
			"non-zero when the pass rate drops below the threshold (the CI gate).\n" +
			"Specify scorers as --scorer name or --scorer name:key=val,key2=val2.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(scorers) == 0 {
				return fmt.Errorf("at least one --scorer is required (available: %s)", strings.Join(obsEvalScorerNames(), ", "))
			}
			cfg, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			if err := requireObsEnabled(cfg); err != nil {
				return err
			}
			sum, err := obsEvalRun(cmd.Context(), cfg, database, slog.Default(), args[0], scorers, runName, judgePrompt, judgeModel, judgeThreshold)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(sum); err != nil {
					return err
				}
			} else {
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "eval run %d on %q: %d/%d passed (%.0f%%), mean score %.3f\n",
					sum.RunID, args[0], sum.Passed, sum.Total, sum.PassRate*100, sum.MeanScore)
			}
			if failUnder > 0 && sum.PassRate < failUnder {
				return fmt.Errorf("eval regression: pass rate %.3f below --fail-under %.3f", sum.PassRate, failUnder)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringArrayVar(&scorers, "scorer", nil, "Scorer spec (repeatable): name or name:key=val,key2=val2")
	cmd.Flags().StringVar(&runName, "name", "", "Optional run name")
	cmd.Flags().Float64Var(&failUnder, "fail-under", 0, "Exit non-zero when pass rate is below this (0..1); the CI gate")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a line")
	cmd.Flags().StringVar(&judgePrompt, "judge-prompt", "", "Prompt for the llm_judge scorer (use {{input}}/{{output}}/{{reference}}); avoids comma-escaping in --scorer")
	cmd.Flags().StringVar(&judgeModel, "judge-model", "", "Override the llm_judge model (else [observability.eval] judge_model)")
	cmd.Flags().Float64Var(&judgeThreshold, "judge-threshold", 0, "Pass threshold for the llm_judge score (0..1; default 0.5)")
	return cmd
}

func newEvalScorersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scorers",
		Short: "List the built-in scorer names",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names := obsEvalScorerNames()
			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(none — this binary was built without observability)")
				return nil
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

// requireObsEnabled gives a clear, actionable error when [observability] is
// off (or compiled out), rather than a confusing empty result.
func requireObsEnabled(cfg config.Config) error {
	if !obsEvalEnabled(cfg) {
		return fmt.Errorf("the eval plane requires [observability] enabled = true in your config.toml (and a binary built with observability)")
	}
	return nil
}
