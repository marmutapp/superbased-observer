package processobs

import (
	"sort"
	"time"
)

// Late-seed re-resolution (docs/process-observability.md §9.2.8).
//
// THE GAP THIS CLOSES. Attribution is resolved ONCE, at exec
// (Attributor.resolveAttribution); metrics() deliberately never re-resolves.
// That is correct for the two seed producers that exist BEFORE the process
// does — the Claude Code SessionStart hook's ancestor walk, and the env-token
// (§5.5 P-B6) path — but it is a race the OTHER producer usually loses. The
// dashboard terminal seeder (cmd/observer/terminal_pidseed.go) can only write
// a session_pid_bridge row once run→session correlation NAMES the session,
// and that lands well after exec:
//
//	tool           source      n   avg_lag_s  min_lag_s
//	claude-code    oob        55       1.9        0.0
//	codex          oob         4       0.2        0.0
//	codex          discovered  3      30.6       30.4
//	kilo-code-cli  discovered  1     266.6      266.6
//
// At out-of-band lag (0.2–1.9s) the seed usually beats the ~2000ms process
// poll and exec-time resolution works. At `discovered` lag (30–266s) it ALWAYS
// loses: the process was resolved as unattributed minutes before the row
// existed, and nothing ever looked again. Those are exactly the non-claude-code
// tools whose resource charts are blank.
//
// THE PASS. ReconcileLateSeeds re-probes the SeedLookup for the runs that are
// still in the live tree and still below the seed's confidence, upgrades the
// ones that now have a seed, and re-inherits down their subtrees — which is
// where the payoff is, since the seeded pid is a launcher whose children are
// the tool. It is additive: exec-time resolution, cross-OS correlation and the
// metric ring are untouched, and a run that never gets a seed is never written.

// Late-seed defaults. Interval is deliberately tied to
// DefaultMetricPersistInterval: an upgraded run's very next persisted row then
// already carries the session, so the dashboard's resource chart fills on the
// first write after the seed lands rather than one full sweep later.
const (
	// DefaultLateSeedInterval is the cadence of the re-resolution pass.
	DefaultLateSeedInterval = 15 * time.Second
	// DefaultLateSeedMaxAge bounds candidacy by process age. The worst observed
	// correlation lag is 266s; 15 minutes is ~3× that with room to spare, while
	// still keeping a long-lived idle box from re-probing its whole process
	// table forever (and keeping a recycled pid from picking up a stale seed).
	DefaultLateSeedMaxAge = 15 * time.Minute
	// DefaultLateSeedMaxLookups hard-caps SeedLookup calls per pass so a fork
	// storm can never turn into an unbounded query burst on the Run goroutine.
	DefaultLateSeedMaxLookups = 512
)

// LateSeedPolicy governs the late-seed re-resolution pass: how often it runs,
// how old a process may be to still be a candidate, and how many SeedLookup
// calls one pass may make.
//
// The zero value means "all defaults" (the "0 inherits the default" contract
// the other process intervals use). A NEGATIVE Interval disables the pass
// entirely — the Attributor then behaves exactly as it did before, so existing
// callers are unaffected (CLAUDE.md rule 6).
type LateSeedPolicy struct {
	// Interval is the pass cadence. 0 = DefaultLateSeedInterval; < 0 = off.
	Interval time.Duration
	// MaxAge is the maximum age (now - StartedAt) of a candidate run.
	// <= 0 = DefaultLateSeedMaxAge.
	MaxAge time.Duration
	// MaxLookups caps SeedLookup calls per pass. <= 0 =
	// DefaultLateSeedMaxLookups.
	MaxLookups int
}

// DefaultLateSeedPolicy returns the shipped late-seed policy.
func DefaultLateSeedPolicy() LateSeedPolicy {
	return LateSeedPolicy{
		Interval:   DefaultLateSeedInterval,
		MaxAge:     DefaultLateSeedMaxAge,
		MaxLookups: DefaultLateSeedMaxLookups,
	}
}

// Enabled reports whether the pass should run at all.
func (p LateSeedPolicy) Enabled() bool { return p.Interval >= 0 }

