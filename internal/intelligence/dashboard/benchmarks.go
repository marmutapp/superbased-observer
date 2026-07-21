package dashboard

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/benchmark"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// benchmarks.go is the read-only node-dashboard backend for the Benchmarks
// Harness (docs/plans/benchmarks-harness-plan-2026-07-11.md §4.2). Every
// endpoint is a point-in-time view over the four node-local benchmark_* tables
// (migration 061) through the single store seam; nothing here drives a harness
// or spends budget (POST /run stays CLI-only, plan §4.2). The stats verdicts
// are sourced from internal/benchmark.ComputeReport — the EXACT computation the
// CLI `benchmark report` runs — never reimplemented here. List responses are
// sanitized: no prompts, repo/workspace paths, or judge rationale ride the wire.

// benchmarkRunSummary is one sanitized run-list row: lifecycle + spend +
// config-matrix shape, all derivable from the run header + the stored spec.
// No prompts / paths — the spec's config identities (harness/model) are the
// only spec fields surfaced, and those are non-sensitive matrix keys.
type benchmarkRunSummary struct {
	RunID          string   `json:"run_id"`
	SpecName       string   `json:"spec_name"`
	SpecHash       string   `json:"spec_hash"`
	Status         string   `json:"status"`
	StartedAt      string   `json:"started_at"`
	FinishedAt     string   `json:"finished_at,omitempty"`
	PlannedCells   int      `json:"planned_cells"`
	CompletedCells int      `json:"completed_cells"`
	SpendUSD       float64  `json:"spend_usd"`
	JudgeSpendUSD  float64  `json:"judge_spend_usd"`
	Configs        int      `json:"configs"`
	Tasks          int      `json:"tasks"`
	Repeats        int      `json:"repeats"`
	Harnesses      []string `json:"harnesses"`
	Models         []string `json:"models"`
}

// handleBenchmarks — GET /api/benchmarks?limit: sanitized run list, most
// recent first. Always returns runs:[] (never null) per the empty-array wire
// contract.
func (s *Server) handleBenchmarks(w http.ResponseWriter, r *http.Request) {
	limit := intArg(r, "limit", 50, 1, 500)
	st := store.New(s.db())
	runs, err := st.ListBenchmarkRuns(r.Context(), limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]benchmarkRunSummary, 0, len(runs))
	for _, run := range runs {
		out = append(out, summarizeBenchmarkRun(run))
	}
	writeJSON(w, map[string]any{"runs": out, "total": len(out)})
}

func summarizeBenchmarkRun(run benchmark.RunRecord) benchmarkRunSummary {
	sum := benchmarkRunSummary{
		RunID:          run.RunID,
		SpecName:       run.SpecName,
		SpecHash:       run.SpecHash,
		Status:         run.Status,
		StartedAt:      run.StartedAt.UTC().Format(time.RFC3339),
		PlannedCells:   run.PlannedCells,
		CompletedCells: run.CompletedCells,
		SpendUSD:       run.SpendUSD,
		JudgeSpendUSD:  run.JudgeSpendUSD,
		Harnesses:      []string{},
		Models:         []string{},
	}
	if !run.FinishedAt.IsZero() {
		sum.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
	}
	// Config-matrix shape from the stored spec snapshot. Decode failure
	// degrades to counts-from-header — never a 500 over a list row.
	var spec benchmark.Spec
	if err := json.Unmarshal([]byte(run.SpecJSON), &spec); err == nil {
		sum.Configs = len(spec.Configs)
		sum.Tasks = len(spec.Tasks)
		sum.Repeats = spec.Repeats
		hSet := map[string]bool{}
		mSet := map[string]bool{}
		for _, c := range spec.Configs {
			if !hSet[c.Harness] {
				hSet[c.Harness] = true
				sum.Harnesses = append(sum.Harnesses, c.Harness)
			}
			if !mSet[c.Model] {
				mSet[c.Model] = true
				sum.Models = append(sum.Models, c.Model)
			}
		}
		sort.Strings(sum.Harnesses)
		sort.Strings(sum.Models)
	}
	return sum
}

