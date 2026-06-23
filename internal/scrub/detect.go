package scrub

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Typed secret detection (guard spec §8.2, G9). The plain Scrubber
// redacts secrets destructively for STORAGE (everything becomes
// "[REDACTED]"); the typed surface here answers a different question —
// WHAT kind of secret appears WHERE — so the proxy egress scan can
// flag/mask/deny with a per-type decision and the R-172 shell-arg rule
// can name the detector that hit. The two surfaces share the same
// shape vocabulary deliberately: a value the Scrubber would redact in
// stored excerpts is a value the egress scan flags on the wire.
//
// Privacy contract: TypedFinding.Value holds the MATCHED SECRET and
// exists only so callers can apply allowlists ([guard.proxy]
// egress_allow) in memory. It must NEVER be persisted, logged, or
// placed on a verdict Reason — callers persist Type counts only.

// TypedFinding is one typed secret detection.
type TypedFinding struct {
	// Type is the stable detector name ("github_pat", "bearer_token",
	// "aws_access_key", ... , "entropy"). Stable strings — they appear
	// in guard_events reasons (as counts) and in [REDACTED:<type>]
	// mask markers.
	Type string
	// Certain reports a pattern-certain detection (a shape that is a
	// secret by construction or by key context). The entropy heuristic
	// is the only non-certain detector; spec §8.2 gates masking on
	// certainty.
	Certain bool
	// Value is the matched secret text — IN-MEMORY ONLY (see the
	// package privacy contract above). Used for egress_allow matching.
	Value string
}

// typedDetector is one row of the typed detection table. Rows reuse
// the defaultPatterns shapes where those are value-bearing, adding a
// stable type name, a value capture group and a cheap literal gate so
// the regex only runs on bodies that can possibly match (latency
// budget §17.9: the proxy egress scan runs per request).
type typedDetector struct {
	// name is the stable TypedFinding.Type.
	name string
	// re is the detection regex.
	re *regexp.Regexp
	// valueGroup is the submatch index holding the secret value
	// (0 = the full match).
	valueGroup int
	// gates are lowercase literals, ANY of which must appear in the
	// lowercased input for the regex to run. Empty means "always run".
	gates []string
	// certain marks pattern-certain rows (everything but entropy).
	certain bool
	// lowercase marks rows whose pattern is WRITTEN lowercase and
	// matched against the ASCII-lowered input instead of carrying
	// (?i). Go's RE2 loses its literal-prefix fast paths under (?i),
	// which blew the §17.9 budget ~5× on realistic bodies; ASCII
	// lowering is length-preserving, so match spans map 1:1 back to
	// the original bytes (values are extracted case-preserved).
	// Case-SENSITIVE rows (AKIA, gh*_, PEM headers) keep
	// lowercase=false and match the original input.
	lowercase bool
	// anchored switches the row from one full-body FindAll pass to
	// anchored attempts at each gate-literal occurrence (the match
	// must BEGIN at the gate, offset by anchorBack / backScanKey).
	// This is the second §17.9 lever: keyword-alternation patterns
	// ("password|secret|token…") have no usable literal prefix, so a
	// full-body NFA scan costs ~16MB/s — while an \A-anchored attempt
	// per occurrence costs O(match length). Rows whose pattern starts
	// with a selective literal (gh*_, AKIA, "-----BEGIN", "://") stay
	// unanchored: RE2's literal-prefix fast path already handles them.
	anchored bool
	// anchorBack is the fixed byte count the match starts BEFORE the
	// gate occurrence (json_secret_value: the opening quote).
	anchorBack int
	// backScanKey, for env_secret_value, walks back over [a-z0-9_]
	// from the gate occurrence to the opening quote (the keyword may
	// sit mid-key: "github_token").
	backScanKey bool
}

