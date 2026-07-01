package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/guard"
	"github.com/marmutapp/superbased-observer/internal/guard/notify"
	"github.com/marmutapp/superbased-observer/internal/orgclient"
	"github.com/marmutapp/superbased-observer/internal/proxy"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// Daemon-wide guard sharing (guard spec §8 / G9). `observer start`
// assembles the proxy and the watcher through separate build
// functions, each with its own Store handle over the same SQLite
// file — but the guard layer's TAINT STATE must be daemon-wide: the
// proxy's §8.4 injection heuristics mark Imperative taint that the
// watcher ingest seam's T-501 consumes. processGuards therefore keys
// ONE Guard per (process, observer.db): the first composition site to
// ask constructs it (with its store backing the approval lookup and
// the policy-state log — equivalent through either handle, same DB),
// and every later site receives the same instance. The cachetrack
// single-engine precedent, adapted for the two-store assembly.
//
// Hook processes remain separate Guards by design (short-lived
// subprocesses — documented in internal/guard/hook.go).
var processGuards = struct {
	mu sync.Mutex
	m  map[string]*guard.Guard
}{m: map[string]*guard.Guard{}}

// acquireProcessGuard returns the per-(process, db-path) shared Guard,
// constructing it on first call. Returns nil when the guard is
// disabled/off or construction failed (WARN-and-continue — the
// daemon never refuses to run over a guard problem). Failed
// constructions are not cached, so a later caller retries.
func acquireProcessGuard(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) *guard.Guard {
	if !cfg.Guard.Enabled || cfg.Guard.Mode == "off" {
		return nil
	}
	key := cfg.Observer.DBPath
	processGuards.mu.Lock()
	defer processGuards.mu.Unlock()
	if g, ok := processGuards.m[key]; ok {
		return g
	}
	g := buildGuardForStore(ctx, cfg, st, logger)
	if g != nil {
		processGuards.m[key] = g
	}
	return g
}

// buildGuardForStore constructs the guard composition layer wired to
// one store handle: policy layers via guard.New, the §14.4
// policy-change log callback, and the §6.3 approval lookup. Every
// failure path is WARN-and-continue (the Q2 fail-open philosophy
// applied at composition).
func buildGuardForStore(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) *guard.Guard {
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warn("guard: home dir unavailable; ~-anchored policy paths degrade", "err", err)
	}
	roots, err := st.ProjectRoots(ctx)
	if err != nil {
		logger.Warn("guard: project roots unavailable; cross-project rule R-151 inert this run", "err", err)
	}
	// §14.2 org policy key pin: the daemon path supplies the enrolment
	// pin so the org bundle loader requires the cached envelope's key
	// to match it (hook processes skip the pin read — §6.4 latency;
	// they still verify the envelope's own signature). One indexed
	// read at construction; a read failure degrades to the
	// self-contained check, never blocks the daemon.
	var orgKeyPin string
	if states, serr := st.LatestGuardPolicyStates(ctx); serr == nil {
		for _, ps := range states {
			if ps.Layer == "org" && strings.HasSuffix(ps.Path, orgclient.PolicyKeyPinSuffix) {
				orgKeyPin = ps.ContentHash
			}
		}
	} else {
		logger.Warn("guard: org policy key pin unavailable; bundle loads with self-contained verification only", "err", serr)
	}
	g, err := guard.New(guard.Options{
		Config:            cfg.Guard,
		Home:              home,
		KnownProjectRoots: roots,
		Notifier:          notify.NewDesktop(),
		OrgKeyPinHash:     orgKeyPin,
		OnPolicyState: func(ps guard.PolicyState) {
			// Project layers load lazily mid-run; use a fresh
			// short-lived context rather than the (possibly long-gone)
			// composition ctx.
			recCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, rerr := st.RecordGuardPolicyState(recCtx, store.GuardPolicyStateRow{
				Layer: ps.Layer, Path: ps.Path, Version: ps.Version,
				ContentHash: ps.ContentHash,
				LoadedAt:    time.Now().UTC(),
			}); rerr != nil {
				logger.Warn("guard: policy-state record failed", "layer", ps.Layer, "path", ps.Path, "err", rerr)
			}
		},
	})
	if err != nil {
		logger.Warn("guard: construction failed; daemon runs unguarded", "err", err)
		return nil
	}
	for _, issue := range g.LoadIssues() {
		logger.Warn("guard: policy load issue", "issue", issue)
	}
	// §6.3 approvals: blocking verdicts consult the grant register.
	// The daemon's store handle is long-lived, so the lookup is one
	// indexed read; errors report false (fail-safe to enforcement).
	g.SetApprovalLookup(func(ruleID, sessionID, rootHash string) bool {
		lctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return st.ApprovalActiveFor(lctx, ruleID, sessionID, rootHash, time.Now().UTC())
	})
	// §12.1 budget lookup: spend-so-far for the B-601/B-602 rows and
	// the §4.4 cost matchers. Guard caches per session (30s TTL), so
	// this SUM query runs at most ~2/min/session; errors report
	// ok=false (rules fail toward silence, never a spurious breach).
	// Wired whenever the guard runs — user cost-matcher rules work
	// even with no [guard.budget] thresholds set.
	g.SetBudgetLookup(func(sessionID string) (float64, float64, bool) {
		lctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		dayStart := time.Now().UTC().Truncate(24 * time.Hour)
		sess, daily, err := st.GuardBudgetSpend(lctx, sessionID, dayStart)
		if err != nil {
			logger.Warn("guard: budget spend lookup failed", "err", err)
			return 0, 0, false
		}
		return sess, daily, true
	})
	// §9.2 pin lookup: pinned-and-approved MCP servers stop marking
	// mcp_unpinned taint. One indexed read, only on MCP-result paths;
	// errors report false (fail toward "unpinned", never toward
	// silently trusting a server).
	g.SetMCPPinLookup(func(server string) bool {
		lctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		ok, err := st.GuardMCPServerApproved(lctx, server)
		return err == nil && ok
	})
	// §9.2 config-change re-scan: a watcher-ingested write to a
	// locate-table MCP registry file re-runs the config scan
	// (debounced — editor save bursts coalesce to one pass).
	if r := newMCPSecRunner(configGuardMCP{
		Pinning:             cfg.Guard.MCP.Pinning,
		PoisoningHeuristics: cfg.Guard.MCP.PoisoningHeuristics,
	}, st, g, logger); r != nil {
		g.SetMCPRescan(debouncedMCPRescan(r, 5*time.Second))
	}
	// §13.2 dialect drift re-check: a watcher-ingested write to a
	// compiled native config (settings.json, opencode.json) re-checks
	// drift against policy — the same config-watch trigger the MCP
	// re-scan rides, second consumer (one mechanism, two consumers).
	if dr := newDialectRunner(configGuardDialects{
		Compile: cfg.Guard.Dialects.Compile,
		Targets: cfg.Guard.Dialects.Targets,
	}, st, g, logger); dr != nil {
		g.SetDialectRescan(dr.watchPaths(), debouncedDialectRescan(dr, 5*time.Second))
	}
	return g
}

