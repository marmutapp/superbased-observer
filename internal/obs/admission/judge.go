package admission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// JudgeClient is admission's OWN judge seam (admission spec §5). It is a
// distinct, tiny interface — NOT internal/obs/eval.JudgeClient — so this pure
// package never imports eval; the host binds BOTH to the same underlying
// chatCompletionsJudge at the obs wiring point (one host judge implementation,
// two consumers — a wiring change, not a new host interface, §14 Q4).
//
// Judge sends the fully-rendered rubric prompt and returns the model's raw
// text reply. The bounded timeout + fail-mode are the caller's (admissionsvc
// sets the context deadline; the pipeline applies strict/fail-open on error).
type JudgeClient interface {
	Judge(ctx context.Context, prompt string) (string, error)
}

// judgeVerdict is the structured reply the rubric asks the judge to emit.
type judgeVerdict struct {
	Decision  string `json:"decision"`
	Criterion string `json:"criterion"`
	Reason    string `json:"reason"`
}

// buildJudgePrompt compiles the judged criteria into a NeMo-`self_check_input`-
// shaped rubric (admission spec §4): the admin's plain-English definitions
// become a numbered policy list, the request is appended, and the judge is
// asked for a single JSON verdict. request is already scope-resolved by the
// caller (last user message or a rendered conversation).
func buildJudgePrompt(request string, judged []Criterion) string {
	var b strings.Builder
	b.WriteString("You are an input-admission policy checker for an application. ")
	b.WriteString("Decide whether the user request below should be admitted, given the policy.\n\n")
	b.WriteString("Policy criteria:\n")
	for i, c := range judged {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, c.ID, name)
		def := strings.TrimSpace(c.Definition)
		if def != "" {
			for _, line := range strings.Split(def, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					fmt.Fprintf(&b, "   %s\n", line)
				}
			}
		}
	}
	b.WriteString("\nUser request:\n\"\"\"\n")
	b.WriteString(request)
	b.WriteString("\n\"\"\"\n\n")
	b.WriteString(`Respond with ONLY a JSON object: {"decision":"allow|flag|ask|deny","criterion":"<the matching criterion id, or empty>","reason":"<one short sentence for the end user>"}.`)
	b.WriteString(" Choose \"allow\" if the request is acceptable under every criterion.")
	return b.String()
}

// parseJudgeVerdict extracts the JSON verdict from the judge's raw reply,
// tolerating markdown code fences and leading/trailing prose. It clamps the
// decision to the recognized vocabulary; an unparseable or unknown reply
// yields ok=false so the caller can treat it as a judge error (fail-mode).
func parseJudgeVerdict(raw string) (judgeVerdict, bool) {
	s := stripCodeFence(raw)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return judgeVerdict{}, false
	}
	var v judgeVerdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return judgeVerdict{}, false
	}
	if _, ok := ParseDecision(v.Decision); !ok {
		return judgeVerdict{}, false
	}
	return v, true
}

// stripCodeFence removes a leading ```lang / trailing ``` fence if present.
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
