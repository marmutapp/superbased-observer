package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// JudgeClient is the FOURTH host interface (after TurnSink / ProxyEnricher /
// ContentGate in internal/obs/interfaces.go — see the note there). It is
// defined HERE, in the pure eval package, rather than in obs root, solely to
// avoid an import cycle: the obs-root orchestrator imports eval, so eval cannot
// import obs root. The host implements it at the wiring point; a judge call is
// the ONLY outbound network in the whole subsystem, made only for an
// explicitly-invoked eval run, tagged sbo.eval=true so the host can exclude it
// from app spend (plan §15 Q4). When no judge is wired, the llm_judge scorer
// is simply unavailable (Build errors) — code scorers run fully offline.
type JudgeClient interface {
	Judge(ctx context.Context, req JudgeRequest) (JudgeResponse, error)
}

// JudgeRequest is one judge invocation: the model id and the fully-rendered
// prompt (the scorer does the template substitution). Kept minimal and
// content-explicit so the host binding is trivial and auditable.
type JudgeRequest struct {
	Model  string
	Prompt string
}

// JudgeResponse is the judge model's raw text reply; the scorer parses the
// score out of it.
type JudgeResponse struct {
	Text string
}

// builder constructs a Scorer from its params (+ the judge for llm_judge).
type builder func(params map[string]string, judge JudgeClient) (Scorer, error)

// registry is the table-driven scorer catalog (CLAUDE.md rule #5). Every entry
// is a deterministic, node-local check except llm_judge. A new scorer is one
// row here + its builder — no switch grows.
var registry = map[string]builder{
	"exact_match":   buildExactMatch,
	"contains":      buildContains,
	"icontains":     buildIContains,
	"regex_match":   buildRegexMatch,
	"json_valid":    buildJSONValid,
	"non_empty":     buildNonEmpty,
	"status_ok":     buildStatusOK,
	"latency_under": buildLatencyUnder,
	"cost_under":    buildCostUnder,
	"llm_judge":     buildLLMJudge,
}

// fn is a code scorer's pure body: it returns a normalized score, pass, and a
// short rationale for one sample.
type fn func(s Sample) (score float64, passed bool, rationale string)

// codeScorer adapts a pure fn into a Scorer.
type codeScorer struct {
	name string
	body fn
}

func (c codeScorer) Name() string { return c.name }

func (c codeScorer) Score(_ context.Context, s Sample) (Score, error) {
	sc, passed, why := c.body(s)
	return Score{Scorer: c.name, Score: sc, Passed: passed, Rationale: why}, nil
}

func boolScore(pass bool, whyTrue, whyFalse string) (float64, bool, string) {
	if pass {
		return 1, true, whyTrue
	}
	return 0, false, whyFalse
}

// reference resolves the comparison target: an explicit "value" param wins,
// else the sample's Reference. This lets a scorer be pinned in config
// (contains value=foo) OR compare against a per-item reference.
func reference(params map[string]string, s Sample) string {
	if v, ok := params["value"]; ok {
		return v
	}
	return s.Reference
}

func buildExactMatch(_ map[string]string, _ JudgeClient) (Scorer, error) {
	return codeScorer{"exact_match", func(s Sample) (float64, bool, string) {
		return boolScore(s.Output == s.Reference, "output == reference", "output != reference")
	}}, nil
}

func buildContains(params map[string]string, _ JudgeClient) (Scorer, error) {
	return codeScorer{"contains", func(s Sample) (float64, bool, string) {
		want := reference(params, s)
		return boolScore(want != "" && strings.Contains(s.Output, want),
			"output contains expected substring", "output missing expected substring")
	}}, nil
}

func buildIContains(params map[string]string, _ JudgeClient) (Scorer, error) {
	return codeScorer{"icontains", func(s Sample) (float64, bool, string) {
		want := reference(params, s)
		return boolScore(want != "" && strings.Contains(strings.ToLower(s.Output), strings.ToLower(want)),
			"output contains expected substring (ci)", "output missing expected substring (ci)")
	}}, nil
}

func buildRegexMatch(params map[string]string, _ JudgeClient) (Scorer, error) {
	pat := params["pattern"]
	if pat == "" {
		return nil, fmt.Errorf("eval: regex_match requires a 'pattern' param")
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("eval: regex_match bad pattern: %w", err)
	}
	return codeScorer{"regex_match", func(s Sample) (float64, bool, string) {
		return boolScore(re.MatchString(s.Output), "output matches /"+pat+"/", "output does not match /"+pat+"/")
	}}, nil
}

func buildJSONValid(_ map[string]string, _ JudgeClient) (Scorer, error) {
	return codeScorer{"json_valid", func(s Sample) (float64, bool, string) {
		return boolScore(json.Valid([]byte(s.Output)), "output is valid JSON", "output is not valid JSON")
	}}, nil
}

