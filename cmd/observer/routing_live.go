package main

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmutapp/superbased-observer/internal/proxy"
	"github.com/marmutapp/superbased-observer/internal/routing"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// liveRouter implements proxy.ModelRouter at the cmd boundary — the
// single place the routing engine, the store-side snapshot refresher,
// and the decision-log store seam meet (§R5). It owns the live
// session-coherence state (the Simulate sessState analog) and the
// pending-decision buffer that links decisions to api_turns rows.
//
// Hot-path contract (§R9.2): Decide reads ONLY the refresher's
// in-memory snapshot plus two map lookups under a short mutex; the
// decision-row insert happens in RecordServed on the proxy's detached
// goroutine (or via the janitor for turns that never land).
type liveRouter struct {
	policy    routing.Policy
	mode      string // advise | enforce
	refresher *store.RoutingRefresher
	store     *store.Store
	logger    *slog.Logger
	now       func() time.Time

	mu       sync.Mutex
	sessions map[string]*liveSessionState
	pending  map[int64]pendingDecision
	nextTok  int64

	// demoted holds the calibration job's §R18.3 rule demotions (rule
	// name → reason). A demoted rule's decisions log but never apply.
	// In-memory by design: a daemon restart clears it and the next
	// calibration pass re-detects within the hour — self-healing, no
	// new table.
	demoted atomic.Pointer[map[string]string]
}

// SetDemotedRules swaps the demotion set (calibration job, §R18.3).
func (lr *liveRouter) SetDemotedRules(m map[string]string) {
	lr.demoted.Store(&m)
}

// DemotedRules returns a copy of the current §R18.3 demotion set
// (rule name → reason) — the read-side surface for the dashboard's
// /api/routing/status (R2.4). A copy, so callers can never mutate the
// set the hot path reads. Empty-non-nil when nothing is demoted.
func (lr *liveRouter) DemotedRules() map[string]string {
	m := lr.demoted.Load()
	if m == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(*m))
	for k, v := range *m {
		out[k] = v
	}
	return out
}

// liveSessionState is the per-session coherence memory (§R13) plus the
// §R7.4 escalation tracker.
type liveSessionState struct {
	turnsSinceSwitch int // -1 = never switched
	lastSeen         time.Time
	// lastDownshiftAt stamps the most recent APPLIED downshift per
	// turn-kind — the escalation detector's arming state.
	lastDownshiftAt map[routing.TurnKind]time.Time
	// escalatedUntil holds active §R7.4 cooldowns per turn-kind.
	escalatedUntil map[routing.TurnKind]time.Time
}

// Escalation tuning (§R7.4). The detector arms when a downshift was
// applied within the window; a failure spike in the session's recent
// actions then escalates the kind for the cooldown. Detection v1 is
// the tool-error-spike signal; immediate-retry and user-override
// detectors are documented extensions.
const (
	escalationDetectWindow = 10 * time.Minute
	escalationCooldown     = 15 * time.Minute
	escalationFailures     = 2 // failures among the last N window actions
)

type pendingDecision struct {
	row     store.RouterDecisionRow
	created time.Time
}

// pendingJanitorAge is how long a decision waits for its turn linkage
// before flushing with a NULL api_turn_id (the turn was dropped — a
// zero-usage stream, an early return).
const pendingJanitorAge = 60 * time.Second

// liveSessionCap bounds the coherence map on long-running daemons.
const liveSessionCap = 1024

func newLiveRouter(policy routing.Policy, mode string, refresher *store.RoutingRefresher, s *store.Store, logger *slog.Logger) *liveRouter {
	return &liveRouter{
		policy:    policy,
		mode:      mode,
		refresher: refresher,
		store:     s,
		logger:    logger,
		now:       time.Now,
		sessions:  map[string]*liveSessionState{},
		pending:   map[int64]pendingDecision{},
	}
}

