package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/govern"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Admin-controlled Plane B Phase 1b, the daemon-side loops and seams
// (docs/plans/admin-controlled-plane-b-phase-1b-mini-spec-2026-08-15.md
// §1.4, §2.4, §4).
//
// Three things live here, all of them daemon-only and all of them no-ops on
// a node that is not enrolled:
//
//  1. the sidecar refresh tick (the ONE writer),
//  2. the hot share-posture provider (lowering-only),
//  3. the grant-renewal tick, including §4.3's idle-node probe.

// grantRenewalTickInterval is how often the renewal loop wakes. It is much
// shorter than the write rate limit below: waking often is free, writing to
// a table that is READ every 15 s is not.
const grantRenewalTickInterval = 15 * time.Minute

// grantRenewalMinWriteGap / grantRenewalMinMove are the §4.4 rate limit: at
// most one write per hour, and only when the new expiry would move the clock
// by more than an hour.
const (
	grantRenewalMinWriteGap = time.Hour
	grantRenewalMinMove     = time.Hour
)

// renewalProbeInterval bounds §4.3's explicit authorization probe. It fires
// ONLY while the authDenied latch is set, so a healthy idle node is
// completely silent and an idle fleet never becomes chatty.
const renewalProbeInterval = time.Hour

// runGovernanceSidecarWriter is the ONE writer of the governance sidecar
// (CLAUDE.md #4). It rewrites the file whenever the resolved posture's hash
// changes — which includes the grant lapsing, an unenrol landing from
// another process, and a new body being accepted — and refreshes an
// unchanged file every sidecarRefreshInterval so written_at stays a
// meaningful liveness signal.
//
// It never returns an error: a write failure is surfaced as an INERT
// condition on the node's own effective-state report (§1.4.1), which is a
// louder and more honest signal than a daemon that dies over a policy file.
// Logging happens inside the handle's own WriteSidecar (it owns the
// write-failure warning), so this loop takes no logger of its own.
func runGovernanceSidecarWriter(ctx context.Context, ngov *nodeGovernanceHandle) {
	if ngov == nil || ngov.SidecarPath() == "" {
		return
	}
	// Write once immediately so a daemon that starts against an existing
	// grant does not leave a stale (or absent) file for a whole interval.
	ngov.WriteSidecar(ctx)
	t := time.NewTicker(governanceGrantRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ngov.WriteSidecar(ctx)
		}
	}
}

// governanceShareProvider resolves the share posture each push ships under:
// the node's OWN [org_client.share] block, LOWERED by whatever the
// organization has directed (§2.1/§2.4).
//
// The direction is the whole point. For a raise, restart-bound would have
// been acceptable; for a lowering it is not — a node that keeps shipping
// content for hours after the org said stop is exactly the failure the
// directive exists to prevent. So this is called on every push, reading the
// same handle the dashboard guard reads.
//
// One owner (store.ShareOptions), two feed paths (TOML, governance).
func governanceShareProvider(cfg config.OrgClientConfig, ngov *nodeGovernanceHandle) func() store.ShareOptions {
	base := orgclient.ShareOptionsFromConfig(cfg)
	if ngov == nil {
		return func() store.ShareOptions { return base }
	}
	return func() store.ShareOptions {
		eff := ngov.Effective(context.Background())
		return lowerShareOptions(base, eff)
	}
}

// lowerShareOptions applies the lowering merge, one row per share key.
//
// Every row is `local AND org` (or an intersection for the list), so the
// result can only ever share LESS than the node's own config. AdminManaged
// is deliberately absent: it is excluded from the org vocabulary entirely,
// so nothing here can touch it, and shipsRawContent() can therefore only
// move true → false under any org body, grant, or signing key.
func lowerShareOptions(local store.ShareOptions, eff govern.Effective) store.ShareOptions {
	out := local
	out.FullContent = eff.LowerBool("full_content", local.FullContent)
	out.RoutingSummary = eff.LowerBool("routing_summary", local.RoutingSummary)
	out.ObsSummary = eff.LowerBool("obs.summary", local.ObsSummary)
	out.ObsTraces = eff.LowerBool("obs.traces", local.ObsTraces)
	out.ObsContent = eff.LowerBool("obs.content", local.ObsContent)
	out.ObsEvalSummary = eff.LowerBool("obs.eval_summary", local.ObsEvalSummary)
	out.ObsAdmission = eff.LowerBool("obs.admission", local.ObsAdmission)
	out.ObsEvalItems = eff.LowerBool("obs.eval_items", local.ObsEvalItems)
	out.TargetActionAllowlist = eff.LowerList("target_action_allowlist", local.TargetActionAllowlist)
	return out
}

