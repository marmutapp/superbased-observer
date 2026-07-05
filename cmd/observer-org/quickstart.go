// quickstart.go — `observer-org quickstart` end-to-end bring-up.
//
// Issue 5 + the eleven friction points in the 2026-06-02 teams test
// findings asked for "admin runs one command and shares a link"
// (vs. compose up + scim profile + curl+jq + docker exec). This
// subcommand orchestrates that flow against the dev compose stack:
//
//   1. docker compose -f deploy/observer-org/docker-compose.yaml up -d --build
//   2. wait until /healthz reports OK
//   3. provision a default admin user via SCIM
//   4. mint an enrolment token for that user with the operator's TTL
//   5. print: Dashboard URL, dev-auth login command, enrol link
//
// All four steps are idempotent; re-running quickstart on a healthy
// stack just re-mints a token. Failure modes (missing docker, missing
// compose, port collision, etc.) surface to the operator with a
// terse remediation hint.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// quickstartDefaults are the dev-stack constants the subcommand assumes.
// They mirror what `deploy/observer-org/docker-compose.yaml` exposes.
const (
	quickstartComposeFile  = "deploy/observer-org/docker-compose.yaml"
	quickstartDashboardURL = "http://localhost:8443"
	quickstartSCIMToken    = "dev-scim-token-change-me" //nolint:gosec // dev-stack default; documented in compose
	quickstartAdminEmail   = "admin@example.com"
	quickstartReadyTimeout = 90 * time.Second
)