// typedDetectors is the detection table, walked in order. Earlier rows
// win span-overlap ties (github_pat beats the generic entropy row on
// the same bytes). One test case per row minimum (§18).
var typedDetectors = []typedDetector{
	{
		name: "github_pat", certain: true,
		re:    regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
		gates: []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"},
	},
	{
		name: "bearer_token", certain: true, lowercase: true, anchored: true,
		// Keep the "bearer " prefix out of the value so masking yields
		// "Bearer [REDACTED:bearer_token]" — still header-shaped. The
		// {8,} floor (absent from the storage pattern) cuts FPs on
		// prose like "Bearer of".
		re:         regexp.MustCompile(`\Abearer\s+([a-z0-9\-._~+/]{8,}=*)`),
		valueGroup: 1,
		gates:      []string{"bearer"},
	},
	{
		name: "api_key_prefixed", certain: true, lowercase: true, anchored: true,
		re:    regexp.MustCompile(`\A(?:sk|pk|ak)[_-][a-z0-9_]{16,}`),
		gates: []string{"sk-", "sk_", "pk-", "pk_", "ak-", "ak_"},
	},
	{
		name: "api_key_named", certain: true, lowercase: true, anchored: true,
		re:    regexp.MustCompile(`\Aapi[_-]?key[_-]?[a-z0-9_]{20,}`),
		gates: []string{"api_key", "api-key", "apikey"},
	},
	{
		name: "aws_access_key", certain: true,
		re:    regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		gates: []string{"akia"},
	},
	{
		name: "private_key_block", certain: true,
		// Lazy dotall body up to the END marker (or end of input for a
		// truncated paste) so masking removes the key material, not
		// just the header. Typed-path only — deliberately NOT added to
		// the storage defaultPatterns in this commit, so stored-excerpt
		// behavior is unchanged (additive rule §17.6).
		re:    regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`),
		gates: []string{"private key-----"},
	},
	{
		name: "json_secret_value", certain: true, lowercase: true, anchored: true, anchorBack: 1,
		// `"password": "value"` and friends — the key context makes the
		// value certain. The value group excludes the quotes so the
		// mask marker stays inside a valid JSON string. Gates anchor at
		// the keyword; the match begins one byte earlier (the opening
		// quote — anchorBack), so `"input_tokens": 42` fails in O(1).
		re:         regexp.MustCompile(`\A"(?:password|secret|token|credential|api[_-]?key|auth[_-]?token)"\s*:\s*"([^"]+)"`),
		valueGroup: 1,
		gates:      []string{"password", "secret", "token", "credential", "api_key", "api-key", "apikey", "auth_token", "auth-token", "authtoken"},
	},
	{
		name: "env_secret_value", certain: true, lowercase: true, anchored: true, backScanKey: true,
		// Env-var-style quoted keys ("GITHUB_TOKEN": "..."): the
		// keyword may sit mid-key, so backScanKey walks back over
		// [a-z_] to the opening quote before the anchored attempt.
		re:         regexp.MustCompile(`\A"[a-z_]*(?:secret|token|password|credential|(?:api|access|private|master|encryption|signing|hmac)_key)[a-z_]*"\s*:\s*"([^"]+)"`),
		valueGroup: 1,
		gates:      []string{"secret", "token", "password", "credential", "_key"},
	},
	{
		name: "secret_assignment", certain: true, lowercase: true, anchored: true,
		// `password=...` / `token: ...` outside JSON (shell args, env
		// files). The {8,} floor keeps prose ("token: yes") quiet.
		re:         regexp.MustCompile(`\A(?:password|secret|credential|token)\s*[=:]\s*(\S{8,})`),
		valueGroup: 1,
		gates:      []string{"password", "secret", "credential", "token"},
	},
	{
		name: "api_key_assignment", certain: true, lowercase: true, anchored: true,
		re:         regexp.MustCompile(`\Aapi[_-]?key\s*[=:]\s*(\S{8,})`),
		valueGroup: 1,
		gates:      []string{"api_key", "api-key", "apikey"},
	},
	{
		name: "export_secret", certain: true, lowercase: true, anchored: true,
		re:         regexp.MustCompile(`\Aexport\s+\w*(?:secret|key|token|password|credential)\w*\s*=\s*(\S+)`),
		valueGroup: 1,
		gates:      []string{"export"},
	},
	{
		name: "connection_string_password", certain: true,
		re:         regexp.MustCompile(`://[^:/\s]+:([^@\s]+)@`),
		valueGroup: 1,
		gates:      []string{"://"},
	},
}