func buildNonEmpty(_ map[string]string, _ JudgeClient) (Scorer, error) {
	return codeScorer{"non_empty", func(s Sample) (float64, bool, string) {
		return boolScore(strings.TrimSpace(s.Output) != "", "output non-empty", "output empty")
	}}, nil
}

func buildStatusOK(_ map[string]string, _ JudgeClient) (Scorer, error) {
	return codeScorer{"status_ok", func(s Sample) (float64, bool, string) {
		return boolScore(s.Facts.Status == "ok", "span status ok", "span status "+s.Facts.Status)
	}}, nil
}

func buildLatencyUnder(params map[string]string, _ JudgeClient) (Scorer, error) {
	ms, err := strconv.ParseInt(params["ms"], 10, 64)
	if err != nil || ms <= 0 {
		return nil, fmt.Errorf("eval: latency_under requires a positive 'ms' param")
	}
	return codeScorer{"latency_under", func(s Sample) (float64, bool, string) {
		return boolScore(s.Facts.DurationMS > 0 && s.Facts.DurationMS <= ms,
			fmt.Sprintf("%dms <= %dms", s.Facts.DurationMS, ms),
			fmt.Sprintf("%dms > %dms", s.Facts.DurationMS, ms))
	}}, nil
}

func buildCostUnder(params map[string]string, _ JudgeClient) (Scorer, error) {
	usd, err := strconv.ParseFloat(params["usd"], 64)
	if err != nil || usd <= 0 {
		return nil, fmt.Errorf("eval: cost_under requires a positive 'usd' param")
	}
	return codeScorer{"cost_under", func(s Sample) (float64, bool, string) {
		return boolScore(s.Facts.CostUSD <= usd,
			fmt.Sprintf("$%.4f <= $%.4f", s.Facts.CostUSD, usd),
			fmt.Sprintf("$%.4f > $%.4f", s.Facts.CostUSD, usd))
	}}, nil
}

// judgeScorer is the LLM-as-judge scorer: it renders {{input}}/{{output}}/
// {{reference}} into the prompt, asks the injected judge, and parses a numeric
// score from the reply. passed = score >= threshold.
type judgeScorer struct {
	model     string
	prompt    string
	threshold float64
	judge     JudgeClient
}

func (j judgeScorer) Name() string { return "llm_judge" }

func (j judgeScorer) Score(ctx context.Context, s Sample) (Score, error) {
	prompt := strings.NewReplacer(
		"{{input}}", s.Input,
		"{{output}}", s.Output,
		"{{reference}}", s.Reference,
	).Replace(j.prompt)
	resp, err := j.judge.Judge(ctx, JudgeRequest{Model: j.model, Prompt: prompt})
	if err != nil {
		return Score{}, fmt.Errorf("eval: llm_judge call failed: %w", err)
	}
	score, rationale := parseJudgeReply(resp.Text)
	return Score{Scorer: "llm_judge", Score: score, Passed: score >= j.threshold, Rationale: rationale}, nil
}

func buildLLMJudge(params map[string]string, judge JudgeClient) (Scorer, error) {
	if judge == nil {
		return nil, fmt.Errorf("eval: llm_judge requires a judge client (none wired — set [observability.eval] judge_model and bind a host JudgeClient)")
	}
	prompt := params["prompt"]
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("eval: llm_judge requires a 'prompt' param (may use {{input}}/{{output}}/{{reference}})")
	}
	threshold := 0.5
	if t, ok := params["threshold"]; ok {
		v, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil, fmt.Errorf("eval: llm_judge bad threshold: %w", err)
		}
		threshold = v
	}
	return judgeScorer{model: params["model"], prompt: prompt, threshold: threshold, judge: judge}, nil
}

// parseJudgeReply extracts a [0,1] score from a judge reply. It accepts a JSON
// object with a "score" field (the structured-output contract we ask judges
// for), falling back to the first float found in the text; an out-of-range or
// absent number yields 0 with the raw reply as rationale.
func parseJudgeReply(text string) (float64, string) {
	trimmed := strings.TrimSpace(text)
	var obj struct {
		Score     float64 `json:"score"`
		Rationale string  `json:"rationale"`
	}
	if json.Unmarshal([]byte(trimmed), &obj) == nil && (obj.Score != 0 || strings.Contains(trimmed, "score")) {
		return clamp01(obj.Score), firstNonEmpty(obj.Rationale, trimmed)
	}
	if f, ok := firstFloat(trimmed); ok {
		return clamp01(f), trimmed
	}
	return 0, "unparseable judge reply: " + truncate(trimmed, 200)
}

var floatRe = regexp.MustCompile(`-?\d+(\.\d+)?`)

func firstFloat(s string) (float64, bool) {
	m := floatRe.FindString(s)
	if m == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func clamp01(f float64) float64 {
	switch {
	case f < 0:
		return 0
	case f > 1:
		return 1
	default:
		return f
	}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