// guardScannerAdapter bridges the daemon's shared *guard.Guard +
// *store.Store behind proxy.GuardScanner — the pipelineAdapter /
// costEngineAdapter pattern: the proxy package holds only its own
// plain types; persistence and alerting live here, behind the seam.
// mcp, when non-nil, receives the §9.2 MCP tool-declaration
// observations the request scan surfaces.
type guardScannerAdapter struct {
	g      *guard.Guard
	st     *store.Store
	logger *slog.Logger
	mcp    *mcpsecRunner
}

// ScanRequest implements proxy.GuardScanner (§8.2 + §8.4). Verdicts
// persist synchronously through the one-owner store seam before the
// decision returns — rare (only verdict-bearing requests pay it), and
// an enforced deny/mask should be on the audit chain before the
// synthetic response leaves. Persistence failure is log-and-continue
// (§17.4): it never blocks the request decision.
//
// MCP declaration observations (§9.2) run DETACHED: the pin diff
// reads + writes the DB, and the request must not wait on it — the
// declarations describe config state, not this request's content.
func (a guardScannerAdapter) ScanRequest(_ context.Context, provider string, body []byte, sessionID string) proxy.GuardRequestResult {
	res := a.g.ScanProxyRequest(provider, body, sessionID, time.Now().UTC())
	a.persistAndAlert(res.Verdicts)
	if len(res.MCPDecls) > 0 && a.mcp != nil {
		decls := res.MCPDecls
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.mcp.ObserveDecls(ctx, decls)
		}()
	}
	var out proxy.GuardRequestResult
	switch {
	case res.Deny:
		out.Action = "deny"
		out.RuleID = res.DenyRuleID
		out.Reason = res.DenyReason
	case res.MaskedBody != nil:
		out.Action = "mask"
		out.Body = res.MaskedBody
	}
	return out
}

// InspectResponse implements proxy.GuardScanner (§8.3): flag/alert
// only — the proxy already delivered the response. apiTurnID anchors
// each persisted verdict to the api_turn this response was captured as
// (0 when the turn was dropped) so the obs trajectory enrichment can
// surface the verdict on the span.
func (a guardScannerAdapter) InspectResponse(_ context.Context, sessionID string, apiTurnID int64, tools []proxy.GuardToolUse) {
	in := make([]guard.ProxyToolUse, 0, len(tools))
	for _, t := range tools {
		in = append(in, guard.ProxyToolUse{Name: t.Name, Input: t.Input})
	}
	verdicts := a.g.InspectProxyResponse(sessionID, in, time.Now().UTC())
	if apiTurnID != 0 {
		for i := range verdicts {
			verdicts[i].Input.APITurnID = apiTurnID
		}
	}
	a.persistAndAlert(verdicts)
}

// persistAndAlert writes record-worthy verdicts through the one-owner
// guard store seam (detached context — the request context may die
// the moment the client has its bytes) and fires desktop alerts
// post-persist (the MaybeAlert call-site contract).
func (a guardScannerAdapter) persistAndAlert(verdicts []guard.ActionVerdict) {
	if len(verdicts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.st.PersistGuardVerdicts(ctx, verdicts); err != nil {
		a.logger.Warn("guard: proxy verdict persist failed", "err", err)
	}
	for i := range verdicts {
		a.g.MaybeAlert(verdicts[i])
	}
}
