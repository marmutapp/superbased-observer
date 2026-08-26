package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Decision is the admission verdict, ordered by strictness so a layered /
// merged policy can ESCALATE but never relax (admission spec §4). Allow is the
// weakest, Deny the strongest.
type Decision int

const (
	DecisionAllow Decision = iota // admit the request
	DecisionFlag                  // admit but record a concern
	DecisionAsk                   // route to a human / require confirmation
	DecisionDeny                  // reject the request
)

// String renders the persisted, user-facing decision token.
func (d Decision) String() string {
	switch d {
	case DecisionAllow:
		return "allow"
	case DecisionFlag:
		return "flag"
	case DecisionAsk:
		return "ask"
	case DecisionDeny:
		return "deny"
	default:
		return "allow"
	}
}

// ParseDecision maps a token to a Decision; unknown/empty → Allow (the safe
// default — an unreadable decision must never silently deny). ok reports
// whether the token was recognized so lint can flag typos.
func ParseDecision(s string) (Decision, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow", "":
		return DecisionAllow, true
	case "flag":
		return DecisionFlag, true
	case "ask":
		return DecisionAsk, true
	case "deny", "block":
		return DecisionDeny, true
	default:
		return DecisionAllow, false
	}
}

// Severity borrows the guard-layer vocabulary as a CONVENTION (admission spec
// borrows-conventions note), ordered low→high.
type Severity int

const (
	SeverityInfo Severity = iota
	SeverityWarn
	SeverityHigh
	SeverityCritical
)

// String renders the persisted severity token.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarn:
		return "warn"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	default:
		return "info"
	}
}

// ParseSeverity maps a token to a Severity; unknown/empty → Warn (a sensible
// mid default). ok reports recognition for lint.
func ParseSeverity(s string) (Severity, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return SeverityInfo, true
	case "warn", "warning", "":
		return SeverityWarn, true
	case "high":
		return SeverityHigh, true
	case "critical":
		return SeverityCritical, true
	default:
		return SeverityWarn, false
	}
}

// CriterionType selects which pipeline layer adjudicates a criterion.
// denied_topics + jailbreak are deterministic pre-filters; valid_use_case +
// custom are judged by the LLM.
type CriterionType string

const (
	TypeValidUseCase CriterionType = "valid_use_case"
	TypeDeniedTopics CriterionType = "denied_topics"
	TypeJailbreak    CriterionType = "jailbreak"
	TypeCustom       CriterionType = "custom"
)

// Judged reports whether this type is adjudicated by the LLM judge (vs a
// deterministic pre-filter). Exported so callers outside this package (the
// evaluation engine in internal/obs/admission, and RequiresJudge/
// ValidateRuntimeCaps below) can branch on it via the CriterionType alias.
func (t CriterionType) Judged() bool {
	return t == TypeValidUseCase || t == TypeCustom
}

// Mode is the admission enforcement posture (off | observe | enforce). P1
// ships observe only; enforce is P2.
type Mode int

const (
	ModeOff Mode = iota
	ModeObserve
	ModeEnforce
)

// ParseMode maps a token to a Mode; empty → Observe (observe-first).
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "disabled":
		return ModeOff
	case "enforce":
		return ModeEnforce
	default:
		return ModeObserve
	}
}

// String renders the mode token.
func (m Mode) String() string {
	switch m {
	case ModeOff:
		return "off"
	case ModeEnforce:
		return "enforce"
	default:
		return "observe"
	}
}

// Scope selects how much of the conversation the judge sees. P1 = last_user.
type Scope int

const (
	ScopeLastUser Scope = iota
	ScopeConversation
)

// ParseScope maps a token to a Scope; empty/unknown → last_user (P1 default).
func ParseScope(s string) Scope {
	if strings.EqualFold(strings.TrimSpace(s), "conversation") {
		return ScopeConversation
	}
	return ScopeLastUser
}

// String renders the scope token.
func (s Scope) String() string {
	if s == ScopeConversation {
		return "conversation"
	}
	return "last_user"
}

// Criterion is one compiled policy rule. denied_topics criteria carry
// normalized substring Topics; judged types carry a Definition rendered into
// the rubric. (Regex power lives in the prefilter allow/deny lists; topics are
// plain phrases so admins author them without regex — spec §4.)
type Criterion struct {
	ID         string
	Type       CriterionType
	Name       string
	Definition string
	Topics     []string
	Decision   Decision
	Severity   Severity
}