// Decide implements proxy.ModelRouter (§R9.2).
func (lr *liveRouter) Decide(shape proxy.RouterShape, sess proxy.RouterSession) proxy.RouterVerdict {
	snap := lr.refresher.Current()
	in := routing.DecisionInput{
		Shape: routing.TurnShape{
			Model:            shape.Model,
			MessageCount:     shape.MessageCount,
			ToolUseCount:     shape.ToolUseCount,
			SystemPromptHash: shape.SystemPromptHash,
			Stream:           shape.Stream,
			Speed:            shape.Speed,
			PromptTokens:     shape.PromptTokensEstimate,
		},
		Entitlement: sess.Entitlement,
	}
	if snap != nil {
		if act, ok := snap.Sessions[sess.SessionID]; ok {
			in.Session.RecentActions = act.RecentActions
			in.Session.ActionsLagged = act.ActionsLagged
			in.Session.ClientPhase = act.ClientPhase
			in.Session.IsSidechain = act.LastSidechain
			in.Session.SessionAgeTurns = act.TurnCount
			in.Project = act.Project
			in.ScopeKeys = act.ScopeKeys
			in.PathClassHits = act.PathClassHits
			in.PathClassHitsHash = act.PathClassHitsHash
		}
		in.Session.PriorCacheReadTokens = snap.SessionCacheRead[sess.SessionID]
	}

	// The escalation tracker is keyed by turn-kind, so classify here
	// (the same pure classifier the engine runs; both see identical
	// inputs, so the kinds agree).
	kind := routing.ClassifyTurnKind(in).Kind

	lr.mu.Lock()
	state := lr.sessionStateLocked(sess.SessionID)
	in.Session.TurnsSinceSwitch = state.turnsSinceSwitch
	// §R7.4 detection: a recent applied downshift of this kind + a
	// failure spike in the action window arms the cooldown.
	if armedAt, armed := state.lastDownshiftAt[kind]; armed &&
		lr.now().Sub(armedAt) <= escalationDetectWindow &&
		failureSpike(in.Session.RecentActions) {
		state.escalatedUntil[kind] = lr.now().Add(escalationCooldown)
		delete(state.lastDownshiftAt, kind)
		lr.logger.Info("routing: escalation armed — downshifted turn-kind failing",
			"session", sess.SessionID, "kind", string(kind))
	}
	if until, active := state.escalatedUntil[kind]; active {
		if lr.now().Before(until) {
			in.Session.EscalatedKinds = append(in.Session.EscalatedKinds, kind)
		} else {
			delete(state.escalatedUntil, kind)
		}
	}
	lr.mu.Unlock()

	d := routing.Decide(lr.policy, snap, in)

	// Effort-only decisions (§R6.5) apply without a model change —
	// the lowest-risk enforce action (zero cache loss).
	apply := lr.mode == "enforce" && !d.AdviseOnly && (d.Changed || d.SetEffort != "")

	// Calibration demotion (§R18.3): a rule the calibration job graded
	// as regressing logs its decision but never acts.
	if apply && d.RuleName != "" {
		if m := lr.demoted.Load(); m != nil {
			if _, isDemoted := (*m)[d.RuleName]; isDemoted {
				apply = false
				d.ReasonCodes = append(d.ReasonCodes, routing.ReasonCalibrationDemoted)
			}
		}
	}

	lr.mu.Lock()
	// Coherence counts MODEL switches only — an applied effort-only
	// decision leaves the session's model (and its cache) untouched.
	if apply && d.Changed {
		state.turnsSinceSwitch = 0
		// Arm the §R7.4 detector: this kind just downshifted.
		state.lastDownshiftAt[d.TurnKind] = lr.now()
	} else if state.turnsSinceSwitch >= 0 {
		state.turnsSinceSwitch++
	}
	lr.nextTok++
	token := lr.nextTok
	lr.pending[token] = pendingDecision{
		row:     decisionRow(d, lr.mode, sess.SessionID, apply, lr.now()),
		created: lr.now(),
	}
	lr.mu.Unlock()

	v := proxy.RouterVerdict{
		SelectedModel: d.SelectedModel,
		Token:         token,
	}
	if apply {
		v.Apply = true
		v.SetEffort = d.SetEffort
	}
	if lr.mode == "enforce" {
		// Fallback chains (§R12.1) are an enforce-class action — a
		// fallback rewrites the model. Advise mode returns no chain.
		v.FallbackModels = d.FallbackModels
	}
	return v
}

