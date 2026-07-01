// users.go — `observer-org users` + `observer-org invite` member admin.

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// newUsersCmd groups org-member management. Server-side (direct DB), so
// provisioning a developer no longer needs a raw SCIM curl.
func newUsersCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage org members",
	}
	cmd.AddCommand(newUsersCreateCmd(configPath))
	return cmd
}

func newUsersCreateCmd(configPath *string) *cobra.Command {
	var displayName string
	cmd := &cobra.Command{
		Use:   "create <email>",
		Short: "Provision an org member (replaces a SCIM POST /Users curl)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			id, created, err := api.ProvisionUser(ctx, db, args[0], displayName)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if created {
				fmt.Fprintf(out, "Created member %s (user_id %s)\n", args[0], id)
			} else {
				fmt.Fprintf(out, "Member %s already exists (user_id %s)\n", args[0], id)
			}
			fmt.Fprintf(out, "Mint an enrolment token with:  observer-org invite %s\n", args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&displayName, "name", "", "display name for the member")
	return cmd
}

// newInviteCmd provisions a member (if needed) AND mints an enrolment
// token in one step — the common "add a developer" action.
func newInviteCmd(configPath *string) *cobra.Command {
	var (
		ttlDays     int
		displayName string
		tokenOnly   bool
	)
	cmd := &cobra.Command{
		Use:   "invite <email>",
		Short: "Provision a member (if needed) and mint an enrolment token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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

			id, created, err := api.ProvisionUser(ctx, db, args[0], displayName)
			if err != nil {
				return err
			}
			ttl := time.Duration(ttlDays) * 24 * time.Hour
			if ttlDays <= 0 {
				ttl = time.Duration(cfg.Enrolment.DefaultTokenLifetimeDays) * 24 * time.Hour
			}
			token, tokenID, expires, err := api.MintEnrolmentTokenForUser(ctx, db, id, ttl)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			// --token-only prints JUST the compound token on one line, so
			// quickstart's dev-auth-401 fallback (which runs this command
			// via `docker compose exec`) can parse it unambiguously.
			if tokenOnly {
				fmt.Fprintln(out, token)
				return nil
			}
			verb := "exists"
			if created {
				verb = "created"
			}
			fmt.Fprintf(out, "Member %s %s (user_id %s)\n", args[0], verb, id)
			fmt.Fprintf(out, "token_id:   %s\n", tokenID)
			fmt.Fprintf(out, "expires_at: %s\n", expires.UTC().Format(time.RFC3339))
			fmt.Fprintln(out, "\nShare with the developer:")
			fmt.Fprintf(out, "  observer enroll <org-url> %s\n", token)
			fmt.Fprintln(out, "(token is single-use and shown once)")
			return nil
		},
	}
	cmd.Flags().IntVar(&ttlDays, "ttl-days", 0, "token lifetime in days (default: config enrolment.default_token_lifetime_days)")
	cmd.Flags().StringVar(&displayName, "name", "", "display name when creating a new member")
	cmd.Flags().BoolVar(&tokenOnly, "token-only", false, "print only the enrolment token (for scripting / quickstart fallback)")
	return cmd
}
