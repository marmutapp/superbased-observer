package admission

import (
	"context"
	"strings"
)

// Turn is one prior conversation message (for conversation-scope evaluation,
// P3). P1 evaluates the last user message only.
type Turn struct {
	Role string
	Text string
}

// Request is one incoming admission check. Text is the user message; the
// tenant/user/session tags mirror the obs span tags (obs plan §3.1).
type Request struct {
	Text    string
	Tenant  string
	User    string
	Session string
	History []Turn
	Meta    map[string]string
}

// Result is the plain verdict Evaluate returns. admissionsvc turns it into an
// SDK/proxy response and an obs_admission_events row; the judge's types never
// leak past this boundary.
type Result struct {
	Decision  Decision
	Severity  Severity
	Criterion string // the criterion id that fired ("" if none)
	Reason    string // written for the end user / calling app to surface
	JudgeUsed bool
	Degraded  string // "" | "timeout-failopen" | "cache" | "prefiltered" | "no-judge"
	LatencyMS int
}

// allow is the default admit result.
func allow() Result { return Result{Decision: DecisionAllow, Severity: SeverityInfo} }

// evalConfig holds the optional per-call dependencies threaded into Evaluate
// via EvalOption (the secret-gate injection). Zero value = the gate is off, so
// existing callers are unaffected (additive, CLAUDE.md #6).
type evalConfig struct {
	secretDetect func(string) []string
	judgeRemote  bool
}

// EvalOption configures an Evaluate call.
type EvalOption func(*evalConfig)

// WithSecretGate injects the pattern-certain secret detector and whether the
// judge is REMOTE, enabling the §3 layer-3 gate: a secret-bearing request is
// never egressed to a hosted judge; the policy's SecretRemoteJudge decision is
// returned locally instead. detect returns the CERTAIN secret-type names found
// (entropy guesses excluded by the caller). A nil detector or a local judge
// leaves the gate inert.
func WithSecretGate(detect func(string) []string, judgeRemote bool) EvalOption {
	return func(c *evalConfig) {
		c.secretDetect = detect
		c.judgeRemote = judgeRemote
	}
}

// Evaluate walks the layered pipeline (admission spec §3) and SHORT-CIRCUITS on
// the first terminal decision. It is the ONE seam every enforcement point
// calls. judge may be nil (no judge wired) — the deterministic layers still
// run and judged criteria are skipped. The caller owns the context deadline;
// on judge error/timeout Evaluate applies the policy's fail-mode.
//
// Layer order:
//  1. Fast allow — an explicit prefilter allow-list match admits immediately.
//  2. Deterministic deny — prefilter deny list, length ceiling, denied_topics,
//     and the jailbreak heuristic, in criterion order — no judge.
//  3. Secret gate — a secret-bearing request that would otherwise reach a
//     REMOTE judge is decided locally (never egressed) per SecretRemoteJudge.
//  4. LLM judge — only the ambiguous middle: judged criteria, when a judge is
//     wired and no prior layer was terminal.
//  5. Default — admit.
func Evaluate(ctx context.Context, r Request, p PolicySpec, judge JudgeClient, opts ...EvalOption) Result {
	var ec evalConfig
	for _, o := range opts {
		o(&ec)
	}
	text := strings.TrimSpace(r.Text)

	// Layer 1 — fast allow.
	if anyMatch(text, p.Prefilter.Allow) {
		res := allow()
		res.Degraded = "prefiltered"
		return res
	}

	// Layer 2a — length ceiling (deterministic deny).
	if p.Prefilter.MaxMessageBytes > 0 && len(text) > p.Prefilter.MaxMessageBytes {
		return Result{
			Decision: DecisionDeny, Severity: SeverityWarn, Criterion: "prefilter.length",
			Reason: "Request exceeds the maximum allowed length.", Degraded: "prefiltered",
		}
	}

	// Layer 2b — prefilter deny list (deterministic deny).
	if anyMatch(text, p.Prefilter.Deny) {
		return Result{
			Decision: DecisionDeny, Severity: SeverityWarn, Criterion: "prefilter.deny",
			Reason: "Request matched a denied pattern.", Degraded: "prefiltered",
		}
	}

	// Layer 2c — deterministic criteria (denied_topics + jailbreak), in policy
	// order. First terminal (Ask/Deny) short-circuits; a Flag is remembered but
	// not terminal (a later criterion may deny).
	var pending *Result
	for i := range p.Criteria {
		c := p.Criteria[i]
		fired, reason := deterministicFire(text, c)
		if !fired {
			continue
		}
		res := Result{
			Decision: c.Decision, Severity: c.Severity, Criterion: c.ID,
			Reason: reason, Degraded: "prefiltered",
		}
		if c.Decision >= DecisionAsk {
			return res // terminal
		}
		if pending == nil || res.Decision > pending.Decision {
			pending = &res
		}
	}

	// Layer 3 — secret gate. When the request would reach a REMOTE judge and it
	// carries a pattern-certain secret, decide locally per SecretRemoteJudge
	// and DON'T egress it (the security half — scrubbing — is item 8; this is
	// the policy half). Only relevant when a remote judge would actually be
	// called (judged criteria present). The gate result combines with any
	// pending deterministic flag (stricter wins); the judge is never called.
	if judged := judgedCriteria(p); judge != nil && len(judged) > 0 &&
		ec.judgeRemote && p.SecretRemoteJudge >= DecisionFlag && ec.secretDetect != nil {
		if kinds := ec.secretDetect(text); len(kinds) > 0 {
			gate := Result{
				Decision: p.SecretRemoteJudge, Severity: SeverityHigh,
				Criterion: "secret.remote_judge",
				Reason:    "Request contains a secret; not sent to the remote judge.",
				Degraded:  "secret-gate",
			}
			if pending != nil && pending.Decision > gate.Decision {
				return *pending
			}
			return gate
		}
	}

	// Layer 4 — LLM judge for the ambiguous middle (judged criteria only).
	// The judge always yields a Result (a verdict, or a fail-mode result on
	// error). The final decision is the STRICTER of the pending deterministic
	// flag and the judge result; judge metadata (JudgeUsed / Degraded) is
	// carried onto the winner regardless.
	if judged := judgedCriteria(p); judge != nil && len(judged) > 0 {
		jr := runJudge(ctx, text, judged, p, judge)
		if pending != nil && pending.Decision >= jr.Decision {
			winner := *pending
			winner.JudgeUsed = true
			if winner.Degraded == "" {
				winner.Degraded = jr.Degraded
			}
			return winner
		}
		return jr
	}

	if pending != nil {
		return *pending
	}
	return allow()
}

