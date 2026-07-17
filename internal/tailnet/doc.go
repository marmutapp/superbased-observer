// Package tailnet is the ONE owner of the `tailscale` CLI exec Observer uses to
// detect the tailnet HTTPS host and (best-effort) the serve mapping
// (dashboard-management-surface plan §D). It runs a LOCAL binary — never a
// network call — so the CLAUDE.md "no network calls in the capture path"
// invariant is untouched; this is dashboard/lifecycle-layer detection, the same
// posture as the pre-existing CLI tailnet-host detection it replaces.
//
// Both the CLI (`observer remote enable`, via detectTailnetHost) and the
// dashboard (GET /api/remote/tailscale/status) call through here, so there is
// exactly one owner of the exec (CLAUDE.md #4), and neither reimplements it.
// Every result is best-effort: an absent binary / logged-out node / parse
// failure is a first-class state (Present/LoggedIn false), never an error the
// caller must handle.
package tailnet