// handleBenchmarkDetail — GET /api/benchmarks/{run_id} and
// GET /api/benchmarks/{run_id}/export. The {run_id} segment is validated
// against path traversal; a run_id contains no slash (URL-decoded), so the
// only guards needed are non-empty + no "..".
func (s *Server) handleBenchmarkDetail(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/benchmarks/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		http.Error(w, "run id required", http.StatusBadRequest)
		return
	}
	segs := strings.Split(rest, "/")
	runID := segs[0]
	wantExport := len(segs) == 2 && segs[1] == "export"
	if len(segs) > 2 || (len(segs) == 2 && !wantExport) {
		http.NotFound(w, r)
		return
	}
	if runID == "" || strings.Contains(runID, "..") {
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}

	st := store.New(s.db())
	run, ok, err := st.LoadBenchmarkRun(r.Context(), runID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	var spec benchmark.Spec
	if err := json.Unmarshal([]byte(run.SpecJSON), &spec); err != nil {
		writeErr(w, err)
		return
	}
	facts, err := st.LoadBenchmarkFacts(r.Context(), runID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// The verdicts are the CLI's exact computation — reuse, never reimplement.
	rep := benchmark.ComputeReport(spec, runID, facts)

	if wantExport {
		// Same canonical, redaction-allowlisted card the CLI `export` emits.
		writeJSON(w, benchmark.BuildExport(run, rep, time.Now().UTC().Format(time.RFC3339)))
		return
	}
	writeJSON(w, buildBenchmarkDetail(run, spec, rep, facts))
}

// benchmarkDetail is the run-detail wire shape: the canonical export card (the
// matrix + verdicts + census + warnings + content-free manifest/pricing
// snapshot) plus the node-view extras the export intentionally omits — run
// lifecycle fields, per-attempt cost dots, and the per-task pass grid.
type benchmarkDetail struct {
	benchmark.Export
	// Run lifecycle (export carries status + spend, but not these).
	PlannedCells   int     `json:"planned_cells"`
	CompletedCells int     `json:"completed_cells"`
	JudgeSpendUSD  float64 `json:"judge_spend_usd"`
	StartedAt      string  `json:"started_at"`
	Repeats        int     `json:"repeats"`
	MinSample      int     `json:"min_sample"`
	// CostDots is the raw per-attempt snapshot cost per config (plan §3.5 —
	// "raw attempt dots for cost", never hidden). Keyed by config id.
	CostDots map[string][]float64 `json:"cost_dots"`
	// Tasks is the per-task pass grid (task × config → attempts/scored/passed).
	Tasks []benchmarkTaskRow `json:"tasks"`
}

// benchmarkTaskRow is one corpus task's per-config outcome (counts only — the
// prompt / repo / path never ride this wire).
type benchmarkTaskRow struct {
	TaskID string                   `json:"task_id"`
	Cells  map[string]benchmarkCell `json:"cells"`
}

type benchmarkCell struct {
	Attempts int `json:"attempts"`
	Scored   int `json:"scored"`
	Passed   int `json:"passed"`
}

func buildBenchmarkDetail(run benchmark.RunRecord, spec benchmark.Spec, rep benchmark.Report, facts []benchmark.AttemptFact) benchmarkDetail {
	d := benchmarkDetail{
		Export:         benchmark.BuildExport(run, rep, time.Now().UTC().Format(time.RFC3339)),
		PlannedCells:   run.PlannedCells,
		CompletedCells: run.CompletedCells,
		JudgeSpendUSD:  run.JudgeSpendUSD,
		StartedAt:      run.StartedAt.UTC().Format(time.RFC3339),
		Repeats:        spec.Repeats,
		MinSample:      spec.Analysis.MinSample,
		CostDots:       map[string][]float64{},
	}
	// Per-config cost dots straight from the pure report (already snapshot
	// billed cost per attempt).
	for _, c := range rep.Configs {
		dots := c.CostSamples
		if dots == nil {
			dots = []float64{}
		}
		d.CostDots[c.ConfigID] = dots
	}
	// Per-task pass grid — a plain count aggregation over facts (no stats).
	taskOrder := make([]string, 0, len(spec.Tasks))
	rows := map[string]*benchmarkTaskRow{}
	ensure := func(taskID string) *benchmarkTaskRow {
		if row, ok := rows[taskID]; ok {
			return row
		}
		row := &benchmarkTaskRow{TaskID: taskID, Cells: map[string]benchmarkCell{}}
		rows[taskID] = row
		taskOrder = append(taskOrder, taskID)
		return row
	}
	for _, t := range spec.Tasks {
		ensure(t.ID)
	}
	for _, f := range facts {
		row := ensure(f.TaskID)
		cell := row.Cells[f.ConfigID]
		cell.Attempts++
		if f.Scored {
			cell.Scored++
			if f.Passed {
				cell.Passed++
			}
		}
		row.Cells[f.ConfigID] = cell
	}
	d.Tasks = make([]benchmarkTaskRow, 0, len(taskOrder))
	for _, id := range taskOrder {
		d.Tasks = append(d.Tasks, *rows[id])
	}
	return d
}