// Prefilter is the compiled deterministic layer (spec §3 layers 1-2).
type Prefilter struct {
	Allow           []*regexp.Regexp
	Deny            []*regexp.Regexp
	MaxMessageBytes int
}

// PolicySpec is the compiled, ready-to-evaluate policy. Hash is the content
// address of the policy (cache-key component + audit provenance).
type PolicySpec struct {
	Mode      Mode
	Strict    bool
	Scope     Scope
	Criteria  []Criterion
	Prefilter Prefilter
	// SecretRemoteJudge is the local decision applied when a request carries a
	// pattern-certain secret and the judge is REMOTE, so the request is never
	// egressed to the hosted judge (spec §3 layer 3). DecisionAllow = off.
	SecretRemoteJudge Decision
	// JudgeChunkBytes bounds a single judge call's content size. Content longer
	// than this is split into overlapping windows, each judged, and the
	// verdicts reduced strictest-wins (map-reduce, spec §4 / the demo's
	// app-layer chunking brought in-core — docs/admission-ollama-demo-playbook.md).
	// 0 = the engine default (DefaultJudgeChunkBytes).
	JudgeChunkBytes int
	// JudgeChunkOverlapBytes is the overlap between adjacent judge windows so a
	// concern straddling a window boundary is still seen whole by one chunk.
	// 0 = the engine default (DefaultJudgeChunkOverlapBytes); it is clamped
	// below JudgeChunkBytes so chunking always makes forward progress.
	JudgeChunkOverlapBytes int
	// JudgeRetries is how many EXTRA bounded in-process attempts judgeOne makes
	// on a judge transport error before applying the fail-mode (Strict → Deny;
	// else Allow). 0 = one attempt, no retry (today's behavior). A cheap
	// reliability lever for a flaky/cold-starting judge that does NOT change the
	// synchronous single-Result contract — distinct from a request-queueing
	// posture, which would need a new proxy AdmitResult shape.
	JudgeRetries int
	Hash         string
}

// PolicyInput is the plain, pre-compile policy translated from config AT THE
// BOUNDARY (admissionsvc), so this pure package never imports internal/config.
type PolicyInput struct {
	Mode      string
	Strict    bool
	Scope     string
	Criteria  []CriterionInput
	Prefilter PrefilterInput
	// SecretRemoteJudge names the local decision (allow|flag|ask|deny) for the
	// secret-bearing-request-to-a-remote-judge gate. Empty = allow = off.
	SecretRemoteJudge string
	// JudgeChunkBytes / JudgeChunkOverlapBytes configure map-reduce judge
	// chunking; 0 for either = the engine default. See PolicySpec.
	JudgeChunkBytes        int
	JudgeChunkOverlapBytes int
	// JudgeRetries is the bounded in-process retry count on judge transport
	// error before the fail-mode fires. 0 = no retry. See PolicySpec.
	JudgeRetries int
}

// CriterionInput is one uncompiled criterion.
type CriterionInput struct {
	ID         string
	Type       string
	Name       string
	Definition string
	Topics     []string
	Decision   string
	Severity   string
}

// PrefilterInput is the uncompiled deterministic layer.
type PrefilterInput struct {
	Allow           []string
	Deny            []string
	MaxMessageBytes int
}

