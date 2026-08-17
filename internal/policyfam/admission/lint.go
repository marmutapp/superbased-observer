package admission

import (
	"fmt"
	"regexp"
	"strings"
)

// Issue is one policy-lint finding. Fatal issues make a policy invalid (lint
// exits non-zero); non-fatal issues are advisory.
type Issue struct {
	CriterionID string
	Message     string
	Fatal       bool
}

// Lint statically checks a PolicyInput and returns findings (admission spec §8
// `admission lint`). It catches the mistakes that would otherwise surface at
// request time or silently no-op: unknown decision/severity/type, malformed
// prefilter regex, judged criteria with no definition, deterministic criteria
// with no matchable content, duplicate ids, and an empty policy. A non-empty
// Fatal set means the policy must not load.
func Lint(in PolicyInput) []Issue {
	var issues []Issue
	seen := map[string]bool{}

	if len(in.Criteria) == 0 && len(in.Prefilter.Allow) == 0 && len(in.Prefilter.Deny) == 0 {
		issues = append(issues, Issue{Message: "policy has no criteria and no prefilter rules — it admits everything", Fatal: false})
	}

	if _, ok := ParseDecision(in.SecretRemoteJudge); !ok {
		issues = append(issues, Issue{Message: fmt.Sprintf("unknown secret_remote_judge decision %q (want allow|flag|ask|deny)", in.SecretRemoteJudge), Fatal: true})
	}

	for _, p := range in.Prefilter.Allow {
		if _, err := regexp.Compile(p); err != nil {
			issues = append(issues, Issue{Message: fmt.Sprintf("prefilter.allow: bad regex %q: %v", p, err), Fatal: true})
		}
	}
	for _, p := range in.Prefilter.Deny {
		if _, err := regexp.Compile(p); err != nil {
			issues = append(issues, Issue{Message: fmt.Sprintf("prefilter.deny: bad regex %q: %v", p, err), Fatal: true})
		}
	}

	for _, c := range in.Criteria {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			issues = append(issues, Issue{Message: "criterion with empty id", Fatal: true})
		} else if seen[id] {
			issues = append(issues, Issue{CriterionID: id, Message: "duplicate criterion id", Fatal: true})
		}
		seen[id] = true

		if _, ok := ParseDecision(c.Decision); !ok {
			issues = append(issues, Issue{CriterionID: id, Message: fmt.Sprintf("unknown decision %q", c.Decision), Fatal: true})
		}
		if _, ok := ParseSeverity(c.Severity); !ok {
			issues = append(issues, Issue{CriterionID: id, Message: fmt.Sprintf("unknown severity %q", c.Severity), Fatal: true})
		}

		switch CriterionType(strings.TrimSpace(c.Type)) {
		case TypeValidUseCase, TypeCustom:
			if strings.TrimSpace(c.Definition) == "" {
				issues = append(issues, Issue{CriterionID: id, Message: "judged criterion has an empty definition — the judge has nothing to check", Fatal: true})
			}
		case TypeDeniedTopics:
			if len(NormalizeTopics(c.Topics)) == 0 {
				issues = append(issues, Issue{CriterionID: id, Message: "denied_topics criterion has no topics", Fatal: true})
			}
		case TypeJailbreak:
			// deterministic heuristic — no author content required.
		default:
			issues = append(issues, Issue{CriterionID: id, Message: fmt.Sprintf("unknown criterion type %q (want valid_use_case|denied_topics|jailbreak|custom)", c.Type), Fatal: true})
		}
	}
	return issues
}

// HasFatal reports whether any lint issue is fatal.
func HasFatal(issues []Issue) bool {
	for _, i := range issues {
		if i.Fatal {
			return true
		}
	}
	return false
}
