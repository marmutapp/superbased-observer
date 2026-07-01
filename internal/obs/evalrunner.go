package obs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/marmutapp/superbased-observer/internal/obs/eval"
	"github.com/marmutapp/superbased-observer/internal/obs/span"
	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// EvalRunner is the obs-root orchestrator for the minimal eval plane (plan
// §8): it bridges the pure scorer core (internal/obs/eval) and the persistence
// seam (internal/obs/store) — load samples, build scorers, run, persist scores
// + the run summary. It does I/O (allowed in obs root), but the scoring logic
// stays pure in eval and the SQL stays in obs/store; this layer only maps
// between them (SampleRow → eval.Sample, eval.Result → ScoreRow). The judge is
// the injected eval.JudgeClient (may be nil → llm_judge unavailable).
type EvalRunner struct {
	store  *obsstore.Store
	judge  eval.JudgeClient
	gate   ContentGate
	logger *slog.Logger
}

// NewEvalRunner builds the orchestrator. judge may be nil (code scorers still
// run; llm_judge fails to build with a clear message). gate may be nil →
// treated as metadata-only (no raw bodies snapshotted into datasets).
func NewEvalRunner(store *obsstore.Store, judge eval.JudgeClient, gate ContentGate, logger *slog.Logger) *EvalRunner {
	if logger == nil {
		logger = slog.Default()
	}
	return &EvalRunner{store: store, judge: judge, gate: gate, logger: logger}
}

// EvalRunResult is the plain summary the host (CLI) renders — no eval/store
// types leak past this seam (rule #2).
type EvalRunResult struct {
	RunID     int64
	DatasetID int64
	Total     int
	Passed    int
	MeanScore float64
	PassRate  float64
}

// CreateDatasetFromTraces creates (or reuses) a named dataset and snapshots up
// to limit recent LLM spans into it. Raw input/output bodies are snapshotted
// only when the ContentGate allows raw content (plan §10) — otherwise items
// are facts-only (content_hash still recorded). Returns the dataset id + the
// number of new items added.
func (r *EvalRunner) CreateDatasetFromTraces(ctx context.Context, name, description string, limit int) (int64, int, error) {
	id, err := r.store.CreateDataset(ctx, name, description)
	if err != nil {
		return 0, 0, err
	}
	added, err := r.store.AddDatasetItemsFromTraces(ctx, id, limit, r.allowsRaw())
	if err != nil {
		return 0, 0, err
	}
	return id, added, nil
}

// ListDatasets returns the node's datasets (newest-first).
func (r *EvalRunner) ListDatasets(ctx context.Context) ([]obsstore.DatasetRow, error) {
	return r.store.ListDatasets(ctx)
}

// RunEval scores a dataset with the given scorer specs and persists the run +
// per-(item,scorer) scores. It errors if the dataset is unknown or a scorer
// spec is invalid (e.g. llm_judge without a wired judge).
func (r *EvalRunner) RunEval(ctx context.Context, datasetName string, specs []eval.Spec, runName string) (EvalRunResult, error) {
	dsID, found, err := r.store.DatasetIDByName(ctx, datasetName)
	if err != nil {
		return EvalRunResult{}, err
	}
	if !found {
		return EvalRunResult{}, fmt.Errorf("eval: dataset %q not found (create it with `observer eval dataset create-from-traces`)", datasetName)
	}
	scorers, err := eval.BuildAll(specs, r.judge)
	if err != nil {
		return EvalRunResult{}, err
	}
	rows, err := r.store.LoadSamples(ctx, dsID)
	if err != nil {
		return EvalRunResult{}, err
	}
	samples := make([]eval.Sample, 0, len(rows))
	for _, row := range rows {
		samples = append(samples, sampleFromRow(row))
	}

	scorersJSON, _ := json.Marshal(specs)
	runID, err := r.store.CreateEvalRun(ctx, dsID, runName, string(scorersJSON))
	if err != nil {
		return EvalRunResult{}, err
	}

	results := eval.Run(ctx, samples, scorers)
	scoreRows := make([]obsstore.ScoreRow, 0, len(results))
	for i := range results {
		res := results[i]
		itemID := res.ItemID
		scoreRows = append(scoreRows, obsstore.ScoreRow{
			RunID:     &runID,
			ItemID:    &itemID,
			SpanID:    res.SpanID,
			Scorer:    res.Score.Scorer,
			Score:     res.Score.Score,
			Passed:    res.Score.Passed,
			Rationale: res.Score.Rationale,
			Source:    "run",
		})
	}
	if err := r.store.InsertScores(ctx, scoreRows); err != nil {
		return EvalRunResult{}, err
	}
	sum := eval.Summarize(results)
	if err := r.store.FinishEvalRun(ctx, runID, sum.Total, sum.Passed, sum.MeanScore, "done"); err != nil {
		return EvalRunResult{}, err
	}
	return EvalRunResult{
		RunID: runID, DatasetID: dsID,
		Total: sum.Total, Passed: sum.Passed, MeanScore: sum.MeanScore, PassRate: sum.PassRate(),
	}, nil
}

