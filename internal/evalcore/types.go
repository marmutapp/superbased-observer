package evalcore

import (
	"context"
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

// JudgeClient is the host interface for the ONLY outbound network call this
// package can trigger: an explicitly-invoked llm_judge scorer. It is defined
// here, in the pure core, so both the node host (internal/obs) and the org
// server (internal/orgserver/orgeval) can implement it themselves without
// creating an import cycle. When no judge is wired, the llm_judge scorer is
// simply unavailable (Build errors) — code scorers run fully offline.
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
