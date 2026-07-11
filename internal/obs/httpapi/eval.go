package httpapi

import (
	"net/http"
	"strconv"

	obsstore "github.com/marmutapp/superbased-observer/internal/obs/store"
)

// This file is the read-only HTTP surface for the eval plane (plan §8). It
// serves the same obs store the trajectory endpoints do — runs/scores/datasets
// all live in the node-local obs_eval/dataset tables, never on the org-push
// wire. The handlers are pure reads (no scoring here — that's the offline
// `observer eval run` path); the comparison itself is a frontend concern that
// aligns two runs cell-for-cell, so the API stays a flat run/scores feed.

// handleEvalDatasets lists datasets newest-first with their item counts.
func (a *API) handleEvalDatasets(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ListDatasets(r.Context())
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if rows == nil {
		rows = []obsstore.DatasetRow{}
	}
	a.writeJSON(w, map[string]any{"datasets": rows})
}

// handleEvalRuns lists recent eval runs across every dataset (the top-level
// Evals feed), each joined with its dataset name.
func (a *API) handleEvalRuns(w http.ResponseWriter, r *http.Request) {
	limit := intParam(r, "limit", 100)
	rows, err := a.store.ListAllEvalRuns(r.Context(), limit)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if rows == nil {
		rows = []obsstore.EvalRunListRow{}
	}
	a.writeJSON(w, map[string]any{"runs": rows})
}

// evalRunResponse is the /api/obs/eval/run/{id} payload: the run summary plus
// every score it wrote, ordered item-then-scorer so the frontend can diff two
// runs cell-for-cell.
type evalRunResponse struct {
	Run    obsstore.EvalRunListRow `json:"run"`
	Scores []obsstore.RunScoreRow  `json:"scores"`
}

// handleEvalRun returns one run's summary + per-item scores; 404 when unknown.
func (a *API) handleEvalRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "bad run id", http.StatusBadRequest)
		return
	}
	run, found, err := a.store.GetEvalRun(r.Context(), id)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if !found {
		http.Error(w, "run not found", http.StatusNotFound)
		return
	}
	scores, err := a.store.LoadRunScores(r.Context(), id)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	if scores == nil {
		scores = []obsstore.RunScoreRow{}
	}
	a.writeJSON(w, evalRunResponse{Run: run, Scores: scores})
}
