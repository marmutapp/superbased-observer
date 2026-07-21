package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgserver/config"
	orgdb "github.com/marmutapp/superbased-observer/internal/orgserver/db"
	"github.com/marmutapp/superbased-observer/internal/orgserver/m365copilotanalytics"
)

// newM365Cmd is the parent for Microsoft 365 Copilot rail diagnostics. It hangs
// under the root so the surface reads `observer-org m365 doctor`.
func newM365Cmd(configPath *string) *cobra.Command {
	m := &cobra.Command{
		Use:   "m365",
		Short: "Microsoft 365 Copilot analytics rail diagnostics",
	}
	m.AddCommand(newM365DoctorCmd(configPath))
	return m
}

// newM365DoctorCmd checks that the M365 Copilot rail can authenticate to Entra
// and reach Microsoft Graph, reusing the server's exact secret-resolution and
// token code paths. It is safe to run against a misconfigured server: every
// failure class prints an actionable message and exits non-zero, and the client
// secret is never printed or echoed in any output or error.
func newM365DoctorCmd(configPath *string) *cobra.Command {
	var userFlag string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the M365 Copilot rail can authenticate + reach Microsoft Graph",
		Long: "Loads the org-server config, resolves the Entra app credentials the way\n" +
			"the poller does, performs a real client-credentials token fetch and one\n" +
			"bounded Graph probe, and reports success or the exact Entra/Graph error\n" +
			"with a fix hint. Exits non-zero on any problem so it is scriptable. The\n" +
			"client secret is never printed.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			return runM365Doctor(cmd, cfg, *configPath, userFlag)
		},
	}
	cmd.Flags().StringVar(&userFlag, "user", "",
		"Graph user (UPN/email/object id) to probe; default: first provisioned org member")
	return cmd
}

