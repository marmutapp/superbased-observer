package admission

import (
	"regexp"
	"strings"
)

// jailbreakHeuristics is a small, deliberately conservative set of
// prompt-injection / jailbreak markers used by the deterministic jailbreak
// pre-filter (spec §3 layer 2). It is NOT a security classifier (AD2: the
// deterministic layers carry what must never pass, but jailbreak defense is
// best served by a real classifier tier — P3). These catch the blatant cases
// cheaply; the LLM judge is the nuanced layer. Patterns are case-insensitive.
var jailbreakHeuristics = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore (all|any|the)? ?(previous|prior|above) (instructions|prompts?|rules?)`),
	regexp.MustCompile(`(?i)disregard (all|any|the)? ?(previous|prior|above)`),
	regexp.MustCompile(`(?i)\byou are now\b.{0,40}\b(dan|do anything now|unrestricted|jailbroken)\b`),
	regexp.MustCompile(`(?i)\b(reveal|print|show|repeat)\b.{0,40}\b(system prompt|initial instructions|your instructions)\b`),
	regexp.MustCompile(`(?i)\bpretend (to be|you are)\b.{0,40}\bno (restrictions|rules|limits)\b`),
	regexp.MustCompile(`(?i)\bdeveloper mode\b`),
}

// anyMatch reports whether text matches any of the compiled patterns.
func anyMatch(text string, pats []*regexp.Regexp) bool {
	for _, re := range pats {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// matchTopics reports which denied-topic term (if any) the text triggers.
// Topics are normalized lowercase substrings (Compile stripped any "label:"
// prefix), matched case-insensitively. Returns the matched term for the
// verdict reason, or "" if none matched.
func matchTopics(text string, c Criterion) string {
	lower := strings.ToLower(text)
	for _, term := range c.Topics {
		if strings.Contains(lower, term) {
			return term
		}
	}
	return ""
}
