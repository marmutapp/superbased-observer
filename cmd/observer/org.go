package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/db"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// buildOrgClient assembles an orgclient.Client from config: the agent DB
// (store) plus the OS-keychain bearer store (0600-file fallback rooted next to
// the DB). It also reports whether the push loop is enabled in config (for
// status/enrol hints). The CLI uses a WARN-level logger so the client's own
// info logs do not compete with the command's printed output. The returned
// cleanup closes the DB; it is non-nil only when err is nil.
func buildOrgClient(ctx context.Context, configPath string) (c *orgclient.Client, cleanup func(), enabled bool, err error) {
	cfg, err := config.Load(config.LoadOptions{GlobalPath: configPath})
	if err != nil {
		return nil, nil, false, fmt.Errorf("load config: %w", err)
	}
	if cfg.OrgClient.KeychainID == "" {
		cfg.OrgClient.KeychainID = config.DefaultKeychainID
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Observer.DBPath), 0o755); err != nil {
		return nil, nil, false, fmt.Errorf("ensure db dir: %w", err)
	}
	database, err := db.Open(ctx, db.Options{Path: cfg.Observer.DBPath})
	if err != nil {
		return nil, nil, false, fmt.Errorf("open db %s: %w", cfg.Observer.DBPath, err)
	}

	logger := newLogger("warn")
	bs := orgclient.OpenBearerStore(cfg.OrgClient.KeychainID, filepath.Dir(cfg.Observer.DBPath), logger)
	c = orgclient.New(cfg.OrgClient, store.New(database), bs, version, nil, logger)
	return c, func() { _ = database.Close() }, cfg.OrgClient.Enabled, nil
}

func newEnrollCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "enroll <org-url> <token>",
		Short: "Enrol this agent in an organisation's Observer server",
		Long: `Exchanges a one-time enrolment token (minted for you by an org admin)
for a long-lived bearer, binding a freshly generated signing key to your
account. After enrolling, set [org_client] enabled = true in config.toml and
restart ` + "`observer start`" + ` to begin sharing content-free activity
rollups. Only activity AFTER enrolment is ever shared.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cleanup, enabled, err := buildOrgClient(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			enr, err := c.Enroll(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Enrolled in %s (org_id %s) as %s.\n", enr.OrgName, enr.OrgID, enr.UserEmail)
			fmt.Fprintf(out, "Pushing to %s.\n", enr.OrgServerURL)
			if !enabled {
				fmt.Fprintln(out, "\nNote: [org_client] enabled = false — set it to true and restart `observer start` to begin pushing.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func newUnenrollCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "unenroll",
		Short: "Leave the enrolled organisation and clear local credentials",
		Long: `Deletes the local enrolment and removes the bearer + signing key from the
OS keychain. A running daemon's push loop stops within one interval. This does
not delete anything already shared with the org server.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cleanup, _, err := buildOrgClient(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !st.Enrolled {
				fmt.Fprintln(out, "Not enrolled; nothing to do.")
				return nil
			}
			if err := c.Unenroll(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintf(out, "Unenrolled from %s. Local credentials cleared.\n", st.OrgName)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func newOrgCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "org",
		Short: "Organisation (Teams) enrolment status and manual push",
	}
	cmd.AddCommand(newOrgStatusCmd(), newOrgPushNowCmd())
	return cmd
}

func newOrgStatusCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show enrolment status and the last push",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cleanup, enabled, err := buildOrgClient(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !st.Enrolled {
				fmt.Fprintln(out, "Not enrolled.")
				fmt.Fprintf(out, "Credential store: %s\n", st.Backend)
				return nil
			}
			fmt.Fprintf(out, "Enrolled:         yes\n")
			fmt.Fprintf(out, "Organisation:     %s (org_id %s)\n", st.OrgName, st.OrgID)
			fmt.Fprintf(out, "User:             %s\n", st.UserEmail)
			fmt.Fprintf(out, "Server:           %s\n", st.OrgServerURL)
			fmt.Fprintf(out, "Enrolled at:      %s\n", st.EnrolledAt)
			fmt.Fprintf(out, "Credential store: %s\n", st.Backend)
			fmt.Fprintf(out, "Pushing enabled:  %t\n", enabled)
			if st.LastPush == nil {
				fmt.Fprintln(out, "Last push:        (none yet)")
			} else {
				lp := st.LastPush
				fmt.Fprintf(out, "Last push:        %s — %s, %d rows, %d bytes%s\n",
					lp.PushedAt, lp.Status, lp.RowCount, lp.Bytes, errSuffix(lp.Error))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func newOrgPushNowCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "push-now",
		Short: "Push any unpushed activity to the org server immediately",
		Long: `Runs one push cycle now instead of waiting for the interval. Useful to
verify enrolment end to end or to flush before shutting down.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, cleanup, _, err := buildOrgClient(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()
			res, err := c.PushOnce(cmd.Context())
			if errors.Is(err, orgclient.ErrNotEnrolled) {
				return errors.New("not enrolled; run `observer enroll <org-url> <token>` first")
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.Empty {
				fmt.Fprintln(out, "Nothing to push — the cursor is up to date.")
				return nil
			}
			fmt.Fprintf(out, "Pushed %d rows (%d bytes): accepted=%d deduped=%d\n",
				res.RowCount, res.Bytes, res.AcceptedRows, res.DedupedRows)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

func errSuffix(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
