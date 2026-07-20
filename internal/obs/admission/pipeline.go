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
	Degraded  string // one of the Degraded* reason codes ("" = none)
	LatencyMS int
}

// Degraded reason codes recorded on Result.Degraded. Only a SUBSET denote
// genuine degradation — a judge that could not deliver a verdict; the rest mark
// fully-intended, deterministic outcomes (a prefilter/criterion short-circuit,
// the secret gate deciding locally, or a verdict-cache hit) that must NOT be
// counted against judge health (gap-audit §5.6 — calibrate mis-flagged these as
// "NOT recommended"). IsDegraded is the ONE owner of that classification.
const (
	// DegradedTimeoutFailopen: the judge errored, timed out, or returned an
	// unreadable verdict, so the policy fail-mode was applied. REAL degradation.
	DegradedTimeoutFailopen = "timeout-failopen"
	// DegradedNoJudge: judged criteria exist but no judge is wired, so they
	// could not be adjudicated. REAL degradation.
	DegradedNoJudge = "no-judge"
	// DegradedPrefiltered: a deterministic prefilter/criterion layer decided the
	// request without the judge. INTENDED short-circuit, NOT degradation.
	DegradedPrefiltered = "prefiltered"
	// DegradedSecretGate: the §3 layer-3 secret gate decided the request locally
	// rather than egress it to a remote judge. INTENDED, NOT degradation.
	DegradedSecretGate = "secret-gate"
	// DegradedCache: the verdict was served from the boundary verdict cache.
	// INTENDED, NOT degradation.
	DegradedCache = "cache"
)

// IsDegraded reports whether a Result.Degraded reason code denotes GENUINE
// judge degradation (the judge could not deliver a verdict), as opposed to an
// intended deterministic short-circuit or a cache hit. Calibrate and any
// judge-health surface MUST count degradation through this predicate, never
// `Degraded != ""` — a prefilter/secret-gate short-circuit is a correct,
// deterministic outcome, not a failure of the judge (gap-audit §5.6).
func IsDegraded(code string) bool {
	switch code {
	case DegradedTimeoutFailopen, DegradedNoJudge:
		return true
	default:
		return false
	}
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
		res.Degraded = DegradedPrefiltered
		return res
	}

	// Layer 2a — length ceiling (deterministic deny).
	if p.Prefilter.MaxMessageBytes > 0 && len(text) > p.Prefilter.MaxMessageBytes {
		return Result{
			Decision: DecisionDeny, Severity: SeverityWarn, Criterion: "prefilter.length",
			Reason: "Request exceeds the maximum allowed length.", Degraded: DegradedPrefiltered,
		}
	}

	// Layer 2b — prefilter deny list (deterministic deny).
	if anyMatch(text, p.Prefilter.Deny) {
		return Result{
			Decision: DecisionDeny, Severity: SeverityWarn, Criterion: "prefilter.deny",
			Reason: "Request matched a denied pattern.", Degraded: DegradedPrefiltered,
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
			Reason: reason, Degraded: DegradedPrefiltered,
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
				Degraded:  DegradedSecretGate,
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

// DefaultJudgeChunkBytes / DefaultJudgeChunkOverlapBytes are the in-core
// map-reduce chunking defaults, mirroring the demo's app-layer strategy
// (docs/admission-ollama-demo-playbook.md): 3500-byte windows with 200-byte
// overlap — so a small judge model never receives an over-long prompt (which it
// would silently front-truncate) yet a concern straddling a window boundary is
// still seen whole by one chunk.
const (
	DefaultJudgeChunkBytes        = 3500
	DefaultJudgeChunkOverlapBytes = 200
)

// runJudge adjudicates the judged criteria with the LLM judge and always
// returns a single Result (JudgeUsed=true). Content larger than the policy's
// judge-chunk size is split into overlapping windows (chunkForJudge), each
// judged, and the per-chunk verdicts reduced STRICTEST-WINS (any deny wins over
// ask over flag over allow — spec §4 / the demo's app-layer map-reduce brought
// in-core). The common path is a single chunk. Genuine per-chunk degradation
// (a judge error mapped through the fail-mode) is carried onto the aggregate;
// an intended allow verdict is never marked degraded.
func runJudge(ctx context.Context, text string, judged []Criterion, p PolicySpec, judge JudgeClient) Result {
	chunks := chunkForJudge(text, p.JudgeChunkBytes, p.JudgeChunkOverlapBytes)
	if len(chunks) == 1 {
		return judgeOne(ctx, chunks[0], judged, p, judge)
	}
	best := Result{Decision: DecisionAllow, Severity: SeverityInfo, JudgeUsed: true}
	degraded := ""
	for _, chunk := range chunks {
		jr := judgeOne(ctx, chunk, judged, p, judge)
		if jr.Degraded != "" && degraded == "" {
			degraded = jr.Degraded
		}
		if jr.Decision > best.Decision {
			best = jr
		}
		if best.Decision == DecisionDeny {
			break // strictest possible verdict — later chunks cannot escalate
		}
	}
	best.JudgeUsed = true
	if best.Degraded == "" {
		best.Degraded = degraded
	}
	return best
}

// chunkForJudge splits text into judge-sized windows so a large request never
// overruns a small judge model's context. Each window is at most size bytes;
// successive windows start overlap bytes before the previous window's end so a
// concern straddling a boundary is still seen whole by one chunk. Windows break
// on a whitespace boundary in their back half where possible (avoiding a
// mid-word cut), else at a UTF-8 rune boundary. size<=0 → DefaultJudgeChunkBytes;
// overlap is clamped to [0, size) so chunking always makes forward progress.
// text no larger than size yields the single input unchanged.
func chunkForJudge(text string, size, overlap int) []string {
	if size <= 0 {
		size = DefaultJudgeChunkBytes
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= size {
		overlap = size / 2
	}
	if len(text) <= size {
		return []string{text}
	}
	var chunks []string
	for start := 0; start < len(text); {
		end := start + size
		if end >= len(text) {
			chunks = append(chunks, text[start:])
			break
		}
		cut := end
		// Prefer a whitespace boundary in the window's back half so we don't
		// split mid-word; fall back to a rune boundary at the raw offset.
		for i := end; i > start+size/2; i-- {
			if isASCIISpace(text[i-1]) {
				cut = i
				break
			}
		}
		if cut == end {
			cut = runeBoundary(text, end)
		}
		chunks = append(chunks, text[start:cut])
		next := runeBoundary(text, cut-overlap)
		if next <= start {
			next = cut // overlap must not rewind into the current window's start
		}
		start = next
	}
	return chunks
}

// isASCIISpace reports whether b is a space/tab/newline/carriage-return.
func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// runeBoundary snaps i back to the start of a UTF-8 rune so a byte-offset cut
// never lands inside a multi-byte sequence. i outside [0,len(s)] is clamped.
func runeBoundary(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && s[i]&0xC0 == 0x80 {
		i--
	}
	return i
}

// judgeFailMode is the shared fail-open/closed result on a judge error.
func judgeFailMode(p PolicySpec, denyReason string) Result {
	if p.Strict {
		return Result{
			Decision: DecisionDeny, Severity: SeverityWarn, JudgeUsed: true,
			Reason: denyReason, Degraded: DegradedTimeoutFailopen,
		}
	}
	return Result{Decision: DecisionAllow, Severity: SeverityInfo, JudgeUsed: true, Degraded: DegradedTimeoutFailopen}
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
