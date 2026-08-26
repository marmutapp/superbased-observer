package arena

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/integration"
	"github.com/marmutapp/superbased-observer/internal/models"
)

// judge.go — the fixed global rubric (operator ruling 2026-08-22): the
// chosen judge harness grades every candidate patch against the original
// prompt and MUST return the rubric JSON. Parse failures surface as an
// honest "unparseable" verdict — scores are never fabricated.

// MaxJudgePatchBytes caps how much of a candidate's patch is embedded in
// a judge prompt. Beyond this the judge sees the head plus a truncation
// marker (the full patch stays on disk for the operator).
const MaxJudgePatchBytes = 48 * 1024

// judgeRubricTemplate is the versioned prompt contract. Changing it is a
// deliberate act: scores are only comparable within one template version.
const judgeRubricTemplate = `You are a strict code-review judge for an "agent arena": several AI coding agents were given the SAME task in isolated copies of a repository, and you must grade ONE candidate's diff.

ORIGINAL TASK:
<task>
%s
</task>

CANDIDATE DIFF (git patch against the base commit):
<patch>
%s
</patch>

Grade ONLY what the diff shows. Score each dimension 0-10 (higher is better EXCEPT risk, where higher means RISKIER):
- correctness: does the diff implement the task without bugs?
- completeness: how much of the task is covered?
- code_quality: structure, naming, idioms, tests included?
- performance: obvious efficiency issues or wins?
- risk: chance this change breaks something else or causes harm (higher = riskier)?
- overall: your single weighted verdict of whether to keep this candidate.

Respond with STRICT JSON ONLY (no prose, no markdown fences), exactly:
{"correctness":N,"completeness":N,"code_quality":N,"performance":N,"risk":N,"overall":N,"verdict_rationale":"one short paragraph"}`

// renderJudgePrompt builds the judge prompt for one candidate.
func renderJudgePrompt(task, patch string) string {
	if len(patch) > MaxJudgePatchBytes {
		patch = patch[:MaxJudgePatchBytes] + "\n… [diff truncated at " +
			fmt.Sprint(MaxJudgePatchBytes) + " bytes; full patch on disk]"
	}
	return fmt.Sprintf(judgeRubricTemplate, task, patch)
}

// judgeCandidate drives the judge harness once and parses its rubric JSON
// out of the final answer (tolerating markdown fences around it). A parse
// failure returns nil scores with an explanatory verdict — never guessed
// numbers.
func judgeCandidate(ctx context.Context, req driveRequest, spec *integration.HeadlessSpec) (*models.JudgeScores, string, error) {
	bin := driveBinOverrides[req.Tool]
	if bin == "" {
		var err error
		bin, err = resolveToolBinary(req.Tool)
		if err != nil {
			return nil, "", fmt.Errorf("arena.judgeCandidate: %w", err)
		}
	}
	res, err := executeDrive(ctx, bin, spec, req)
	if err != nil {
		return nil, "", fmt.Errorf("arena.judgeCandidate: %w", err)
	}
	scores, perr := parseJudgeScores(res.FinalAnswer)
	if perr != nil {
		return nil, "judge output unparseable: " + truncateStr(stripANSI(res.FinalAnswer), 400), nil
	}
	return scores, scores.VerdictRationale, nil
}

// parseJudgeScores extracts and range-checks the rubric JSON from a judge
// answer. Every field must be present and within 0..10.
func parseJudgeScores(answer string) (*models.JudgeScores, error) {
	cleaned := strings.TrimSpace(answer)
	cleaned = strings.TrimPrefix(cleaned, "```json")
	cleaned = strings.TrimPrefix(cleaned, "```")
	cleaned = strings.TrimSuffix(cleaned, "```")
	start := strings.Index(cleaned, "{")
	end := strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found")
	}
	var raw struct {
		Correctness      *int   `json:"correctness"`
		Completeness     *int   `json:"completeness"`
		CodeQuality      *int   `json:"code_quality"`
		Performance      *int   `json:"performance"`
		Risk             *int   `json:"risk"`
		Overall          *int   `json:"overall"`
		VerdictRationale string `json:"verdict_rationale"`
	}
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("judge JSON: %w", err)
	}
	pick := func(n *int, name string) (int, error) {
		if n == nil {
			return 0, fmt.Errorf("missing %s", name)
		}
		if *n < 0 || *n > 10 {
			return 0, fmt.Errorf("%s out of range: %d", name, *n)
		}
		return *n, nil
	}
	out := &models.JudgeScores{VerdictRationale: raw.VerdictRationale}
	for _, f := range []struct {
		n    *int
		name string
		dst  *int
	}{
		{raw.Correctness, "correctness", &out.Correctness},
		{raw.Completeness, "completeness", &out.Completeness},
		{raw.CodeQuality, "code_quality", &out.CodeQuality},
		{raw.Performance, "performance", &out.Performance},
		{raw.Risk, "risk", &out.Risk},
		{raw.Overall, "overall", &out.Overall},
	} {
		v, err := pick(f.n, f.name)
		if err != nil {
			return nil, err
		}
		*f.dst = v
	}
	return out, nil
}