// withDefaults fills unset (0 / <= 0) fields from DefaultLateSeedPolicy,
// preserving a negative Interval (which means "disabled", not "unset").
func (p LateSeedPolicy) withDefaults() LateSeedPolicy {
	d := DefaultLateSeedPolicy()
	if p.Interval == 0 {
		p.Interval = d.Interval
	}
	if p.MaxAge <= 0 {
		p.MaxAge = d.MaxAge
	}
	if p.MaxLookups <= 0 {
		p.MaxLookups = d.MaxLookups
	}
	return p
}

// LateSeedResult reports what one re-resolution pass did. Examined vs Probed
// is the work bound made observable: Examined is the whole live tree, Probed is
// how many SeedLookup calls it actually cost.
type LateSeedResult struct {
	// Examined is how many live runs the pass looked at (the whole tracked
	// tree — never the persisted process_runs table).
	Examined int
	// Candidates is how many passed the decision table (lateSeedRules).
	Candidates int
	// Probed is how many SeedLookup calls were made (<= MaxLookups).
	Probed int
	// Truncated reports that MaxLookups clipped the candidate list.
	Truncated bool
	// Roots is how many runs were upgraded by a direct seed hit.
	Roots int
	// Reinherited is how many descendants were re-walked and upgraded.
	Reinherited int
	// Upgraded holds every run whose attribution actually changed (roots first,
	// then their re-inherited descendants) — exactly the set the caller must
	// persist. Empty on a no-op pass, which is the steady state.
	Upgraded []*ProcessRun
}

// confidenceRank ranks Confidence for MAX-upgrade comparisons. It is the ONE
// place the precedence order lives, derived from the two existing rules that
// already encode it:
//
//   - Attributor.resolveAttribution treats a direct identity (env-token, then
//     pid seed) as ConfHigh and lets it override whatever was inherited.
//   - CorrelateCrossOS and its store seam both refuse to touch a run at
//     ConfHigh ("authoritative — never re-anchored"; the SQL guard is
//     `attribution_confidence != 'high'`).
//
// So: high is authoritative and is never downgraded or re-homed, and anything
// strictly below it may be upgraded by a stronger signal. An unknown/empty
// confidence ranks 0 (an unattributed run).
var confidenceRank = map[Confidence]int{
	ConfNone:   0,
	ConfLow:    1,
	ConfMedium: 2,
	ConfHigh:   3,
}

// rankOf returns the precedence rank of a confidence value; unknown ranks 0.
func rankOf(c Confidence) int { return confidenceRank[c] }

// outranks reports whether candidate is a STRICT upgrade over current. Strict
// (not >=) is load-bearing twice over: it is what makes an already-seeded run a
// no-op on the second pass (idempotence), and what stops a late seed from
// re-writing an equally-confident attribution (no churn in attribution_source).
func outranks(candidate, current Confidence) bool { return rankOf(candidate) > rankOf(current) }

// lateSeedVerdict is the outcome of the candidate decision table. The skip
// verdicts are named (not booleans) so a row's reason is legible in tests and
// in the doc.
type lateSeedVerdict string

const (
	lateSeedProbe             lateSeedVerdict = "probe"
	lateSeedSkipExited        lateSeedVerdict = "skip_exited"
	lateSeedSkipNoPID         lateSeedVerdict = "skip_no_pid"
	lateSeedSkipAuthoritative lateSeedVerdict = "skip_authoritative"
	lateSeedSkipStale         lateSeedVerdict = "skip_stale"
)

// lateSeedCandidate is the input a decision-table row is evaluated against.
type lateSeedCandidate struct {
	Run    *ProcessRun
	Now    time.Time
	Policy LateSeedPolicy
	// SeedConfidence is the confidence a direct pid seed would carry. Only a
	// run BELOW it can be a candidate — a run at or above it already holds an
	// attribution at least as trustworthy.
	SeedConfidence Confidence
}

// lateSeedRule is one row of the ordered candidate decision table.
type lateSeedRule struct {
	Verdict lateSeedVerdict
	// Why documents the row; it is the reason string a test asserts on.
	Why  string
	When func(lateSeedCandidate) bool
}

