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

// judgeVerdict is one per-criterion decision the rubric asks the judge to emit.
type judgeVerdict struct {
	Criterion string `json:"criterion"`
	Decision  string `json:"decision"`
	Reason    string `json:"reason"`
}

// judgeReply is the object wrapper the rubric asks for: one verdict per
// criterion, each echoing the criterion id (admission spec §4). The per-
// criterion structure + the explicit id echo eliminate the criterion
// MISATTRIBUTION seen on small judge models (gap-audit §5.2 — a single numbered
// block let e.g. qwen2.5:1.5b pin a verdict to the wrong criterion).
type judgeReply struct {
	Verdicts []judgeVerdict `json:"verdicts"`
}

// buildJudgePrompt compiles the judged criteria into a per-criterion rubric
// (admission spec §4; gap-audit §5.2): each criterion is its own section with
// an explicit id, its plain-English definition, and a DECISION HINT (the action
// to emit when that criterion is violated), and the judge must return one
// verdict per criterion echoing the id. request is already scope-resolved by
// the caller (last user message or a rendered conversation).
func buildJudgePrompt(request string, judged []Criterion) string {
	var b strings.Builder
	b.WriteString("You are an input-admission policy checker for an application. ")
	b.WriteString("Evaluate the user request below against EACH policy criterion INDEPENDENTLY.\n\n")
	b.WriteString("For every criterion: if the request does NOT violate it, choose \"allow\"; ")
	b.WriteString("if it DOES violate it, use the decision noted for that criterion.\n\n")
	b.WriteString("Criteria:\n")
	for _, c := range judged {
		name := c.Name
		if name == "" {
			name = c.ID
		}
		fmt.Fprintf(&b, "\n- id: %s\n", c.ID)
		fmt.Fprintf(&b, "  name: %s\n", name)
		def := strings.TrimSpace(c.Definition)
		if def != "" {
			b.WriteString("  definition:\n")
			for _, line := range strings.Split(def, "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					fmt.Fprintf(&b, "    %s\n", line)
				}
			}
		}
		fmt.Fprintf(&b, "  if_violated_respond: %q\n", c.Decision.String())
	}
	b.WriteString("\nUser request:\n\"\"\"\n")
	b.WriteString(request)
	b.WriteString("\n\"\"\"\n\n")
	b.WriteString("Respond with ONLY a JSON object listing one verdict per criterion, ")
	b.WriteString("echoing each criterion id EXACTLY as written above:\n")
	b.WriteString(`{"verdicts":[{"criterion":"<id>","decision":"allow|flag|ask|deny","reason":"<one short sentence, empty when allow>"}]}`)
	b.WriteString("\nInclude exactly one entry for every criterion id listed. ")
	b.WriteString("Choose \"allow\" for any criterion the request satisfies.")
	return b.String()
}

// parseJudgeVerdicts extracts the per-criterion verdicts from the judge's raw
// reply, tolerating markdown code fences, leading/trailing prose, and three
// shapes the model may emit: the requested {"verdicts":[…]} object, a bare
// [{…}] array, and the LEGACY single {"decision":…,"criterion":…} object (so a
// model that answers with one verdict — or an older prompt cache — still
// parses). ok=false when no structured verdict can be found at all, so the
// caller applies the judge fail-mode.
func parseJudgeVerdicts(raw string) ([]judgeVerdict, bool) {
	s := stripCodeFence(raw)

	if obj, ok := extractDelimited(s, '{', '}'); ok {
		// Preferred shape: {"verdicts":[…]}.
		var w judgeReply
		if err := json.Unmarshal(obj, &w); err == nil && len(w.Verdicts) > 0 {
			return w.Verdicts, true
		}
		// Legacy shape: a single {"decision":…,"criterion":…,"reason":…} object.
		var v judgeVerdict
		if err := json.Unmarshal(obj, &v); err == nil && (v.Decision != "" || v.Criterion != "") {
			return []judgeVerdict{v}, true
		}
	}
	// Bare array: [{…},{…}].
	if arr, ok := extractDelimited(s, '[', ']'); ok {
		var vs []judgeVerdict
		if err := json.Unmarshal(arr, &vs); err == nil && len(vs) > 0 {
			return vs, true
		}
	}
	return nil, false
}

// extractDelimited returns the substring from the first open byte to the last
// close byte (inclusive), tolerating prose around the JSON. ok=false when no
// such span exists.
func extractDelimited(s string, openB, closeB byte) ([]byte, bool) {
	start := strings.IndexByte(s, openB)
	end := strings.LastIndexByte(s, closeB)
	if start < 0 || end <= start {
		return nil, false
	}
	return []byte(s[start : end+1]), true
}

// judgeOne renders the rubric for one chunk of content, calls the judge, and
// reduces its per-criterion verdicts into a single Result. On a judge
// error/timeout or an unreadable reply it applies the policy fail-mode (strict
// → Deny; else fail-open Allow, both Degraded=timeout-failopen).
func judgeOne(ctx context.Context, text string, judged []Criterion, p PolicySpec, judge JudgeClient) Result {
	prompt := buildJudgePrompt(text, judged)
	raw, err := judge.Judge(ctx, prompt)
	if err != nil {
		return judgeFailMode(p, "Policy check unavailable.")
	}
	verdicts, ok := parseJudgeVerdicts(raw)
	if !ok {
		return judgeFailMode(p, "Policy check returned an unreadable verdict.")
	}
	res, ok := reduceVerdicts(verdicts, judged)
	if !ok {
		return judgeFailMode(p, "Policy check returned an unreadable verdict.")
	}
	return res
}

// reduceVerdicts folds the judge's per-criterion verdicts into one Result,
// STRICTEST-WINS: the strongest decision (deny > ask > flag > allow) wins and is
// attributed to the criterion that produced it (the echoed id validated against
// the judged set; an unknown/empty id falls back to the strictest judged
// criterion, whose severity is applied). All verdicts "allow" → a clean allow.
// ok=false when NO verdict carried a readable decision (the reply was
// structurally present but semantically empty), so the caller fail-modes.
func reduceVerdicts(verdicts []judgeVerdict, judged []Criterion) (Result, bool) {
	best := DecisionAllow
	var winner judgeVerdict
	found := false
	for _, v := range verdicts {
		dec, ok := ParseDecision(v.Decision)
		if !ok {
			continue
		}
		if !found || dec > best {
			best, winner, found = dec, v, true
		}
	}
	if !found {
		return Result{}, false
	}
	if best == DecisionAllow {
		return Result{Decision: DecisionAllow, Severity: SeverityInfo, JudgeUsed: true}, true
	}
	c := attributeJudged(winner.Criterion, judged)
	reason := strings.TrimSpace(winner.Reason)
	if reason == "" {
		reason = "Request is outside the allowed policy."
	}
	return Result{Decision: best, Severity: c.Severity, Criterion: c.ID, Reason: reason, JudgeUsed: true}, true
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
