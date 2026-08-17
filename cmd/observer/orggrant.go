package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/govern"
	"github.com/marmutapp/superbased-observer/internal/govern/sidecar"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Admin-controlled Plane B, the CLI consent surface
// (docs/plans/admin-controlled-plane-b-spec-2026-08-15.md §2.3/§2.4,
// adversarial review A4). The grant is written HERE and nowhere else:
// internal/orgclient has no TTY and, by the time it holds the response, it
// has already saved the bearer, written the enrolment and bumped the
// generation — there is no honest place there to ask a human anything.

// authorityPlainEnglish is the plain-language rendering of each authority
// token. A prompt that shows only machine tokens is a consent theatre, so
// every token in the closed vocabulary has a sentence here, and an UNKNOWN
// token is rendered honestly as "this version of Observer does not
// understand this, and will not act on it".
func authorityPlainEnglish(tok string) string {
	switch tok {
	case govern.AuthorityDashboardVisibility:
		return "hide or lock pages and settings sections in THIS dashboard"
	case govern.AuthoritySettingsPin:
		return "pin some local settings so they cannot be changed here"
	case govern.AuthorityCapturePin:
		return "reduce, or lock at its current level, what this machine shares with the organisation. It can never increase it"
	case govern.AuthorityCaptureRaise:
		// RETIRED (Phase 1b §2.3). A grant carrying it grants nothing. Say
		// so plainly: the fix is a fresh `observer enroll`, which is a
		// node-side act, not an admin action.
		return "RETIRED - this token grants nothing. Re-run `observer enroll` if your organisation needs to manage sharing"
	case govern.AuthorityFeatureLock:
		return "force some features on or off"
	default:
		return "UNKNOWN to this version of Observer - it will be recorded but never acted on"
	}
}

// confirmAndStoreGrant prints the offered grant, obtains consent, and stores
// it. The rules, in the order they matter:
//
//   - a TTY gets an explicit y/N prompt naming every authority token;
//   - no TTY and no --accept-governance means ENROL WITHOUT THE GRANT, with
//     a loud warning naming the flag. Silently accepting governance because
//     nobody was watching would make the consent claim false, and the admin
//     sees an ungoverned node in fleet state, which is the honest signal;
//   - declining is not an error: the node is enrolled and ungoverned.
func confirmAndStoreGrant(cmd *cobra.Command, st *store.Store, offer *orgclient.GrantOffer, accept bool) error {
	out := cmd.OutOrStdout()
	printGrantOffer(out, offer)

	switch {
	case accept:
		fmt.Fprintln(out, "\nAccepted via --accept-governance.")
	case isInteractiveTerminal(cmd):
		ok, err := promptYesNo(cmd, "Accept these settings from this organisation? [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "\nDeclined. This machine is enrolled and reporting, but NOT governed:")
			fmt.Fprintln(out, "  nothing about this dashboard will be changed by the organisation.")
			return nil
		}
	default:
		fmt.Fprintln(out, "\nNot accepted: this is not an interactive terminal and --accept-governance was not passed.")
		fmt.Fprintln(out, "  This machine is enrolled and reporting, but NOT governed. Re-run `observer enroll`")
		fmt.Fprintln(out, "  interactively, or pass --accept-governance, to accept the settings above.")
		return nil
	}

	row := store.EnrolmentGrant{
		OrgKey:       offer.OrgKey,
		Generation:   offer.Generation,
		OrgID:        offer.Grant.OrgID,
		OrgName:      grantOrgName(cmd, offer),
		OrgServerURL: offer.Grant.OrgServerURL,
		KeyPinSHA256: offer.KeyPinSHA256,
		Authority:    offer.Grant.Authority,
		ConsentMode:  govern.ConsentInteractive,
		ConsentActor: localConsentActor(),
		GrantedAt:    parseRFC3339OrNow(offer.Grant.GrantedAt),
		ExpiresAt:    parseRFC3339OrZero(offer.Grant.ExpiresAt),
		// The SIGNED window, set at the one moment it is by definition
		// identical to ExpiresAt (review M1). Without it, migration 083's
		// NOT NULL DEFAULT '' leaves it empty on every fresh enrolment, the
		// derived renewal TTL is NEGATIVE, and renewal silently never fires
		// for any node enrolled after Phase 1b ships.
		SignedExpiresAt: parseRFC3339OrZero(offer.Grant.ExpiresAt),
		Signature:       offer.Grant.Signature,
		ReceiptHash:     offer.ReceiptHash,
	}
	if err := st.WriteEnrolmentGrant(cmd.Context(), row); err != nil {
		return fmt.Errorf("observer enroll: could not record the governance grant: %w", err)
	}
	fmt.Fprintln(out, "Recorded. Run `observer org grant show` at any time to see exactly what this machine granted,")
	fmt.Fprintln(out, "and `observer unenroll` to revoke it.")
	return nil
}