// deterministicFire reports whether a deterministic criterion (denied_topics /
// jailbreak) fires on text, with a user-facing reason. Non-deterministic
// (judged) types never fire here.
func deterministicFire(text string, c Criterion) (bool, string) {
	switch c.Type {
	case TypeDeniedTopics:
		if term := matchTopics(text, c); term != "" {
			reason := c.Name
			if reason == "" {
				reason = "Request references a denied topic."
			}
			return true, reason
		}
	case TypeJailbreak:
		if anyMatch(text, jailbreakHeuristics) {
			reason := c.Name
			if reason == "" {
				reason = "Request looks like a prompt-injection / jailbreak attempt."
			}
			return true, reason
		}
	}
	return false, ""
}

// judgedCriteria returns the criteria the LLM judge adjudicates.
func judgedCriteria(p PolicySpec) []Criterion {
	var out []Criterion
	for _, c := range p.Criteria {
		if c.Type.judged() {
			out = append(out, c)
		}
	}
	return out
}

// runJudge calls the injected judge and maps its verdict onto the policy,
// always returning a single Result with JudgeUsed=true. On judge error/timeout
// or an unparseable reply it applies the fail-mode: strict → Deny; else
// fail-open Allow with Degraded="timeout-failopen". A parsed allow returns a
// clean Allow (JudgeUsed=true, no Degraded).
func runJudge(ctx context.Context, text string, judged []Criterion, p PolicySpec, judge JudgeClient) Result {
	prompt := buildJudgePrompt(text, judged)
	raw, err := judge.Judge(ctx, prompt)
	if err != nil {
		return judgeFailMode(p, "Policy check unavailable.")
	}
	v, ok := parseJudgeVerdict(raw)
	if !ok {
		return judgeFailMode(p, "Policy check returned an unreadable verdict.")
	}

	dec, _ := ParseDecision(v.Decision)
	if dec == DecisionAllow {
		return Result{Decision: DecisionAllow, Severity: SeverityInfo, JudgeUsed: true}
	}
	// Attribute to the judge-named criterion when valid, else the strictest
	// judged criterion; clamp severity to that criterion.
	c := attributeJudged(v.Criterion, judged)
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "Request is outside the allowed policy."
	}
	return Result{Decision: dec, Severity: c.Severity, Criterion: c.ID, Reason: reason, JudgeUsed: true}
}

// judgeFailMode is the shared fail-open/closed result on a judge error.
func judgeFailMode(p PolicySpec, denyReason string) Result {
	if p.Strict {
		return Result{
			Decision: DecisionDeny, Severity: SeverityWarn, JudgeUsed: true,
			Reason: denyReason, Degraded: "timeout-failopen",
		}
	}
	return Result{Decision: DecisionAllow, Severity: SeverityInfo, JudgeUsed: true, Degraded: "timeout-failopen"}
}

// attributeJudged picks the criterion a judge verdict belongs to: the
// judge-named id when it matches a judged criterion, otherwise the strictest
// (highest-decision, then highest-severity) judged criterion.
func attributeJudged(named string, judged []Criterion) Criterion {
	named = strings.TrimSpace(named)
	for _, c := range judged {
		if c.ID == named {
			return c
		}
	}
	best := judged[0]
	for _, c := range judged[1:] {
		if c.Decision > best.Decision || (c.Decision == best.Decision && c.Severity > best.Severity) {
			best = c
		}
	}
	return best
}
