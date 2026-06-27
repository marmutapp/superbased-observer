// policy.go — `observer-org policy` subcommands: the G13 authoring
// surface of the org guard-policy bundle channel (guard spec §14.2).
//
// Publishing is a direct-DB operation (the new-enrolment-token
// pattern): the operator runs it on the server host with access to
// both the server DB and the policy signing key, so the long-running
// server process never holds the private key for serving. Dashboard
// authoring with RBAC (policy_admin) joins in G14 on top of the same
// api.PublishPolicyBundle gate.
package main

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

func newPolicyCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "policy",
		Short: "Author and inspect org guard-policy bundles (guard spec §14.2)",
	}
	cmd.AddCommand(
		newPolicyKeygenCmd(configPath),
		newPolicyPublishCmd(configPath),
		newPolicyListCmd(configPath),
		newPolicyShowCmd(configPath),
	)
	return cmd
}

// newPolicyKeygenCmd generates the Ed25519 policy signing key at
// [policy].signing_key_path (PEM PKCS#8, 0600). Refuses to overwrite.
func newPolicyKeygenCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "keygen",
		Short: "Generate the Ed25519 policy signing key at [policy].signing_key_path",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			keyPath := cfg.Policy.SigningKeyPath
			if keyPath == "" {
				return errors.New("observer-org policy keygen: set [policy].signing_key_path in the config first")
			}
			if _, err := os.Stat(keyPath); err == nil {
				return fmt.Errorf("observer-org policy keygen: %s already exists — rotating the policy key invalidates every agent's pin (agents must re-enrol); delete the file explicitly if that is intended", keyPath)
			}
			priv, pemBytes, err := auth.GenerateSigningKeyPEM()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
				return fmt.Errorf("observer-org policy keygen: %w", err)
			}
			if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
				return fmt.Errorf("observer-org policy keygen: %w", err)
			}
			pub := auth.EncodePublicKey(priv.Public().(ed25519.PublicKey))
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "policy signing key written: %s\n", keyPath)
			fmt.Fprintf(out, "public key (delivered to agents at enrolment): %s\n", pub)
			fmt.Fprintln(out, "\nRestart the server so enrolments deliver the key, then publish with: observer-org policy publish --file <bundle.toml>")
			return nil
		},
	}
}

// newPolicyPublishCmd validates, signs and stores a bundle as the next
// version.
func newPolicyPublishCmd(configPath *string) *cobra.Command {
	var (
		file        string
		description string
	)
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Validate, sign and publish a policy bundle as the next version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("observer-org policy publish: --file is required")
			}
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			if cfg.Policy.SigningKeyPath == "" {
				return errors.New("observer-org policy publish: set [policy].signing_key_path and run `observer-org policy keygen` first")
			}
			priv, err := auth.LoadSigningKey(cfg.Policy.SigningKeyPath)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("observer-org policy publish: %w", err)
			}
			ctx := cmd.Context()
			db, err := orgdb.Open(ctx, orgdb.Options{Path: cfg.Server.DBPath})
			if err != nil {
				return err
			}
			defer db.Close()

			version, err := api.PublishPolicyBundle(ctx, db, priv, string(raw), currentOSUser(), description)
			if err != nil {
				return fmt.Errorf("observer-org policy publish: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "published policy bundle version %d (%d bytes)\n", version, len(raw))
			fmt.Fprintln(out, "Agents pick it up at their next poll (default hourly) or `observer start`.")
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to the bundle TOML (guard §4.4 [[rule]]/[[override]] format) (required)")
	cmd.Flags().StringVar(&description, "description", "", "operator note recorded in the version history")
	return cmd
}

// newPolicyListCmd prints the version history, newest first.
func newPolicyListCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List published bundle versions, newest first",
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
			metas, err := api.ListPolicyBundles(ctx, db)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(metas) == 0 {
				fmt.Fprintln(out, "no policy bundles published")
				return nil
			}
			for _, m := range metas {
				fmt.Fprintf(out, "v%-4d %s  %4d bytes  by %-20s %s\n",
					m.Version, m.SignedAt, m.TOMLBytes, m.CreatedBy, m.Description)
			}
			return nil
		},
	}
}

// newPolicyShowCmd prints one bundle's TOML (latest by default).
func newPolicyShowCmd(configPath *string) *cobra.Command {
	var version int64
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print a published bundle's TOML (latest by default)",
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
			b, err := api.PolicyBundleByVersion(ctx, db, version)
			if errors.Is(err, api.ErrNoPolicyBundle) {
				return errors.New("observer-org policy show: no such bundle")
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# version %d, signed %s\n", b.Version, b.SignedAt)
			if b.Description != "" {
				fmt.Fprintf(out, "# %s\n", b.Description)
			}
			fmt.Fprint(out, b.BundleTOML)
			return nil
		},
	}
	cmd.Flags().Int64Var(&version, "version", 0, "bundle version to show (0 = latest)")
	return cmd
}

// currentOSUser returns the operator identity recorded as created_by
// ("" when unresolvable — identity here is provenance, not auth).
func currentOSUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}