// RecordServed implements proxy.ModelRouter: called on the proxy's
// detached insert goroutine once the turn lands. Inserts the linked
// decision row plus any janitor-expired orphans. A served model
// differing from the decision's selection means the §R12.1 fallback
// chain fired after the decision — the row is annotated with
// availability_fallback and records what actually served.
func (lr *liveRouter) RecordServed(token, apiTurnID int64, servedModel string) {
	now := lr.now()
	lr.mu.Lock()
	rows := lr.collectExpiredLocked(now)
	if pend, ok := lr.pending[token]; ok {
		delete(lr.pending, token)
		if apiTurnID > 0 {
			pend.row.APITurnID = &apiTurnID
		}
		// What we EXPECTED to serve: the selection when applied, the
		// original otherwise (advise mode / holds). Response model ids
		// often carry a -YYYYMMDD suffix the request id lacks —
		// normalized before comparing so a dated echo never
		// false-fires a fallback annotation.
		expected := pend.row.OriginalModel
		if pend.row.Applied {
			expected = pend.row.SelectedModel
		}
		if servedModel != "" && !sameModelID(servedModel, expected) {
			pend.row.SelectedModel = servedModel
			pend.row.Applied = true
			pend.row.ReasonCodes = append(pend.row.ReasonCodes, string(routing.ReasonAvailabilityFallback))
		}
		rows = append(rows, pend.row)
	}
	lr.mu.Unlock()
	lr.insertRows(rows)
}

// sessionStateLocked returns (creating if needed) the session's
// coherence state and prunes the map past its cap.
func (lr *liveRouter) sessionStateLocked(sid string) *liveSessionState {
	st, ok := lr.sessions[sid]
	if !ok {
		if len(lr.sessions) >= liveSessionCap {
			cutoff := lr.now().Add(-time.Hour)
			for k, v := range lr.sessions {
				if v.lastSeen.Before(cutoff) {
					delete(lr.sessions, k)
				}
			}
		}
		st = &liveSessionState{
			turnsSinceSwitch: -1,
			lastDownshiftAt:  map[routing.TurnKind]time.Time{},
			escalatedUntil:   map[routing.TurnKind]time.Time{},
		}
		lr.sessions[sid] = st
	}
	st.lastSeen = lr.now()
	return st
}

// failureSpike reports a §R7.4 failure signal: at least
// escalationFailures failed actions among the window's last five.
func failureSpike(window []routing.ActionSignal) bool {
	start := len(window) - 5
	if start < 0 {
		start = 0
	}
	failures := 0
	for _, a := range window[start:] {
		if !a.Success {
			failures++
		}
	}
	return failures >= escalationFailures
}

// collectExpiredLocked drains pendings past the janitor age — their
// turns never landed; the decision row persists with a NULL turn id.
func (lr *liveRouter) collectExpiredLocked(now time.Time) []store.RouterDecisionRow {
	var out []store.RouterDecisionRow
	for tok, pend := range lr.pending {
		if now.Sub(pend.created) > pendingJanitorAge {
			out = append(out, pend.row)
			delete(lr.pending, tok)
		}
	}
	return out
}

func (lr *liveRouter) insertRows(rows []store.RouterDecisionRow) {
	if len(rows) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := lr.store.InsertRouterDecisions(ctx, rows); err != nil {
		lr.logger.Warn("routing: decision row insert failed", "err", err)
	}
}

// decisionRow converts a routing.Decision into the store seam's
// SQL-shaped row — the §24.2 boundary conversion (routing types never
// cross the store seam).
func decisionRow(d routing.Decision, mode, sessionID string, applied bool, ts time.Time) store.RouterDecisionRow {
	codes := make([]string, len(d.ReasonCodes))
	for i, rc := range d.ReasonCodes {
		codes[i] = string(rc)
	}
	return store.RouterDecisionRow{
		SessionID:       sessionID,
		Timestamp:       ts,
		Mode:            mode,
		Channel:         string(routing.ChannelProxy),
		OriginalModel:   d.OriginalModel,
		SelectedModel:   d.SelectedModel,
		TurnKind:        string(d.TurnKind),
		PolicyName:      d.PolicyName,
		PolicyHash:      d.PolicyHash,
		ReasonCodes:     codes,
		EstSavingsUSD:   d.EstSavingsUSD,
		CacheForfeitUSD: d.CacheForfeitUSD,
		EstimateVersion: d.EstimateVersion,
		Applied:         applied,
	}
}

// sameModelID compares model ids ignoring case and a trailing
// -YYYYMMDD date suffix (responses echo dated SKUs for undated
// request aliases).
func sameModelID(a, b string) bool {
	return normalizeModelID(a) == normalizeModelID(b)
}

func normalizeModelID(m string) string {
	m = strings.ToLower(m)
	if len(m) > 9 && m[len(m)-9] == '-' {
		allDigits := true
		for _, c := range m[len(m)-8:] {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return m[:len(m)-9]
		}
	}
	return m
}