// printGrantOffer renders the offer. Plain hyphens, no em-dashes (the
// user-facing copy rule).
func printGrantOffer(out io.Writer, offer *orgclient.GrantOffer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "This organisation is asking to manage some of this machine's Observer settings.")
	fmt.Fprintf(out, "  Organisation: %s\n", offer.Grant.OrgID)
	fmt.Fprintf(out, "  Server:       %s\n", offer.Grant.OrgServerURL)
	if offer.Grant.ExpiresAt != "" {
		fmt.Fprintf(out, "  Expires:      %s (after this, local settings apply again)\n", offer.Grant.ExpiresAt)
	}
	fmt.Fprintln(out, "\nIf you accept, the organisation may:")
	for _, tok := range offer.Grant.Authority {
		fmt.Fprintf(out, "  - %s\n     (%s)\n", authorityPlainEnglish(tok), tok)
	}
	fmt.Fprintln(out, "\nIt may NOT:")
	fmt.Fprintln(out, "  - read your code, your files, or your command output")
	fmt.Fprintln(out, "  - hide the Privacy page or the enrolment settings, so you can always see")
	fmt.Fprintln(out, "     what is shared and who manages this machine")
	fmt.Fprintln(out, "  - stop you leaving: `observer unenroll` removes this at any time")
}

// grantOrgName prefers the enrolment's own org name for display; the grant
// itself carries only the org id (the display name rides the signed policy
// body's notice, not the grant).
func grantOrgName(cmd *cobra.Command, offer *orgclient.GrantOffer) string {
	_ = cmd
	return offer.Grant.OrgID
}

func localConsentActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "local"
}

func parseRFC3339OrNow(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Now().UTC()
}

func parseRFC3339OrZero(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// isInteractiveTerminal reports whether stdin is a real terminal. A cobra
// command whose InOrStdin is not os.Stdin (tests, pipes) is never
// interactive.
func isInteractiveTerminal(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

func promptYesNo(cmd *cobra.Command, prompt string) (bool, error) {
	fmt.Fprint(cmd.OutOrStdout(), "\n"+prompt)
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return false, nil // EOF with no answer is a decline, never a crash
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

// newOrgGrantCmd is `observer org grant`. Its one subcommand prints the full
// stored grant with a verify line, so the developer (or an auditor) can
// always read what this machine handed over and to whom.
func newOrgGrantCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Show the organisation governance grant this machine recorded at enrolment",
	}
	cmd.AddCommand(newOrgGrantShowCmd())
	return cmd
}

func newOrgGrantShowCmd() *cobra.Command {
	var configPath string
	c := &cobra.Command{
		Use:   "show",
		Short: "Print the recorded enrolment grant and what it authorises",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()
			out := cmd.OutOrStdout()

			loader := governanceIdentityLoader(b.store)
			grant, live, err := loader(cmd.Context())
			if err != nil {
				return err
			}
			if !live.Enrolled {
				fmt.Fprintln(out, "Not enrolled, so nothing manages this machine.")
				return nil
			}
			if grant == nil {
				fmt.Fprintln(out, "Enrolled, but NOT governed: this machine granted no authority to the organisation.")
				fmt.Fprintln(out, "Your dashboard and settings are entirely local.")
				return nil
			}
			fmt.Fprintf(out, "Granted to:   %s (org_id %s)\n", grant.OrgName, grant.OrgID)
			fmt.Fprintf(out, "Server:       %s\n", grant.OrgServerURL)
			fmt.Fprintf(out, "Granted at:   %s\n", formatGrantStamp(grant.GrantedAt))
			row, _, _ := b.store.LoadEnrolmentGrant(cmd.Context(), live.OrgKey) //nolint:errcheck // display-only; a read failure just omits the renewal detail
			fmt.Fprintln(out, expiryLine(grant, row))
			fmt.Fprintf(out, "Consent:      %s (%s)\n", grant.ConsentMode, grant.ConsentActor)
			fmt.Fprintf(out, "Receipt:      %s\n", grant.ReceiptHash)
			fmt.Fprintln(out, "\nAuthority granted:")
			for _, tok := range grant.Authority {
				fmt.Fprintf(out, "  - %-24s %s\n", tok, authorityPlainEnglish(tok))
			}

			// The one honest verification line: is the key this grant was
			// bound to still the key this node pins?
			switch {
			case grant.KeyPinSHA256 == "":
				fmt.Fprintln(out, "\nSigning key:  NOT BOUND (this grant predates key binding and will not be honoured)")
			case grant.KeyPinSHA256 == live.KeyPinSHA256:
				fmt.Fprintf(out, "\nSigning key:  verified against the pinned organisation key (%s)\n", short(grant.KeyPinSHA256))
			default:
				fmt.Fprintf(out, "\nSigning key:  MISMATCH - grant bound to %s, this machine now pins %s.\n",
					short(grant.KeyPinSHA256), short(live.KeyPinSHA256))
				fmt.Fprintln(out, "              The grant is NOT being honoured. Re-enrol to re-establish it.")
			}
			if grant.Generation != live.Generation {
				fmt.Fprintf(out, "Enrolment:    STALE - grant recorded for enrolment %d, this machine is on %d.\n",
					grant.Generation, live.Generation)
				fmt.Fprintln(out, "              The grant is NOT being honoured.")
			}
			if !grant.ExpiresAt.IsZero() && time.Now().UTC().After(grant.ExpiresAt) {
				fmt.Fprintln(out, "Status:       EXPIRED - local settings apply again.")
			}
			printPinnedSettings(out, b.cfg, configPath)
			fmt.Fprintln(out, "\nRun `observer unenroll` to revoke this at any time.")
			return nil
		},
	}
	c.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return c
}