func newQuickstartCmd() *cobra.Command {
	var (
		ttlDays      int
		skipUp       bool
		dashboardURL string
		email        string
		publicHost   string
	)
	cmd := &cobra.Command{
		Use:   "quickstart",
		Short: "Bring up the dev org stack and print a ready-to-share enrol link",
		Long: `End-to-end dev setup against deploy/observer-org/docker-compose.yaml:
brings the stack up, waits for /healthz, provisions an admin user via
SCIM, mints an enrolment token, and prints the dashboard URL +
ready-to-share enrol command for a developer.

Re-runnable: each step is idempotent and just re-mints a fresh token
when the stack is already healthy. Pass --skip-up to skip the
` + "`docker compose up`" + ` step (useful when the stack is already
running under a different lifecycle).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			if !skipUp {
				// `docker compose up -d --build` runs under cmd.Context()
				// directly (no internal timeout). A cold Go-image build
				// can easily take several minutes; the prior
				// 120s-budget-for-the-whole-flow design SIGKILL'd it mid-
				// build and surfaced as "compose up: ... signal: killed"
				// (N2 in docs/teams-test-regression-2026-06-03.md). The
				// operator is watching compose's own output here; ^C
				// remains the abort path.
				fmt.Fprintln(out, "==> bringing the dev stack up (compose up -d --build)")
				if err := runComposeUp(cmd.Context()); err != nil {
					return fmt.Errorf("compose up: %w (is docker daemon running?)", err)
				}
			}

			// Only the post-up readiness + provisioning steps live under
			// the bounded readiness budget. Each shells out to localhost,
			// so 90s + a small overhead is plenty.
			ctx, cancel := context.WithTimeout(cmd.Context(), quickstartReadyTimeout+30*time.Second)
			defer cancel()

			fmt.Fprintln(out, "==> waiting for the org server's /healthz to report OK")
			if err := waitForHealth(ctx, dashboardURL); err != nil {
				return fmt.Errorf("readiness wait: %w", err)
			}

			fmt.Fprintln(out, "==> provisioning admin user via SCIM:", email)
			userID, err := scimProvisionUser(ctx, dashboardURL, email)
			if err != nil {
				return fmt.Errorf("scim provision: %w", err)
			}

			fmt.Fprintln(out, "==> minting enrolment token (TTL", ttlDays, "days)")
			token, err := mintTokenViaDevAuth(ctx, dashboardURL, userID, ttlDays, email)
			if errors.Is(err, errDevAuthUnavailable) {
				// Issue #2: dev-auth (the password-free login the mint
				// endpoint needs) is unavailable. Rather than dead-end on
				// a 401, fall back to the server-side mint by running
				// `observer-org invite` INSIDE the org container.
				fmt.Fprintln(out, "==> dev-auth unavailable ("+err.Error()+"); falling back to server-side mint via docker compose exec")
				token, err = mintViaContainerFallback(ctx, email, ttlDays)
			}
			if err != nil {
				return fmt.Errorf("mint token: %w", err)
			}

			enrollURL := dashboardURL
			if publicHost != "" {
				enrollURL = "http://" + publicHost + ":8443"
			}

			fmt.Fprintln(out)
			fmt.Fprintln(out, "Dashboard:")
			fmt.Fprintf(out, "  %s\n", dashboardURL)
			if publicHost != "" {
				// Issue #15/#1: print the exact SSH tunnel. 8088 is the dev
				// SAML IdP in the compose stack — forward it too so the
				// SAML dashboard login resolves through the tunnel.
				fmt.Fprintln(out)
				fmt.Fprintln(out, "Dashboard SSH tunnel (run on your laptop):")
				fmt.Fprintf(out, "  ssh -L 8443:127.0.0.1:8443 -L 8088:127.0.0.1:8088 root@%s\n", publicHost)
				fmt.Fprintln(out, "  then open http://127.0.0.1:8443  (8088 = dev SAML IdP)")
			}
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Dev-auth login (POST /auth/dev/login):")
			fmt.Fprintf(out, "  curl -fsSL -c cookies.txt %s/auth/dev/login -d email=%s\n", dashboardURL, email)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Share with a developer (enrol link):")
			fmt.Fprintf(out, "  observer enroll %s %s\n", enrollURL, token)
			fmt.Fprintln(out, "  (token is single-use and TTL", ttlDays, "days; re-run quickstart to mint another)")
			return nil
		},
	}
	cmd.Flags().IntVar(&ttlDays, "ttl-days", 7, "enrolment-token TTL in days")
	cmd.Flags().BoolVar(&skipUp, "skip-up", false, "skip the `docker compose up` step (stack is already running)")
	cmd.Flags().StringVar(&dashboardURL, "dashboard-url", quickstartDashboardURL, "URL of the running org server")
	cmd.Flags().StringVar(&email, "admin-email", quickstartAdminEmail, "email to provision via SCIM and mint the token for")
	cmd.Flags().StringVar(&publicHost, "public-host", "", "public IP/DNS of the org host — when set, prints the SSH tunnel command + a worker enrol URL against it")
	return cmd
}

// errDevAuthUnavailable signals that the dev-auth mint path failed in a
// way the server-side fallback can recover from (a 401/403 — dev-auth
// disabled or the session didn't take). mintTokenViaDevAuth wraps it so
// quickstart can branch to the container-exec fallback.
var errDevAuthUnavailable = errors.New("dev-auth mint path unavailable")

// mintViaContainerFallback mints an enrolment token by running
// `observer-org invite <email> --token-only` INSIDE the org container
// (the server-side mint path that doesn't need dev-auth). It reuses the
// same compose file quickstart brought up, so it only works against the
// local dev stack — exactly the case the 401 fallback targets.
func mintViaContainerFallback(ctx context.Context, email string, ttlDays int) (string, error) {
	args := []string{"compose", "-f", quickstartComposeFile, "exec", "-T", "org", "observer-org", "invite", email, "--token-only"}
	if ttlDays > 0 {
		args = append(args, "--ttl-days", strconv.Itoa(ttlDays))
	}
	raw, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return "", fmt.Errorf("server-side mint fallback (`docker %s`) failed: %w — enable [server].dev_auth=true on the org server, or run `observer-org invite %s` on the org host directly",
			strings.Join(args, " "), err, email)
	}
	// invite --token-only prints exactly the token; take the last
	// non-empty line to be robust against any leading exec noise.
	token := ""
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			token = s
		}
	}
	if token == "" {
		return "", fmt.Errorf("server-side mint fallback returned no token")
	}
	return token, nil
}

// runComposeUp shells out to `docker compose up -d --build`. It does
// NOT capture stdout — operators want to see the compose progress.
func runComposeUp(ctx context.Context) error {
	args := []string{"compose", "-f", quickstartComposeFile, "up", "-d", "--build"}
	c := exec.CommandContext(ctx, "docker", args...)
	c.Stdout = nil
	c.Stderr = nil
	if err := c.Run(); err != nil {
		return fmt.Errorf("`docker %s` failed: %w", strings.Join(args, " "), err)
	}
	return nil
}

// waitForHealth polls GET <base>/healthz until it returns 200 or the
// timeout fires.
func waitForHealth(ctx context.Context, base string) error {
	deadline := time.Now().Add(quickstartReadyTimeout)
	hc := &http.Client{Timeout: 3 * time.Second}
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			_ = resp.Body.Close()
			return nil
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timed out after %s waiting for %s/healthz", quickstartReadyTimeout, base)
}

// scimProvisionUser provisions a SCIM user with the given email and
// returns the resolved user id. Idempotent: a 409 from the server is
// treated as success (user already provisioned) and the existing user
// id is looked up via GET /scim/v2/Users.
func scimProvisionUser(ctx context.Context, base, email string) (string, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	body := fmt.Sprintf(`{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":%q,"emails":[{"value":%q,"primary":true}],"active":true}`, email, email)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/scim/v2/Users", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+quickstartSCIMToken)
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rbody, _ := io.ReadAll(resp.Body)
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var r struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rbody, &r); err != nil || r.ID == "" {
			return "", fmt.Errorf("could not parse SCIM POST response: %s", string(rbody))
		}
		return r.ID, nil
	case http.StatusConflict:
		// Existing user — fetch by email filter.
		return scimLookupUser(ctx, base, email)
	default:
		return "", fmt.Errorf("SCIM POST returned %d: %s", resp.StatusCode, string(rbody))
	}
}