// lateSeedRules is the ordered decision table walked top-down, first match
// wins (CLAUDE.md rule 5 — a data table, not an if/else ladder). The final row
// is the unconditional default.
//
// NOTE on boundaries: an init/systemd/WSL-relay run is NOT skipped here, on
// purpose. resolveAttribution sets IsBoundary and then falls through to the
// direct-identity checks, which re-attribute and clear the boundary; the late
// path mirrors that exactly so the two can never diverge. Inheritance still
// stops at a boundary — that rule lives in the subtree walk, where inherit()
// enforces it (§9.2.6).
var lateSeedRules = []lateSeedRule{
	{
		Verdict: lateSeedSkipExited,
		Why:     "the process is finished; its row is final and its pid is free to be recycled",
		When:    func(c lateSeedCandidate) bool { return c.Run.Exited },
	},
	{
		Verdict: lateSeedSkipNoPID,
		Why:     "no OS pid to look up (a synthesized action-correlation run, §9.2.4)",
		When:    func(c lateSeedCandidate) bool { return c.Run.PID <= 0 },
	},
	{
		Verdict: lateSeedSkipAuthoritative,
		Why:     "already attributed at or above the seed's confidence — never downgrade or clobber",
		When: func(c lateSeedCandidate) bool {
			return !outranks(c.SeedConfidence, c.Run.Attribution.Confidence)
		},
	},
	{
		Verdict: lateSeedSkipStale,
		Why:     "older than MaxAge — past any plausible correlation lag, and a pid-reuse hazard",
		When: func(c lateSeedCandidate) bool {
			if c.Policy.MaxAge <= 0 || c.Run.StartedAt.IsZero() || c.Now.IsZero() {
				return false
			}
			return c.Now.Sub(c.Run.StartedAt) > c.Policy.MaxAge
		},
	},
	{
		Verdict: lateSeedProbe,
		Why:     "live, recent and below the seed's confidence — worth one SeedLookup",
		When:    func(lateSeedCandidate) bool { return true },
	},
}

// classifyLateSeed walks lateSeedRules top-down and returns the first match.
func classifyLateSeed(c lateSeedCandidate) lateSeedVerdict {
	for _, rule := range lateSeedRules {
		if rule.When(c) {
			return rule.Verdict
		}
	}
	return lateSeedProbe
}

// ReconcileLateSeeds re-resolves attribution for live runs whose pid seed
// arrived AFTER they were observed at exec, and re-inherits the upgrade down
// their subtrees. It is the deferred counterpart of resolveAttribution's
// pid-seed rule (§9.2.1) and the in-memory sibling of the deferred cwd pass
// CorrelateCrossOS — same MAX-upgrade discipline, same "high is authoritative"
// guard, same idempotence.
//
// WORK BOUND. The pass touches ONLY the in-memory tree (bounded by
// Options.MaxTracked, in practice the live process table) — never the
// persisted process_runs table, which is millions of rows. Within that tree,
// lateSeedRules drops everything already at or above the seed's confidence and
// everything older than MaxAge, and MaxLookups caps the surviving candidates.
// So the marginal cost of a pass in the steady state is ZERO SeedLookup calls:
// once a subtree is upgraded to high, every one of its runs fails the
// authoritative rule for the rest of its life.
//
// IDEMPOTENCE. An upgrade lands the run at the seed's confidence, and outranks
// is strict, so re-running the pass re-classifies it as skip_authoritative
// before any lookup happens. A second pass returns an empty result and writes
// nothing.
//
// now is the clock the staleness rule is measured against (the caller's
// Options.Now). Returns an empty result when there is no SeedLookup, no tracked
// run, or the policy is disabled.
func (a *Attributor) ReconcileLateSeeds(now time.Time, policy LateSeedPolicy) LateSeedResult {
	var res LateSeedResult
	if a == nil || a.seed == nil || len(a.runs) == 0 {
		return res
	}
	p := policy.withDefaults()
	if !p.Enabled() {
		return res
	}

	// A direct pid seed carries ConfHigh unless the SeedLookup says otherwise;
	// that is what resolveAttribution assumes (orDefaultConf(s.Confidence,
	// ConfHigh)), so the candidate filter must assume the same.
	const seedConfidence = ConfHigh

	res.Examined = len(a.runs)
	candidates := make([]*ProcessRun, 0, 16)
	for _, run := range a.runs {
		c := lateSeedCandidate{Run: run, Now: now, Policy: p, SeedConfidence: seedConfidence}
		if classifyLateSeed(c) != lateSeedProbe {
			continue
		}
		candidates = append(candidates, run)
	}
	res.Candidates = len(candidates)
	if len(candidates) == 0 {
		return res
	}

	// Newest-first, ties broken on the stable process key: map iteration is
	// random, so without this the MaxLookups clip would be non-deterministic —
	// and newest-first is also the right priority, because a late seed always
	// names a process that started recently.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].StartedAt.Equal(candidates[j].StartedAt) {
			return candidates[i].StartedAt.After(candidates[j].StartedAt)
		}
		return candidates[i].ProcessKey < candidates[j].ProcessKey
	})
	if p.MaxLookups > 0 && len(candidates) > p.MaxLookups {
		candidates = candidates[:p.MaxLookups]
		res.Truncated = true
	}

	// The children index is built at most once per pass, and only when an
	// upgrade actually happens — the steady-state pass allocates nothing.
	var children map[string][]string
	for _, run := range candidates {
		res.Probed++
		seed, ok := a.seed(run.PID)
		if !ok || seed.SessionID == "" {
			continue
		}
		next := Attribution{
			SessionID:  seed.SessionID,
			Tool:       seed.Tool,
			ProjectID:  seed.ProjectID,
			Source:     orDefault(seed.Source, AttrBridge),
			Confidence: orDefaultConf(seed.Confidence, ConfHigh),
		}
		// Re-check against the ACTUAL seed confidence (the lookup may return
		// something weaker than the assumed high): a MAX-upgrade, never a
		// clobber.
		if !outranks(next.Confidence, run.Attribution.Confidence) {
			continue
		}
		// A direct identity clears the boundary flag, exactly as
		// resolveAttribution does when a boundary process turns out to be a
		// directly-identified AI-tool root.
		run.IsBoundary = false
		run.Attribution = next
		res.Roots++
		res.Upgraded = append(res.Upgraded, run)

		if children == nil {
			children = a.childIndex()
		}
		res.Reinherited += a.reinheritSubtree(run, children, &res.Upgraded)
	}
	return res
}

