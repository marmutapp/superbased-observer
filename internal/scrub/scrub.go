package scrub

import (
	"encoding/json"
	"regexp"
)

// MaxRawInputBytes is the cap applied to raw_tool_input after scrubbing.
// Spec §8.2 requires truncation to 2KB before storage.
const MaxRawInputBytes = 2048

// Redacted is the replacement string substituted in place of secret values.
const Redacted = "[REDACTED]"

// Scrubber applies a list of compiled redaction patterns to strings.
// The zero value is unusable — call New or NewWithExtra.
type Scrubber struct {
	patterns []pattern
}

type pattern struct {
	re      *regexp.Regexp
	replace string // If empty, the full match is replaced with Redacted.
	// If replace uses references like $1[REDACTED]$2, callers must include
	// the full template.
}

// sensitiveKeyRE matches JSON/map keys that should always have their string
// values scrubbed, regardless of content.
var sensitiveKeyRE = regexp.MustCompile(`(?i)(password|secret|token|credential|api[_-]?key|auth)|(^|[_-])key($|[_-])`)

var defaultPatterns = []pattern{
	// GitHub Personal Access Tokens: ghp_, gho_, ghu_, ghs_, ghr_ prefix.
	{re: regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)},
	// Bearer tokens
	{re: regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`)},
	// Common API key prefixes: sk-..., pk-..., ak-..., api_key=..., etc.
	// Allow underscores inside the body to catch forms like pk_test_abc123...
	{re: regexp.MustCompile(`(?i)(?:sk|pk|ak)[_-][A-Za-z0-9_]{16,}`)},
	{re: regexp.MustCompile(`(?i)api[_-]?key[_-]?[A-Za-z0-9_]{20,}`)},
	// AWS access key IDs
	{re: regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	// JSON-style sensitive key → replace the string value.
	// Matches: "password": "value" / "api_key":"value" / etc.
	{
		re:      regexp.MustCompile(`(?i)("(?:password|secret|token|credential|api[_-]?key|auth[_-]?token)"?\s*:\s*)"[^"]*"`),
		replace: `$1"` + Redacted + `"`,
	},
	{
		re:      regexp.MustCompile(`(?i)("[A-Z_]*(?:SECRET|KEY|TOKEN|PASSWORD|CREDENTIAL)[A-Z_]*"\s*:\s*)"[^"]*"`),
		replace: `$1"` + Redacted + `"`,
	},
	// Generic key=value / key: value secrets — redact the value after '=' or ':'.
	{
		re:      regexp.MustCompile(`(?i)(password|secret|token|credential)(\s*[=:]\s*)(\S+)`),
		replace: `$1$2` + Redacted,
	},
	// Explicit "api_key: value" / "api-key = value".
	{
		re:      regexp.MustCompile(`(?i)(api[_-]?key)(\s*[=:]\s*)(\S+)`),
		replace: `$1$2` + Redacted,
	},
	// export SECRET_FOO=bar → export SECRET_FOO=[REDACTED]
	{
		re:      regexp.MustCompile(`(?i)(export\s+\w*(?:SECRET|KEY|TOKEN|PASSWORD|CREDENTIAL)\w*\s*=\s*)(\S+)`),
		replace: `$1` + Redacted,
	},
	// Connection-string user:password@host → user:[REDACTED]@host
	{
		re:      regexp.MustCompile(`(://[^:/\s]+:)([^@\s]+)(@)`),
		replace: `$1` + Redacted + `$3`,
	},
}

// New returns a Scrubber loaded with the default patterns (spec §8.1).
func New() *Scrubber {
	return NewWithExtra(nil)
}

// NewWithExtra returns a Scrubber loaded with the default patterns plus any
// additional full-match regex patterns supplied by the user.
// Invalid regexes are silently skipped — callers should validate config with
// ValidatePatterns first if they want to surface errors.
func NewWithExtra(extra []string) *Scrubber {
	s := &Scrubber{patterns: append([]pattern{}, defaultPatterns...)}
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		s.patterns = append(s.patterns, pattern{re: re})
	}
	return s
}

// ValidatePatterns reports the first invalid regex in patterns, if any.
func ValidatePatterns(patterns []string) error {
	for _, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			return err
		}
	}
	return nil
}

// String applies every pattern to s, returning the scrubbed result.
// Full-match patterns replace their entire match with [REDACTED]; patterns
// with an explicit replace template use that template (supporting $1 $2 etc.).
func (s *Scrubber) String(v string) string {
	out := v
	for _, p := range s.patterns {
		if p.replace == "" {
			out = p.re.ReplaceAllString(out, Redacted)
		} else {
			out = p.re.ReplaceAllString(out, p.replace)
		}
	}
	return out
}

// JSONValue recursively scrubs every string value within an arbitrary
// unmarshaled JSON structure (map[string]any / []any / string / numbers).
// When traversing a map, any string value whose key name matches
// sensitiveKeyRE is replaced with [REDACTED] regardless of content.
// Non-string values are returned unchanged.
func (s *Scrubber) JSONValue(v any) any {
	return s.jsonValue(v, "")
}

func (s *Scrubber) jsonValue(v any, parentKey string) any {
	switch t := v.(type) {
	case string:
		if parentKey != "" && sensitiveKeyRE.MatchString(parentKey) {
			return Redacted
		}
		return s.String(t)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, child := range t {
			out[k] = s.jsonValue(child, k)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, child := range t {
			out[i] = s.jsonValue(child, parentKey)
		}
		return out
	default:
		return v
	}
}

// RawJSON scrubs every string value inside the supplied raw JSON payload and
// returns the re-marshaled, truncated result. If the payload is not valid
// JSON, it falls back to plain-string scrubbing. The output is always
// truncated to MaxRawInputBytes (spec §8.2).
func (s *Scrubber) RawJSON(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Truncate(s.String(string(raw)))
	}
	scrubbed := s.JSONValue(decoded)
	out, err := json.Marshal(scrubbed)
	if err != nil {
		return Truncate(s.String(string(raw)))
	}
	return Truncate(string(out))
}

// Truncate caps v at MaxRawInputBytes, appending an ellipsis marker if the
// value was cut.
func Truncate(v string) string {
	if len(v) <= MaxRawInputBytes {
		return v
	}
	const marker = "…[truncated]"
	cut := MaxRawInputBytes - len(marker)
	if cut < 0 {
		cut = 0
	}
	return v[:cut] + marker
}
