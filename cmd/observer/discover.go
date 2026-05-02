package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/intelligence/discover"
)

// newDiscoverCmd wires `observer discover` — reports optimization
// opportunities across sessions (spec §15.1).
func newDiscoverCmd() *cobra.Command {
	var (
		configPath  string
		projectRoot string
		days        int
		limit       int
		jsonOut     bool
	)
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Surface stale re-reads, redundant commands, and cross-tool redundancy",
		Long: "Walks the captured action set and surfaces optimization opportunities.\n" +
			"Stale-read waste is estimated as (file_size/4) × stale_count — a rough\n" +
			"token proxy. Repeated commands track runs with no intervening edit.\n" +
			"Cross-tool files are touched by more than one AI tool. Native vs Bash\n" +
			"shows the native tool-use advantage over Bash-only compressors (RTK).",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			report, err := discover.New(database).Run(cmd.Context(), discover.Options{
				ProjectRoot: projectRoot,
				Days:        days,
				Limit:       limit,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				body, _ := json.MarshalIndent(report, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(body))
				return nil
			}
			printDiscover(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().StringVar(&projectRoot, "project", "", "Filter to a project root path")
	cmd.Flags().IntVar(&days, "days", 30, "Restrict to the last N days (0 disables)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Max rows per section")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON")
	return cmd
}

func printDiscover(w io.Writer, r discover.Report) {
	fmt.Fprintf(w, "== Discovery report ==\n")
	if r.Project != "" {
		fmt.Fprintf(w, "project: %s\n", r.Project)
	}
	if r.Days > 0 {
		fmt.Fprintf(w, "window:  last %d day(s)\n", r.Days)
	}
	fmt.Fprintf(w, "actions: %d  stale_reads: %d (~%d tokens wasted)  repeated_cmd_groups: %d  cross_tool_files: %d  native: %d  bash: %d\n\n",
		r.Summary.TotalActions, r.Summary.StaleReadCount, r.Summary.EstWastedTokens,
		r.Summary.RepeatedCmdGroups, r.Summary.CrossToolFileCount,
		r.Summary.NativeActionCount, r.Summary.BashActionCount)

	if len(r.StaleReads) > 0 {
		fmt.Fprintln(w, "Stale re-reads (top by stale_count):")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  FILE\tSTALE\tTOTAL\tWASTED~")
		for _, s := range r.StaleReads {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\n",
				truncate(s.FilePath, 60), s.StaleCount, s.TotalReads, s.EstWastedTokens)
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}
	if len(r.RepeatedCommands) > 0 {
		fmt.Fprintln(w, "Repeated commands:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  COMMAND\tRUNS\tNO_CHANGE_RERUNS\tSUCCESS\tFAIL")
		for _, rc := range r.RepeatedCommands {
			fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%d\n",
				truncate(rc.Command, 50), rc.TotalRuns, rc.NoChangeReruns, rc.Successful, rc.Failed)
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}
	if len(r.CrossToolFiles) > 0 {
		fmt.Fprintln(w, "Cross-tool files:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  FILE\tTOOLS\tACCESSES")
		for _, c := range r.CrossToolFiles {
			fmt.Fprintf(tw, "  %s\t%s\t%d\n",
				truncate(c.FilePath, 50), strings.Join(c.Tools, ","), c.Accesses)
		}
		_ = tw.Flush()
		fmt.Fprintln(w)
	}
	if len(r.NativeVsBash) > 0 {
		fmt.Fprintln(w, "Native vs Bash:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  TYPE\tNATIVE\tCOUNT")
		for _, b := range r.NativeVsBash {
			native := "no"
			if b.IsNative {
				native = "yes"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%d\n", b.ActionType, native, b.Count)
		}
		_ = tw.Flush()
	}
}