// scimLookupUser fetches a user by email via the SCIM filter syntax.
func scimLookupUser(ctx context.Context, base, email string) (string, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	u, _ := url.Parse(base + "/scim/v2/Users")
	q := u.Query()
	q.Set("filter", fmt.Sprintf(`userName eq %q`, email))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+quickstartSCIMToken)
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rbody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SCIM lookup returned %d: %s", resp.StatusCode, string(rbody))
	}
	var r struct {
		Resources []struct {
			ID string `json:"id"`
		} `json:"Resources"`
	}
	if err := json.Unmarshal(rbody, &r); err != nil || len(r.Resources) == 0 {
		return "", fmt.Errorf("SCIM lookup returned no users for %s: %s", email, string(rbody))
	}
	return r.Resources[0].ID, nil
}

// mintTokenViaDevAuth logs in via dev-auth (so the session-protected
// /api/org/enrolment-tokens endpoint accepts the request) and mints a
// token for userID with the given TTL. Returns the token string.
//
// Dev-auth login is the only password-free login path in the org
// server; production deployments use SAML, which a CLI tool can't
// drive without browser interaction.
func mintTokenViaDevAuth(ctx context.Context, base, userID string, ttlDays int, email string) (string, error) {
	jar, err := newCookieJar()
	if err != nil {
		return "", err
	}
	hc := &http.Client{Timeout: 5 * time.Second, Jar: jar}

	// 1. dev-auth login → session cookie.
	loginBody := url.Values{"email": []string{email}}.Encode()
	loginReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/dev/login", strings.NewReader(loginBody))
	if err != nil {
		return "", err
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(loginReq)
	if err != nil {
		return "", err
	}
	rbody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("%w: dev-auth login returned %d (is [server].dev_auth=true?)", errDevAuthUnavailable, resp.StatusCode)
		}
		return "", fmt.Errorf("dev-auth login returned %d (is [server].dev_auth=true on the org server?): %s",
			resp.StatusCode, string(rbody))
	}

	// 2. mint the token.
	mintBody := fmt.Sprintf(`{"user_id":%q,"ttl_days":%d}`, userID, ttlDays)
	mintReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/org/enrolment-tokens", strings.NewReader(mintBody))
	if err != nil {
		return "", err
	}
	mintReq.Header.Set("Content-Type", "application/json")
	mintResp, err := hc.Do(mintReq)
	if err != nil {
		return "", err
	}
	mintRBody, _ := io.ReadAll(mintResp.Body)
	_ = mintResp.Body.Close()
	if mintResp.StatusCode != http.StatusOK {
		if mintResp.StatusCode == http.StatusUnauthorized || mintResp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("%w: mint returned %d (login required)", errDevAuthUnavailable, mintResp.StatusCode)
		}
		return "", fmt.Errorf("mint returned %d: %s", mintResp.StatusCode, string(mintRBody))
	}
	var r struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mintRBody, &r); err != nil || r.Token == "" {
		return "", fmt.Errorf("could not parse mint response: %s", string(mintRBody))
	}
	return r.Token, nil
}

// newCookieJar returns a fresh cookiejar.Jar; the dev-auth session
// cookie needs to round-trip from POST /auth/dev/login to POST
// /api/org/enrolment-tokens within a single quickstart invocation.
func newCookieJar() (http.CookieJar, error) {
	return cookiejar.New(nil)
}