// grantRenewer owns the renewal clock for one daemon run.
type grantRenewer struct {
	client  *orgclient.Client
	store   *store.Store
	tracker *orgclient.RenewalTracker
	logger  *slog.Logger
	now     func() time.Time

	lastWrite time.Time
}

// runGrantRenewal is the §4 renewal tick.
//
// Renewal is a LOCAL write, and the TTL is derived from the grant itself:
//
//	ttl       := signed_expires_at - granted_at
//	newExpiry := now + ttl
//
// so a renewal can never extend the grant beyond the window the organization
// actually signed for, and no ttl_days field is added anywhere.
func runGrantRenewal(ctx context.Context, oc *orgclient.Client, st *store.Store, tracker *orgclient.RenewalTracker, logger *slog.Logger) {
	if oc == nil || st == nil || tracker == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	r := &grantRenewer{client: oc, store: st, tracker: tracker, logger: logger, now: time.Now}
	t := time.NewTicker(grantRenewalTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *grantRenewer) tick(ctx context.Context) {
	now := r.now().UTC()
	if r.tracker.Latched() {
		// While latched, NO renewal is written no matter how many 2xx or 304
		// responses arrive from other paths (R5). The latch clears only on an
		// authorized response on the PUSH path, and an idle node produces
		// none — hence the explicit probe (review M2).
		if r.tracker.ShouldProbe(now, renewalProbeInterval) {
			if err := r.client.ProbePushAuthorization(ctx); err != nil {
				r.logger.Debug("governance: renewal probe failed", "err", err)
			}
		}
		return
	}
	if !r.lastWrite.IsZero() && now.Sub(r.lastWrite) < grantRenewalMinWriteGap {
		return
	}
	enr, err := r.store.LoadEnrolment(ctx)
	if err != nil || enr == nil {
		return
	}
	orgKey := orgclient.OrgKey(enr.OrgServerURL, enr.OrgID)
	grant, ok, err := r.store.LoadEnrolmentGrant(ctx, orgKey)
	if err != nil || !ok {
		return
	}
	newExpiry, ok := renewedExpiry(grant, now)
	if !ok {
		return
	}
	if err := r.store.RenewEnrolmentGrant(ctx, orgKey, grant.Generation, newExpiry); err != nil {
		r.logger.Warn("governance: could not renew the enrolment grant", "err", err)
		return
	}
	r.lastWrite = now
}

// renewedExpiry computes the renewed working expiry, or reports that this
// tick must not renew.
//
// The guards are review M1's: a non-positive TTL, or one derived from a zero
// GrantedAt, SKIPS renewal rather than computing nonsense. Without them a
// grant written before migration 083's backfill (or by a store write that
// forgot the column) would yield a large NEGATIVE duration and an expiry in
// the past.
func renewedExpiry(grant store.EnrolmentGrant, now time.Time) (time.Time, bool) {
	if grant.GrantedAt.IsZero() || grant.SignedExpiresAt.IsZero() {
		return time.Time{}, false
	}
	ttl := grant.SignedExpiresAt.Sub(grant.GrantedAt)
	if ttl <= 0 {
		return time.Time{}, false
	}
	newExpiry := now.Add(ttl)
	// Only write when the clock actually moves. A DB write per poll cycle is
	// needless churn on a table that is read every 15 s.
	if newExpiry.Sub(grant.ExpiresAt) <= grantRenewalMinMove {
		return time.Time{}, false
	}
	return newExpiry, true
}