func (r *EvalRunner) allowsRaw() bool {
	return r.gate != nil && r.gate.AllowsRawContent()
}

func sampleFromRow(row obsstore.SampleRow) eval.Sample {
	return eval.Sample{
		ItemID:    row.ItemID,
		SpanID:    row.SpanID,
		TraceID:   row.TraceID,
		Input:     row.Input,
		Output:    row.Output,
		Reference: row.Reference,
		Facts: eval.SpanFacts{
			Status:       row.Status,
			DurationMS:   row.DurationMS,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			CostUSD:      row.CostUSD,
			Model:        row.Model,
		},
	}
}

// OnlineSampler runs a fixed set of (facts-based) scorers over a sampled
// fraction of live LLM spans as they ingest — the Langfuse/Arize online-eval
// model (plan §8). It implements SpanSampler so the TraceIngestor can call it
// without importing eval. Scores persist with source='online' (run_id NULL).
// It is best-effort: a scoring/persist error is logged and dropped, never
// failing ingest.
type OnlineSampler struct {
	store   *obsstore.Store
	scorers []eval.Scorer
	rate    float64
	logger  *slog.Logger

	mu  sync.Mutex
	rng *rand.Rand
}

// NewOnlineSampler builds a sampler from already-built scorers and a rate in
// (0,1]. Returns nil when rate<=0 or no scorers (online sampling off), so the
// caller can wire it unconditionally.
func NewOnlineSampler(store *obsstore.Store, scorers []eval.Scorer, rate float64, logger *slog.Logger) *OnlineSampler {
	if rate <= 0 || len(scorers) == 0 || store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &OnlineSampler{
		store:   store,
		scorers: scorers,
		rate:    rate,
		logger:  logger,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SampleSpan scores one ingested span with probability = rate. Only LLM spans
// are sampled (others have no model output to score).
func (o *OnlineSampler) SampleSpan(ctx context.Context, s span.Span) {
	if o == nil || s.Kind != span.KindLLM {
		return
	}
	o.mu.Lock()
	roll := o.rng.Float64()
	o.mu.Unlock()
	if roll >= o.rate {
		return
	}
	sample := eval.Sample{
		SpanID:  s.SpanID,
		TraceID: s.TraceID,
		Facts: eval.SpanFacts{
			Status:       string(s.Status),
			DurationMS:   spanDurationMS(s),
			InputTokens:  derefI64(s.InputTokens),
			OutputTokens: derefI64(s.OutputTokens),
			CostUSD:      derefF64(s.CostUSD),
			Model:        s.Model,
		},
	}
	results := eval.Run(ctx, []eval.Sample{sample}, o.scorers)
	rows := make([]obsstore.ScoreRow, 0, len(results))
	for i := range results {
		rows = append(rows, obsstore.ScoreRow{
			SpanID: results[i].SpanID, Scorer: results[i].Score.Scorer,
			Score: results[i].Score.Score, Passed: results[i].Score.Passed,
			Rationale: results[i].Score.Rationale, Source: "online",
		})
	}
	if err := o.store.InsertScores(ctx, rows); err != nil {
		o.logger.Warn("obs eval: online score persist failed", "span_id", s.SpanID, "err", err)
	}
}

func spanDurationMS(s span.Span) int64 {
	if s.StartedAt.IsZero() || s.EndedAt.IsZero() || s.EndedAt.Before(s.StartedAt) {
		return 0
	}
	return s.EndedAt.Sub(s.StartedAt).Milliseconds()
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefF64(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}