// Compile validates a PolicyInput and produces a ready PolicySpec: it compiles
// every regex, resolves the decision/severity/type vocabulary, and computes a
// deterministic content Hash. A malformed regex or unknown decision is a hard
// error so a bad policy is caught at load, never at request time.
func Compile(in PolicyInput) (PolicySpec, error) {
	secretDec, ok := ParseDecision(in.SecretRemoteJudge)
	if !ok {
		return PolicySpec{}, fmt.Errorf("admission.Compile: unknown secret_remote_judge decision %q", in.SecretRemoteJudge)
	}
	spec := PolicySpec{
		Mode:                   ParseMode(in.Mode),
		Strict:                 in.Strict,
		Scope:                  ParseScope(in.Scope),
		SecretRemoteJudge:      secretDec,
		JudgeChunkBytes:        in.JudgeChunkBytes,
		JudgeChunkOverlapBytes: in.JudgeChunkOverlapBytes,
		JudgeRetries:           in.JudgeRetries,
	}

	pf, err := compilePatterns("prefilter.allow", in.Prefilter.Allow)
	if err != nil {
		return PolicySpec{}, err
	}
	pd, err := compilePatterns("prefilter.deny", in.Prefilter.Deny)
	if err != nil {
		return PolicySpec{}, err
	}
	spec.Prefilter = Prefilter{Allow: pf, Deny: pd, MaxMessageBytes: in.Prefilter.MaxMessageBytes}

	for _, ci := range in.Criteria {
		dec, ok := ParseDecision(ci.Decision)
		if !ok {
			return PolicySpec{}, fmt.Errorf("admission.Compile: criterion %q: unknown decision %q", ci.ID, ci.Decision)
		}
		sev, ok := ParseSeverity(ci.Severity)
		if !ok {
			return PolicySpec{}, fmt.Errorf("admission.Compile: criterion %q: unknown severity %q", ci.ID, ci.Severity)
		}
		spec.Criteria = append(spec.Criteria, Criterion{
			ID:         ci.ID,
			Type:       CriterionType(strings.TrimSpace(ci.Type)),
			Name:       ci.Name,
			Definition: ci.Definition,
			Topics:     NormalizeTopics(ci.Topics),
			Decision:   dec,
			Severity:   sev,
		})
	}

	spec.Hash = hashPolicy(in)
	return spec, nil
}

// NormalizeTopics lowercases each denied-topic phrase and strips an optional
// "label:" organizational prefix (e.g. "competitor:AcmeCorp" → "acmecorp"),
// so the match term is what the admin actually wants matched. Empty entries
// are dropped. Exported because internal/obs/admission's templates.go (a
// caller outside this package) also normalizes rendered topics.
func NormalizeTopics(topics []string) []string {
	var out []string
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if i := strings.IndexByte(t, ':'); i >= 0 && isLabelPrefix(t[:i]) {
			t = strings.TrimSpace(t[i+1:])
		}
		if t == "" {
			continue
		}
		out = append(out, strings.ToLower(t))
	}
	return out
}

// isLabelPrefix reports whether s is a simple organizational label (letters /
// digits / _ / - only) — used to decide whether a "prefix:value" topic should
// have its prefix stripped. A value containing spaces or regex-ish characters
// is NOT a label, so "http://..." or "a: b" keep their colon.
func isLabelPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// compilePatterns compiles each pattern case-insensitively. A pattern is
// treated as a regex; plain substrings/prefixes are valid regexes too, so no
// separate "prefix vs regex" mode is needed.
func compilePatterns(where string, pats []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range pats {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			return nil, fmt.Errorf("admission.Compile: %s: bad pattern %q: %w", where, p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// hashPolicy computes a stable content hash over the semantic policy fields so
// the same policy always yields the same Hash (cache key + audit). Field order
// is fixed and criteria are sorted by id.
func hashPolicy(in PolicyInput) string {
	h := sha256.New()
	writeField := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0x1e})
		}
	}
	writeField("mode", ParseMode(in.Mode).String())
	writeField("scope", ParseScope(in.Scope).String())
	if in.Strict {
		writeField("strict", "1")
	}
	writeField("pf_max", fmt.Sprintf("%d", in.Prefilter.MaxMessageBytes))
	if in.SecretRemoteJudge != "" {
		writeField("secret_remote_judge", in.SecretRemoteJudge)
	}
	if in.JudgeChunkBytes > 0 {
		writeField("judge_chunk_bytes", fmt.Sprintf("%d", in.JudgeChunkBytes))
	}
	if in.JudgeChunkOverlapBytes > 0 {
		writeField("judge_chunk_overlap", fmt.Sprintf("%d", in.JudgeChunkOverlapBytes))
	}
	if in.JudgeRetries > 0 {
		writeField("judge_retries", fmt.Sprintf("%d", in.JudgeRetries))
	}
	for _, a := range in.Prefilter.Allow {
		writeField("pf_allow", a)
	}
	for _, d := range in.Prefilter.Deny {
		writeField("pf_deny", d)
	}
	crit := append([]CriterionInput(nil), in.Criteria...)
	sort.Slice(crit, func(i, j int) bool { return crit[i].ID < crit[j].ID })
	for _, c := range crit {
		writeField("crit", c.ID, c.Type, c.Definition, c.Decision, c.Severity)
		for _, t := range c.Topics {
			writeField("topic", t)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