// typedMatch is one located finding with its span on the SHIELDED
// input (spans are internal — shielding changes offsets, so they are
// never exposed to callers).
type typedMatch struct {
	start, end int
	finding    TypedFinding
}

// DetectSecrets runs the typed detector table plus the
// context-gated entropy heuristic over v and returns the findings in
// span order. Fernet `encrypted_content` values are shielded first
// (the same mechanism Scrubber.String uses) so OpenAI ZDR reasoning
// blobs can't false-positive.
func DetectSecrets(v string) []TypedFinding {
	matches := findTyped(v)
	out := make([]TypedFinding, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.finding)
	}
	return out
}

// CertainSecretTypes returns the type names of pattern-certain
// findings in v, deduplicated, in first-seen order. This is the
// injectable detector for the R-172 shell-arg rule
// (policy.Config.SecretDetect) — entropy hits are excluded there by
// design: random-looking tokens are routine in shell args (commit
// SHAs, cache keys) and the heuristic would drown the rule.
func CertainSecretTypes(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range DetectSecrets(v) {
		if !f.Certain || seen[f.Type] {
			continue
		}
		seen[f.Type] = true
		out = append(out, f.Type)
	}
	return out
}

// MaskSecrets rewrites v with each finding for which shouldMask
// returns true replaced by "[REDACTED:<type>]", and returns the
// rewritten string plus ALL findings (masked or not). A nil shouldMask
// masks nothing (pure detection). Shield-aware like DetectSecrets:
// encrypted_content values survive byte-identical.
func MaskSecrets(v string, shouldMask func(TypedFinding) bool) (string, []TypedFinding) {
	shielded, restorations := shieldFernetEncryptedContent(v)
	matches := findTypedShielded(shielded)
	findings := make([]TypedFinding, 0, len(matches))
	for _, m := range matches {
		findings = append(findings, m.finding)
	}
	if shouldMask == nil {
		return v, findings
	}
	var b strings.Builder
	b.Grow(len(shielded))
	last := 0
	masked := false
	for _, m := range matches {
		if !shouldMask(m.finding) {
			continue
		}
		b.WriteString(shielded[last:m.start])
		b.WriteString("[REDACTED:" + m.finding.Type + "]")
		last = m.end
		masked = true
	}
	if !masked {
		return v, findings
	}
	b.WriteString(shielded[last:])
	return restoreShielded(b.String(), restorations), findings
}

// findTyped shields v and locates findings on the shielded form.
func findTyped(v string) []typedMatch {
	shielded, _ := shieldFernetEncryptedContent(v)
	return findTypedShielded(shielded)
}

// findTypedShielded walks the detector table over an already-shielded
// input, applies the entropy heuristic, sorts by span and drops
// overlaps (earlier table rows win — the table order tie-break).
// Lowercase rows match against the ASCII-lowered copy (computed once);
// values always extract from the original, case preserved.
func findTypedShielded(shielded string) []typedMatch {
	lower := asciiLower(shielded)
	var all []typedMatch
	for i := range typedDetectors {
		d := &typedDetectors[i]
		input := shielded
		if d.lowercase {
			input = lower
		}
		if d.anchored {
			all = append(all, anchoredMatches(d, input, shielded, lower)...)
			continue
		}
		if !gatesOpen(lower, d.gates) {
			continue
		}
		for _, idx := range d.re.FindAllStringSubmatchIndex(input, -1) {
			if m, ok := detectorMatch(d, shielded, idx, 0); ok {
				all = append(all, m)
			}
		}
	}
	all = append(all, entropyFindings(shielded, lower)...)
	// Stable sort: span order; ties keep table order (append order).
	sort.SliceStable(all, func(i, j int) bool { return all[i].start < all[j].start })
	out := all[:0]
	lastEnd := -1
	for _, m := range all {
		if m.start < lastEnd {
			continue // overlap: the earlier (certain) finding wins
		}
		out = append(out, m)
		lastEnd = m.end
	}
	return out
}

