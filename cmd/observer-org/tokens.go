// tokens.go — `observer-org tokens` enrolment-token inspection.

package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// newTokensCmd groups enrolment-token inspection commands. Server-side
// (direct DB), so it runs on the org host without a SCIM curl or a
// hand-written SQLite query.
func newTokensCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Inspect enrolment tokens",
	}
	cmd.AddCommand(newTokensListCmd(configPath))
	return cmd
}

func newTokensListCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List enrolment tokens (id, user, created, expires, status)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			db, err := orgdb.Open(ctx, orgdb.Options{Path: cfg.Server.DBPath})
			if err != nil {
				return err
			}
			defer db.Close()

			toks, err := api.ListEnrolmentTokens(ctx, db)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(toks) == 0 {
				fmt.Fprintln(out, "No enrolment tokens yet. Mint one with `observer-org invite <email>` or `observer-org new-enrolment-token --user-id <id>`.")
				return nil
			}

			now := time.Now()
			tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "TOKEN_ID\tUSER\tCREATED\tEXPIRES\tSTATUS")
			for _, t := range toks {
				status := "active"
				switch {
				case t.Redeemed():
					status = "used " + t.UsedAt.UTC().Format("2006-01-02T15:04Z")
				case t.Expired(now):
					status = "expired"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
					t.TokenID, t.UserEmail,
					t.CreatedAt.UTC().Format("2006-01-02T15:04Z"),
					t.ExpiresAt.UTC().Format("2006-01-02T15:04Z"),
					status)
			}
			return tw.Flush()
		},
	}
}
