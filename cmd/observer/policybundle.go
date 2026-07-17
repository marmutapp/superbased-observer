package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/policy"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Org policy-bundle poll orchestration (guard spec §14.2, G13). The
// orgclient owns the wire + verification; this runner owns what the
// cmd layer always owns (the mcpsecRunner / dialectRunner pattern):
// turning a REJECTED poll into an R-205 guard event through the real
// engine, persisting via the one-owner store seam, and alerting.

// orgBundleCachePath resolves [guard.rules].org_bundle to an absolute
// path ("" when unset — the channel is then off for this daemon).
func orgBundleCachePath(cfg config.Config) string {
	p := cfg.Guard.Rules.OrgBundle
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	return filepath.FromSlash(p)
}

// policyBundleRunner receives PolicyPollLoop results. Rejections emit
// R-205 once per rejection state — a bad bundle served unchanged for
// hours is ONE event, a different bad bundle (new version or new
// failure mode) is the next (the R-204 once-per-drift-state
// precedent); a daemon restart re-emits once, which is the desired
// "still broken" heartbeat.
type policyBundleRunner struct {
	st      *store.Store
	logger  *slog.Logger
	orgURL  string
	acquire func(context.Context) *guard.Guard

	mu         sync.Mutex
	lastReject string
}

// newPolicyBundleRunner builds the result handler over the daemon's
// shared guard (acquired lazily — only a rejection needs it). orgURL
// is audit metadata for the R-205 finding target.
func newPolicyBundleRunner(cfg config.Config, st *store.Store, logger *slog.Logger, orgURL string) *policyBundleRunner {
	return &policyBundleRunner{
		st: st, logger: logger, orgURL: orgURL,
		acquire: func(ctx context.Context) *guard.Guard {
			return acquireProcessGuard(ctx, cfg, st, logger)
		},
	}
}

// onResult is the PolicyPollLoop callback.
func (r *policyBundleRunner) onResult(res orgclient.PolicyResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if res.Status != orgclient.PolicyRejected {
		// Any healthy outcome re-arms the dedup so a LATER rejection
		// emits again even if it textually matches an old one.
		r.lastReject = ""
		return
	}
	key := fmt.Sprintf("%d|%s", res.Version, res.Detail)
	if key == r.lastReject {
		return
	}
	r.lastReject = key

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gd := r.acquire(ctx)
	if gd == nil {
		// Guard off mid-flight (config raced) — the rejection still
		// protected the cache; it is just not auditable as an event.
		r.logger.Warn("org policy: bundle rejected (guard unavailable, no R-205 recorded)",
			"version", res.Version, "detail", res.Detail)
		return
	}
	// The finding carries the channel subject: Client "org" (the org
	// server, not an AI client) and the bundle endpoint as target. The
	// bundle URL travels INSIDE the finding per the R-204 pinned
	// discovery — never as Event.Target.
	verdicts := gd.EvaluatePostureFindings([]policy.PostureFinding{{
		Kind:   policy.PostureFindingBundleSignature,
		Client: "org",
		Target: r.orgURL + "/api/v1/policy-bundle",
		Detail: res.Detail,
	}}, time.Now().UTC())
	if len(verdicts) == 0 {
		return // R-205 disabled via [guard.rules].disable — operator's call
	}
	if _, err := r.st.PersistGuardVerdicts(ctx, verdicts); err != nil {
		r.logger.Warn("org policy: R-205 persist failed", "err", err)
	}
	for i := range verdicts {
		gd.MaybeAlert(verdicts[i])
	}
	r.logger.Warn("org policy: bundle REJECTED — running on previous policy",
		"version", res.Version, "detail", res.Detail, "rule", "R-205")
}