// asciiLower lowers ASCII letters only — length-preserving by
// construction (unlike strings.ToLower, whose Unicode folding can
// change byte length), so spans found on the lowered copy map 1:1
// onto the original. Returns the input unchanged (no allocation) when
// nothing needs lowering.
func asciiLower(s string) string {
	hasUpper := false
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			hasUpper = true
			break
		}
	}
	if !hasUpper {
		return s
	}
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if c := b[i]; c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// gatesOpen reports whether any gate literal appears in the lowercased
// input (no gates = always open).
func gatesOpen(lower string, gates []string) bool {
	if len(gates) == 0 {
		return true
	}
	for _, g := range gates {
		if strings.Contains(lower, g) {
			return true
		}
	}
	return false
}

// anchoredMatches runs one anchored detector at every gate-literal
// occurrence: the \A-prefixed regex attempts only from the anchor
// position, so cost is O(occurrences × match length) instead of one
// O(body) NFA scan per detector — the §17.9 fix for keyword
// patterns whose alternations defeat RE2's literal-prefix fast path.
func anchoredMatches(d *typedDetector, input, shielded, lower string) []typedMatch {
	var out []typedMatch
	for _, gate := range d.gates {
		from := 0
		for {
			i := strings.Index(lower[from:], gate)
			if i < 0 {
				break
			}
			pos := from + i
			from = pos + len(gate)
			start := pos - d.anchorBack
			if d.backScanKey {
				start = backScanQuotedKey(lower, pos)
			}
			if start < 0 {
				continue
			}
			idx := d.re.FindStringSubmatchIndex(input[start:])
			if idx == nil {
				continue
			}
			if m, ok := detectorMatch(d, shielded, idx, start); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// backScanQuotedKey walks back over [a-z_] from a keyword occurrence
// to the opening quote of a JSON key, returning the quote's offset
// (-1 when the shape doesn't hold within 64 bytes — keys are short).
func backScanQuotedKey(lower string, pos int) int {
	for i := pos - 1; i >= 0 && pos-i <= 64; i-- {
		c := lower[i]
		if c == '"' {
			return i
		}
		if (c < 'a' || c > 'z') && c != '_' {
			return -1
		}
	}
	return -1
}

// detectorMatch shapes one regex match (span index slice + base
// offset) into a typedMatch. ok=false when the value group is absent.
func detectorMatch(d *typedDetector, shielded string, idx []int, base int) (typedMatch, bool) {
	g := d.valueGroup * 2
	if g+1 >= len(idx) || idx[g] < 0 {
		return typedMatch{}, false
	}
	start, end := base+idx[g], base+idx[g+1]
	return typedMatch{
		start: start,
		end:   end,
		finding: TypedFinding{
			Type:    d.name,
			Certain: d.certain,
			Value:   shielded[start:end],
		},
	}, true
}

// Entropy heuristic (spec §8.2: "high-entropy heuristic gated by
// length+context to control FP rate"). A candidate token is examined
// ONLY inside a window around a secret-context word — never over the
// whole body — which both controls false positives (a git SHA in
// ordinary output has no secret context) and keeps the scan cheap
// (windows are rare and bounded).
const (
	// entropyWindow is the half-width of the context window scanned
	// around each context-word occurrence.
	entropyWindow = 200
	// entropyMinLen is the candidate length floor.
	entropyMinLen = 32
	// entropyMinBits is the Shannon entropy floor in bits/char.
	// Mixed-case base64 secrets sit near 5; hex SHAs near 3.9 — the
	// 4.2 floor plus the mixed-class requirement below keeps hex
	// digests and lowercase identifiers out.
	entropyMinBits = 4.2
)

// entropyContextWords are the lowercase context literals that open an
// entropy window. Table-driven; extend here.
var entropyContextWords = []string{
	"secret", "passwd", "password", "credential", "api_key", "api-key",
	"apikey", "access_key", "private_key", "bearer", "auth",
}

// isBase64Byte reports membership in the candidate alphabet
// (base64 + url-safe variants).
func isBase64Byte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '_' || c == '-'
}

// base64Runs yields the maximal alphabet runs of length ≥
// entropyMinLen in s as [start,end) spans. A manual scan — the
// equivalent counted-repeat regex ran at ~16MB/s on the NFA path and
// blew the §17.9 budget on context-word-dense bodies.
func base64Runs(s string, yield func(start, end int)) {
	runStart := -1
	for i := 0; i <= len(s); i++ {
		if i < len(s) && isBase64Byte(s[i]) {
			if runStart < 0 {
				runStart = i
			}
			continue
		}
		if runStart >= 0 && i-runStart >= entropyMinLen {
			yield(runStart, i)
		}
		runStart = -1
	}
}

// entropyFindings scans the context windows for high-entropy
// candidates. Findings are Certain=false — spec §8.2 keeps them out of
// mask/deny decisions by default.
//
// Window mechanics (§17.9): all context-word hits are collected
// first, their ±entropyWindow spans MERGED, and the candidate regex
// runs once per merged region — a body dense with context words
// (every "no secrets here" line opens one) degrades to a single
// full-body pass of a cheap character-class regex instead of
// thousands of overlapping window scans.
func entropyFindings(shielded, lower string) []typedMatch {
	regions := entropyRegions(lower, len(shielded))
	if len(regions) == 0 {
		return nil
	}
	var out []typedMatch
	for _, r := range regions {
		base64Runs(shielded[r[0]:r[1]], func(s, e int) {
			start, end := r[0]+s, r[0]+e
			tok := shielded[start:end]
			if !mixedClasses(tok) || shannonBits(tok) < entropyMinBits {
				return
			}
			out = append(out, typedMatch{
				start:   start,
				end:     end,
				finding: TypedFinding{Type: "entropy", Certain: false, Value: tok},
			})
		})
	}
	return out
}

// entropyRegions returns the merged, sorted ±entropyWindow spans
// around every context-word occurrence.
func entropyRegions(lower string, n int) [][2]int {
	var spans [][2]int
	for _, w := range entropyContextWords {
		from := 0
		for {
			i := strings.Index(lower[from:], w)
			if i < 0 {
				break
			}
			at := from + i
			lo, hi := at-entropyWindow, at+entropyWindow
			if lo < 0 {
				lo = 0
			}
			if hi > n {
				hi = n
			}
			spans = append(spans, [2]int{lo, hi})
			from = at + len(w)
		}
	}
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s[0] <= last[1] {
			if s[1] > last[1] {
				last[1] = s[1]
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// mixedClasses requires upper + lower + digit — the cheap pre-grade
// that rejects hex digests (no uppercase) and ALL-CAPS identifiers.
func mixedClasses(s string) bool {
	var hasUpper, hasLower, hasDigit bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= '0' && c <= '9':
			hasDigit = true
		}
	}
	return hasUpper && hasLower && hasDigit
}

// shannonBits computes the Shannon entropy of s in bits per byte.
func shannonBits(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
	}
	n := float64(len(s))
	var bits float64
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		bits -= p * math.Log2(p)
	}
	return bits
}
