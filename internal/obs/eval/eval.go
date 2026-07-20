// Package eval is the pure-logic core of the obs minimal eval plane (plan §8):
// a table-driven scorer registry (built-in deterministic code scorers + an
// LLM-as-judge scorer over an injected client) and a runner that scores a set
// of samples. It is PURE — no database/sql, net/http, or fsnotify (pinned by
// imports_test.go): persistence is the obs/store seam's job, and the judge's
// network call is the host's, reached only through the injected JudgeClient
// interface so this package never imports the proxy. Scorers are a data table
// walked by name (CLAUDE.md rule #5), so a new scorer is one registry row.
package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Sample is one unit to score: the span/trace identity, its content (input /
// output / reference, populated by the store seam from obs_span_content +
// dataset items, honoring the ContentGate), and content-free facts. A code
// scorer reads whichever fields it needs; a judge reads the text.
type Sample struct {
	ItemID    int64
	SpanID    string
	TraceID   string
	Input     string
	Output    string
	Reference string
	Facts     SpanFacts
}

// SpanFacts is the content-free side of a sample — always available even when
// raw bodies are gated off, so the facts-based scorers (status/latency/cost)
// work on any node.
type SpanFacts struct {
	Status       string
	DurationMS   int64
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Model        string
}

// Score is one scorer's verdict on one sample. Score is normalized to [0,1];
// Passed is the boolean gate the CI regression check aggregates.
type Score struct {
	Scorer    string
	Score     float64
	Passed    bool
	Rationale string
}

// Result ties a Score back to the sample it scored, for persistence.
type Result struct {
	ItemID int64
	SpanID string
	Score  Score
}

// Scorer scores a single sample. Implementations are built by the registry
// from a Spec; they are stateless and safe to reuse across a run.
type Scorer interface {
	Name() string
	Score(ctx context.Context, s Sample) (Score, error)
}

// Spec names a scorer and its parameters, as parsed from config / CLI (e.g.
// {Name:"latency_under", Params:{"ms":"2000"}}). The registry resolves it to a
// Scorer instance.
type Spec struct {
	Name   string
	Params map[string]string
}

// Run scores every sample with every scorer, returning one Result per
// (sample, scorer). A scorer error fails CLOSED — it becomes a Passed=false
// Result with the error in the rationale, so a broken scorer is visible and
// never silently drops a sample. Order is stable (sample-major, scorer-minor).
func Run(ctx context.Context, samples []Sample, scorers []Scorer) []Result {
	out := make([]Result, 0, len(samples)*len(scorers))
	for _, s := range samples {
		for _, sc := range scorers {
			res, err := sc.Score(ctx, s)
			if err != nil {
				res = Score{Scorer: sc.Name(), Score: 0, Passed: false, Rationale: "error: " + err.Error()}
			}
			if res.Scorer == "" {
				res.Scorer = sc.Name()
			}
			out = append(out, Result{ItemID: s.ItemID, SpanID: s.SpanID, Score: res})
		}
	}
	return out
}

// Summary is the aggregate of a run: counts + mean score, used for the run row
// and the CI --fail-under gate.
type Summary struct {
	Total     int
	Passed    int
	MeanScore float64
}

// Summarize aggregates results. Mean is over all results; PassRate is
// Passed/Total. An empty input yields the zero Summary.
func Summarize(results []Result) Summary {
	if len(results) == 0 {
		return Summary{}
	}
	var sum float64
	passed := 0
	for _, r := range results {
		sum += r.Score.Score
		if r.Score.Passed {
			passed++
		}
	}
	return Summary{Total: len(results), Passed: passed, MeanScore: sum / float64(len(results))}
}

// PassRate is Passed/Total, or 0 when Total is 0.
func (s Summary) PassRate() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Passed) / float64(s.Total)
}

// BuildAll resolves every Spec to a Scorer via the registry, preserving order.
// A single unknown/invalid spec fails the whole build (a typo'd scorer name
// should be loud, not silently skipped).
func BuildAll(specs []Spec, judge JudgeClient) ([]Scorer, error) {
	out := make([]Scorer, 0, len(specs))
	for _, sp := range specs {
		sc, err := Build(sp, judge)
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// Build resolves one Spec to a Scorer. Unknown names return an error listing
// the available scorers.
func Build(spec Spec, judge JudgeClient) (Scorer, error) {
	b, ok := registry[spec.Name]
	if !ok {
		return nil, fmt.Errorf("eval.Build: unknown scorer %q (available: %v)", spec.Name, Names())
	}
	return b(spec.Params, judge)
}

// ParseSpec parses a scorer spec string of the form "name" or
// "name:key=val,key2=val2" (the CLI + config surface). Whitespace around tokens
// is trimmed; an empty string is an error.
func ParseSpec(s string) (Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Spec{}, fmt.Errorf("eval.ParseSpec: empty spec")
	}
	name, rest, hasParams := strings.Cut(s, ":")
	spec := Spec{Name: strings.TrimSpace(name)}
	if spec.Name == "" {
		return Spec{}, fmt.Errorf("eval.ParseSpec: empty scorer name in %q", s)
	}
	if !hasParams {
		return spec, nil
	}
	spec.Params = map[string]string{}
	for _, kv := range strings.Split(rest, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return Spec{}, fmt.Errorf("eval.ParseSpec: bad param %q in %q (want key=val)", kv, s)
		}
		spec.Params[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return spec, nil
}

// ParseSpecs parses a list of spec strings.
func ParseSpecs(ss []string) ([]Spec, error) {
	out := make([]Spec, 0, len(ss))
	for _, s := range ss {
		spec, err := ParseSpec(s)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, nil
}

// Names returns the registered scorer names, sorted — for error messages and
// the CLI's discovery output.
func Names() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