// runM365Doctor is the doctor body, factored out for testability. It writes
// human-readable check lines to the command's stdout and returns exitErr(1) on
// any failure. It never emits the client secret.
func runM365Doctor(cmd *cobra.Command, cfg config.Config, configPath, userFlag string) error {
	out := cmd.OutOrStdout()
	m := cfg.M365Copilot

	// 1. Rail enabled?
	if !m.Enabled {
		fmt.Fprintln(out, "✗ [m365_copilot] rail is disabled")
		fmt.Fprintf(out, "  Set enabled = true under [m365_copilot] in %s to activate the rail.\n",
			configPathOrDefault(configPath))
		return exitErr(1)
	}

	// 2. Config completeness (tenant_id / client_id inline; secret via env or file).
	logger := newLogger(cfg.Server.LogLevel, "") // gives B4 the perm-warn on the doctor path too
	secret, serr := m365copilotanalytics.ResolveClientSecret(m.ClientSecretFile, logger)

	var missing []string
	if strings.TrimSpace(m.TenantID) == "" {
		missing = append(missing, "tenant_id (Entra tenant id) — set it under [m365_copilot]")
	}
	if strings.TrimSpace(m.ClientID) == "" {
		missing = append(missing, "client_id (Entra app id) — set it under [m365_copilot]")
	}
	if serr != nil {
		missing = append(missing, clientSecretHint(m.ClientSecretFile))
	}
	if len(missing) > 0 {
		fmt.Fprintln(out, "✗ [m365_copilot] configuration incomplete:")
		for _, x := range missing {
			fmt.Fprintf(out, "  - missing %s\n", x)
		}
		fmt.Fprintf(out, "  Config file: %s (never inline the client secret in TOML).\n",
			configPathOrDefault(configPath))
		return exitErr(1)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	// 3. Real client-credentials token fetch (the same TokenSource the server uses).
	tokenSrc := m365copilotanalytics.NewTokenSource(m.TenantID, m.ClientID, secret, m.LoginBaseURL, nil)
	if _, err := tokenSrc.Token(ctx); err != nil {
		fmt.Fprintf(out, "✗ Entra token request failed: %v\n", err)
		fmt.Fprintf(out, "  fix: %s\n", entraFixHint(err))
		return exitErr(1)
	}
	fmt.Fprintf(out, "✓ Entra client-credentials token acquired (tenant %s)\n", m.TenantID)

	// 4. One bounded Graph probe for a single user.
	probeUser := resolveProbeUser(ctx, cfg, userFlag)
	if probeUser == "" {
		fmt.Fprintln(out, "✗ authenticated, but no user to probe")
		fmt.Fprintln(out, "  getAllEnterpriseInteractions is per-user; Graph reachability is unverified.")
		fmt.Fprintln(out, "  fix: provision members via SCIM, or pass --user <licensed-upn>.")
		return exitErr(1)
	}

	poller, perr := m365copilotanalytics.NewPoller(nil, string(m365copilotanalytics.SurfaceGraph),
		m.GraphBaseURL, tokenSrc, m.TenantID, "")
	if perr != nil {
		return fmt.Errorf("m365 doctor: build probe poller: %w", perr)
	}
	end := time.Now().UTC().Add(-time.Duration(m.LagToleranceHours) * time.Hour)
	start := end.AddDate(0, 0, -7)
	count, err := poller.ProbeGraph(ctx, probeUser, start, end)
	if err != nil {
		fmt.Fprintf(out, "✗ Microsoft Graph probe failed for user %s: %v\n", probeUser, err)
		fmt.Fprintf(out, "  fix: %s\n", graphFixHint(err))
		return exitErr(1)
	}
	fmt.Fprintf(out, "✓ rail can authenticate + reached Graph; %d interaction(s) visible in the probe window (last 7 days, user %s)\n",
		count, probeUser)

	// 5. Honest surface note: only Rail A (graph) collects data; purview is scaffolded.
	if surfacesIncludePurview(m.Surfaces) {
		fmt.Fprintln(out, "  note: the 'purview' surface is scaffolded (metadata rail not yet wired) — only 'graph' (Rail A) collects data today.")
	}
	return nil
}

// resolveProbeUser picks the Graph user to probe: the explicit --user flag wins,
// else the first provisioned org member (best-effort — a DB open/query failure
// yields "", which the caller reports honestly rather than crashing).
func resolveProbeUser(ctx context.Context, cfg config.Config, userFlag string) string {
	if u := strings.TrimSpace(userFlag); u != "" {
		return u
	}
	if strings.TrimSpace(cfg.Server.DBPath) == "" {
		return ""
	}
	db, err := orgdb.Open(ctx, orgdb.Options{Path: cfg.Server.DBPath})
	if err != nil {
		return ""
	}
	defer func() { _ = db.Close() }()
	users, err := m365copilotanalytics.ResolveUserIDs(ctx, db, "")
	if err != nil || len(users) == 0 {
		return ""
	}
	return users[0]
}

// configPathOrDefault names the config file for an actionable message.
func configPathOrDefault(p string) string {
	if strings.TrimSpace(p) == "" {
		return config.DefaultPath
	}
	return p
}

// clientSecretHint describes where to set the client secret (never the value).
func clientSecretHint(secretFile string) string {
	if strings.TrimSpace(secretFile) == "" {
		return "client secret — set the M365_COPILOT_CLIENT_SECRET env var, or set client_secret_file under [m365_copilot]"
	}
	return fmt.Sprintf("client secret — M365_COPILOT_CLIENT_SECRET is unset and client_secret_file %q is empty or unreadable", secretFile)
}

// entraFixHint maps an Entra token error (machine code carried in the error
// string) to a remediation hint. The input error is never secret-bearing.
func entraFixHint(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "invalid_client"):
		return "Entra rejected the app credentials (invalid_client): regenerate the client secret in the app registration and update M365_COPILOT_CLIENT_SECRET / client_secret_file."
	case strings.Contains(s, "unauthorized_client"):
		return "the app is not authorized for the client-credentials grant in this tenant; check the app registration and tenant_id."
	case strings.Contains(s, "invalid_request"), strings.Contains(s, "invalid_tenant"):
		return "check tenant_id and client_id under [m365_copilot] (a malformed tenant or client id)."
	case strings.Contains(s, "aadsts"):
		return "see the AADSTS code above; verify tenant_id, client_id and the client secret."
	default:
		return "verify tenant_id / client_id / client secret and that the Entra app exists in this tenant."
	}
}

// graphFixHint maps a Graph probe error to a remediation hint. The input error is
// never secret-bearing (the poller wraps status codes / parse errors only).
func graphFixHint(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "consent_required"), strings.Contains(s, "authorization_requestdenied"), strings.Contains(s, "403"):
		return "grant admin consent for AiEnterpriseInteraction.Read.All (application permission) on the Entra app; a 403/Authorization_RequestDenied means the permission is not consented."
	case strings.Contains(s, "404"):
		return "the probe user was not found or has no Copilot interaction history; try --user <licensed-upn>."
	case strings.Contains(s, "401"):
		return "Graph rejected the token (401); re-check the application permission and admin consent."
	default:
		return "verify admin consent for AiEnterpriseInteraction.Read.All and that the tenant is on the global commercial cloud (GCC-High/DoD/21Vianet are not supported)."
	}
}

// surfacesIncludePurview reports whether the configured surfaces list the
// scaffolded purview rail.
func surfacesIncludePurview(surfaces []string) bool {
	for _, s := range surfaces {
		if strings.TrimSpace(s) == string(m365copilotanalytics.SurfacePurview) {
			return true
		}
	}
	return false
}