func formatGrantStamp(t time.Time) string {
	if t.IsZero() {
		return "(none)"
	}
	return t.Format(time.RFC3339)
}

func short(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// expiryLine renders the grant's clock HONESTLY: the working expiry, plus —
// when the grant has been renewed — the signed expiry it was derived from.
//
// Both are printed because renewal deliberately does not destroy the
// evidence property (amendment A1 / §4.4): signed_expires_at preserves what
// the organization actually signed, so an auditor can still verify the
// stored signature against its own row, while expires_at is the working
// clock the resolver enforces.
func expiryLine(grant *govern.Grant, row store.EnrolmentGrant) string {
	line := fmt.Sprintf("Expires:      %s", formatGrantStamp(grant.ExpiresAt))
	if row.LastRenewedAt.IsZero() || row.SignedExpiresAt.IsZero() {
		return line
	}
	return fmt.Sprintf("%s (renewed %s; originally signed to expire %s)",
		line, row.LastRenewedAt.Format("2006-01-02"), row.SignedExpiresAt.Format("2006-01-02"))
}

// printPinnedSettings is the §1.7 disclosure block. It is NON-NEGOTIABLE
// (threat T8): a developer must be able to see the MECHANISM, not just its
// output — which key, what value, who set it, where the file is, and
// crucially whether the config THIS VERY PROCESS loaded actually applied it.
func printPinnedSettings(out io.Writer, cfg config.Config, configPath string) {
	path := config.ResolveGovernanceSidecarPath(cfg, "")
	// The re-Load MUST honour the same --config the rest of this command
	// resolved, or the disclosure reads the DEFAULT machine config's sidecar
	// while printing this config's path — an "absent" verdict about a file
	// that exists.
	_, gout, err := config.LoadGovernance(config.LoadOptions{GlobalPath: configPath})
	fmt.Fprintln(out, "\nPinned settings:")
	fmt.Fprintf(out, "  Effective-settings file: %s\n", path)
	switch {
	case err != nil:
		// Structurally unreachable — Load never fails because of governance
		// — but reported rather than swallowed if it ever happens.
		fmt.Fprintf(out, "  Could not re-read this machine's configuration: %v\n", err)
		return
	case gout.Discarded:
		fmt.Fprintln(out, "  NONE APPLIED - the organisation's settings were refused by this machine's own")
		fmt.Fprintf(out, "  configuration check and every one of them was ignored: %s\n", gout.DiscardErr)
		return
	case len(gout.Applied) == 0:
		fmt.Fprintf(out, "  None. (%s)\n", pinnedAbsenceReason(gout.Reason))
	default:
		for _, key := range gout.Applied {
			fmt.Fprintf(out, "  %-40s Source: your organisation (policy v%d)\n", key, gout.Version)
		}
		fmt.Fprintln(out, "  These are the values THIS command's own configuration actually used.")
	}
	for key, why := range gout.Skipped {
		fmt.Fprintf(out, "  %-40s IGNORED by this machine: %s\n", key, why)
	}
	if !gout.WrittenAt.IsZero() {
		fmt.Fprintf(out, "  Last written: %s\n", gout.WrittenAt.Format(time.RFC3339))
	}
}

// pinnedAbsenceReason turns a sidecar read reason into a sentence a
// developer can act on.
func pinnedAbsenceReason(reason string) string {
	switch reason {
	case sidecar.ReasonAbsent:
		return "no settings file has been written on this machine"
	case sidecar.ReasonUnreadable:
		return "the settings file exists but cannot be read - check its permissions"
	case sidecar.ReasonOversize, sidecar.ReasonMalformed:
		return "the settings file is unreadable or damaged and is being ignored - delete it and restart `observer start`"
	case sidecar.ReasonSchemaTooNew:
		return "the settings file was written by a NEWER version of Observer and is being ignored"
	case sidecar.ReasonGrantExpired:
		return "the grant has expired, so nothing is applied"
	case sidecar.ReasonNotApplied:
		return "this machine is not currently governed"
	default:
		return "nothing is pinned"
	}
}
