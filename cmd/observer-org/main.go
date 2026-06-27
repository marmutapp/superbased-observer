// Command observer-org is the SuperBased Observer org server: the
// customer-self-hosted service that agents enrol against and push to, and
// that hosts the SAML-authenticated org dashboard and SCIM provisioning.
//
// It is a wholly separate binary from `observer` (the agent): different
// config, different database, different deployment. Subcommands:
//
//	serve                 run the HTTP server until SIGINT/SIGTERM
//	migrate               apply DB migrations and report the schema version
//	new-enrolment-token   mint a one-time enrolment token for a user (admin)
//	policy                author/inspect signed guard-policy bundles (keygen|publish|list|show)
//	dump-config           print the effective config (no secrets, only paths)
//	doctor                validate config + environment, exit non-zero on any problem
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgserver"
	"github.com/marmutapp/superbased-observer/internal/orgserver/api"
	"github.com/marmutapp/superbased-observer/internal/orgserver/config"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// version is stamped at build time via -ldflags="-X main.version=...".
var version = "dev"

// exitErr carries a specific process exit code up to main().
type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var ec exitErr
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		configPath string
		logLevel   string
	)
	root := &cobra.Command{
		Use:          "observer-org",
		Short:        "SuperBased Observer org server",
		Long:         "Customer-self-hosted org server: agent enrolment + push, SAML dashboard, SCIM provisioning.",
		Version:      version,
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", config.DefaultPath, "path to the org server config TOML")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "", "log level: debug|info|warn|error (overrides config)")

	root.AddCommand(
		newServeCmd(&configPath, &logLevel),
		newMigrateCmd(&configPath),
		newEnrolmentTokenCmd(&configPath),
		newTokensCmd(&configPath),
		newUsersCmd(&configPath),
		newInviteCmd(&configPath),
		newPolicyCmd(&configPath),
		newDumpConfigCmd(&configPath),
		newDoctorCmd(&configPath),
		newScrubContentCmd(&configPath),
		newQuickstartCmd(),
	)
	return root
}

// loadConfig loads the config and, unless validation is skipped, validates it.
func loadConfig(path string) (config.Config, error) {
	return config.Load(path)
}

// newLogger builds a slog logger; the flag (when set) overrides the config.
func newLogger(cfgLevel, flagLevel string) *slog.Logger {
	level := cfgLevel
	if flagLevel != "" {
		level = flagLevel
	}
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func newServeCmd(configPath, logLevel *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the org server until SIGINT/SIGTERM",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			logger := newLogger(cfg.Server.LogLevel, *logLevel)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			srv, err := orgserver.New(ctx, cfg, logger)
			if err != nil {
				return err
			}
			return srv.Run(ctx)
		},
	}
}

func newMigrateCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply DB migrations and report the schema version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			if cfg.Server.DBPath == "" {
				return errors.New("observer-org migrate: server.db_path is required")
			}
			ctx := cmd.Context()
			db, err := orgdb.Open(ctx, orgdb.Options{Path: cfg.Server.DBPath})
			if err != nil {
				return err
			}
			defer db.Close()
			v, err := orgdb.Version(ctx, db)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "migrations applied; schema version = %d\n", v)
			return nil
		},
	}
}

func newEnrolmentTokenCmd(configPath *string) *cobra.Command {
	var (
		userID  string
		ttlDays int
	)
	cmd := &cobra.Command{
		Use:   "new-enrolment-token",
		Short: "Mint a one-time enrolment token for a user",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if userID == "" {
				return errors.New("observer-org new-enrolment-token: --user-id is required")
			}
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

			ttl := time.Duration(ttlDays) * 24 * time.Hour
			if ttlDays <= 0 {
				ttl = time.Duration(cfg.Enrolment.DefaultTokenLifetimeDays) * 24 * time.Hour
			}
			token, id, expires, err := api.MintEnrolmentTokenForUser(ctx, db, userID, ttl)
			if errors.Is(err, api.ErrUserNotFound) {
				return fmt.Errorf("observer-org new-enrolment-token: no such user %q (provision via SCIM first)", userID)
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "token:      %s\n", token)
			fmt.Fprintf(out, "token_id:   %s\n", id)
			fmt.Fprintf(out, "user_id:    %s\n", userID)
			fmt.Fprintf(out, "expires_at: %s\n", expires.UTC().Format(time.RFC3339))
			fmt.Fprintln(out, "\nGive the agent operator:  observer enroll <org-url> <token>")
			fmt.Fprintln(out, "(this token is shown once and cannot be retrieved again)")
			return nil
		},
	}
	cmd.Flags().StringVar(&userID, "user-id", "", "SCIM user id to bind the token to (required)")
	cmd.Flags().IntVar(&ttlDays, "ttl-days", 0, "token lifetime in days (default: config enrolment.default_token_lifetime_days)")
	return cmd
}

func newDumpConfigCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "dump-config",
		Short: "Print the effective config (paths only, never secrets)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(*configPath)
			if err != nil {
				return err
			}
			out, err := config.Dump(cfg)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), out)
			return nil
		},
	}
}
