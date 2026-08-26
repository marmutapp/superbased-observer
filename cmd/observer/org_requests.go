package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmutapp/superbased-observer/internal/orgclient"
)

// org_requests.go — W6 dev->org REQUEST/MESSAGE system, CLI half (see
// docs/plans/org-parity-full-depth-plan-2026-08-24.md §0.4 + §4 "W6",
// internal/orgclient/requests.go for the client methods this wraps). This is
// the node operator's only ask channel to the org: `observer org request`
// posts a short typed ask, `observer org requests` reads back this node's
// own asks and their statuses. Neither command grants anything locally —
// the admin always responds by editing policy on the existing policy rail;
// these commands only carry the ask and show its recorded status.

// newOrgRequestCmd posts a request/message to the enrolled organisation
// (POST /api/agent/requests via orgclient.PostOrgRequest).
func newOrgRequestCmd() *cobra.Command {
	var (
		configPath string
		kind       string
		target     string
	)
	cmd := &cobra.Command{
		Use:   "request <message>",
		Short: "Send a short typed ask to your organisation (e.g. enable a feature, raise a budget)",
		Long: `Posts a short request/message to the enrolled organisation. This is the
only ask channel a node has -- it does not change anything locally. The
admin sees the ask in their Requests queue and responds by editing policy;
that change reaches this node on the normal policy rail, not through this
command.

--kind is a closed vocabulary: enable_feature, raise_budget, allow_tool, or
other (the default when omitted). --target names the specific feature,
budget, or tool the ask concerns (optional, e.g. "terminals" or "bash").

Examples:
  observer org request "please enable terminals for my node" --kind enable_feature --target terminals
  observer org request "our monthly budget is too low for this project" --kind raise_budget
  observer org request "need bash access for CI debugging" --kind allow_tool --target bash`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			message := strings.TrimSpace(args[0])
			if message == "" {
				return errors.New("observer org request: message must not be empty")
			}
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()

			got, err := b.client.PostOrgRequest(cmd.Context(), kind, target, message)
			if errors.Is(err, orgclient.ErrNotEnrolled) {
				return errors.New("not enrolled; run `observer enroll <org-url> <token>` first")
			}
			if errors.Is(err, orgclient.ErrOrgRequestCapReached) {
				return errors.New("you already have too many open requests to this organisation; wait for one to be resolved, or run `observer org requests` to review them")
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Request sent (id %d, kind %s", got.ID, orgOrDefault(got.Kind, "other"))
			if got.Target != "" {
				fmt.Fprintf(out, ", target %s", got.Target)
			}
			fmt.Fprintln(out, ").")
			fmt.Fprintln(out, "Your admin will respond by updating organisation policy; run `observer org requests` to check its status.")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	cmd.Flags().StringVar(&kind, "kind", "other", "request kind: enable_feature | raise_budget | allow_tool | other")
	cmd.Flags().StringVar(&target, "target", "", "the specific feature/budget/tool this ask concerns (optional)")
	return cmd
}

// newOrgRequestsCmd lists this node's own requests and their current
// statuses (GET /api/agent/requests via orgclient.ListMyOrgRequests). The
// server scopes the list to the enrolled identity -- there is no way to see
// anyone else's requests from here.
func newOrgRequestsCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "List the requests you've sent to your organisation and their status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := buildOrgBundle(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer b.cleanup()

			reqs, err := b.client.ListMyOrgRequests(cmd.Context())
			if errors.Is(err, orgclient.ErrNotEnrolled) {
				return errors.New("not enrolled; run `observer enroll <org-url> <token>` first")
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(reqs) == 0 {
				fmt.Fprintln(out, "(no requests sent yet — use `observer org request \"<message>\"` to send one)")
				return nil
			}
			for _, r := range reqs {
				fmt.Fprintf(out, "#%d  %-16s  %-8s  %s\n", r.ID, orgOrDefault(r.Kind, "other"), r.Status, r.CreatedAt)
				if r.Target != "" {
					fmt.Fprintf(out, "     target: %s\n", r.Target)
				}
				fmt.Fprintf(out, "     %s\n", r.Message)
				if r.Status != "open" {
					fmt.Fprintf(out, "     resolved by %s at %s", orgOrDefault(r.ResolvedBy, "(unknown)"), r.ResolvedAt)
					if r.ResolutionNote != "" {
						fmt.Fprintf(out, ": %s", r.ResolutionNote)
					}
					fmt.Fprintln(out)
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml (defaults to ~/.observer/config.toml)")
	return cmd
}

// orgOrDefault returns s unless it's empty, in which case it returns def.
func orgOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