// childIndex builds a parent-key → child-keys index over the live tree. Runs
// that name themselves as parent are skipped so a corrupt self-edge can never
// become a one-node cycle.
func (a *Attributor) childIndex() map[string][]string {
	idx := make(map[string][]string, len(a.runs))
	for key, run := range a.runs {
		if run.ParentProcessKey == "" || run.ParentProcessKey == key {
			continue
		}
		idx[run.ParentProcessKey] = append(idx[run.ParentProcessKey], key)
	}
	for _, kids := range idx {
		sort.Strings(kids) // deterministic walk order
	}
	return idx
}

// reinheritSubtree re-runs the §9.2.2 inheritance walk below an upgraded run
// and appends every descendant it changed to out. This is where the payoff is:
// the seeded pid is a launcher, and the tool's real workers are its children.
//
// It mirrors CorrelateCrossOS's subtree DFS: a descendant already attributed at
// or above what it would inherit OWNS ITSELF, so it is neither overwritten nor
// descended into (its own subtree already inherits from it); a boundary
// (§9.2.6) is never attributed and never descended through; an exited or
// no-longer-tracked descendant is skipped. A visited set plus the self-edge
// guard in childIndex make a cyclic parent chain terminate.
func (a *Attributor) reinheritSubtree(root *ProcessRun, children map[string][]string, out *[]*ProcessRun) int {
	changed := 0
	visited := map[string]bool{root.ProcessKey: true}
	stack := append([]string(nil), children[root.ProcessKey]...)
	for len(stack) > 0 {
		key := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[key] {
			continue
		}
		visited[key] = true

		child := a.runs[key]
		if child == nil || child.Exited || child.IsBoundary {
			continue
		}
		parent := a.runs[child.ParentProcessKey]
		if parent == nil {
			continue
		}
		want := inherit(parent)
		if want.SessionID == "" {
			continue
		}
		if !outranks(want.Confidence, child.Attribution.Confidence) {
			continue // owns itself — do not overwrite, do not descend
		}
		child.Attribution = want
		*out = append(*out, child)
		changed++
		stack = append(stack, children[key]...)
	}
	return changed
}
