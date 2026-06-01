package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/crewjam/saml/samlsp"
	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgserver/auth"
	"github.com/marmutapp/superbased-observer/internal/orgserver/config"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
)

// newDoctorCmd validates the config and the runtime environment end to end.
// It prints one line per check and exits non-zero (exitErr) if any fail.
func newDoctorCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Validate config + environment; exit non-zero on any problem",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()

			var failures int
			check := func(name string, err error) {
				if err != nil {
					failures++
					fmt.Fprintf(out, "✗ %s: %v\n", name, err)
					return
				}
				fmt.Fprintf(out, "✓ %s\n", name)
			}

			check("config valid", config.Validate(cfg))
			check("bearer signing key decodable", checkBearerKey(cfg.Bearer.SigningKeyPath))
			check("session key present (>=32 bytes)", checkSessionKey(cfg.Server.SessionKeyPath))
			check("SCIM token file mode 0600", checkSCIMTokenMode(cfg.SCIM.AuthTokenPath))
			check("SP cert/key loadable", checkSPKeypair(cfg.SAML.SPCertPath, cfg.SAML.SPKeyPath))
			check("IdP metadata reachable", checkIDPMetadata(ctx, cfg.SAML.IDPMetadataURL))
			check("DB writable", checkDBWritable(ctx, cfg.Server.DBPath))
			check("listen port available", checkListen(cfg.Server.Listen))

			if failures > 0 {
				fmt.Fprintf(out, "\n%d check(s) failed\n", failures)
				return exitErr(1)
			}
			fmt.Fprintln(out, "\nall checks passed")
			return nil
		},
	}
}

func checkBearerKey(path string) error {
	if path == "" {
		return fmt.Errorf("bearer.signing_key_path not set")
	}
	_, err := auth.LoadSigningKey(path)
	return err
}

func checkSessionKey(path string) error {
	if path == "" {
		return fmt.Errorf("server.session_key_path not set")
	}
	_, err := auth.LoadSessionKey(path)
	return err
}

func checkSCIMTokenMode(path string) error {
	if path == "" {
		return fmt.Errorf("scim.auth_token_path not set")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("permissions are %04o, want 0600 (group/other must have no access)", perm)
	}
	return nil
}

func checkSPKeypair(certPath, keyPath string) error {
	if certPath == "" || keyPath == "" {
		return fmt.Errorf("saml.sp_cert_path / sp_key_path not set")
	}
	if _, err := os.Stat(certPath); err != nil {
		return fmt.Errorf("cert: %w", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return fmt.Errorf("key: %w", err)
	}
	return nil
}

func checkIDPMetadata(ctx context.Context, metadataURL string) error {
	if metadataURL == "" {
		return fmt.Errorf("saml.idp_metadata_url not set")
	}
	u, err := url.Parse(metadataURL)
	if err != nil || u.Scheme == "" {
		return fmt.Errorf("invalid URL %q", metadataURL)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if _, err := samlsp.FetchMetadata(ctx, client, *u); err != nil {
		return err
	}
	return nil
}

func checkDBWritable(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("server.db_path not set")
	}
	db, err := orgdb.Open(ctx, orgdb.Options{Path: path})
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _doctor_probe (k INTEGER)`); err != nil {
		return err
	}
	_, _ = db.ExecContext(ctx, `DROP TABLE IF EXISTS _doctor_probe`)
	return nil
}

func checkListen(addr string) error {
	if addr == "" {
		return fmt.Errorf("server.listen not set")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	_ = ln.Close()
	return nil
}
