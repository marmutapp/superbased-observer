// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package rollup

import (
	"context"
	"database/sql"
	"testing"
)

// evalItem is one seeded obs_eval_items row for the per-item eval rollup tests.
type evalItem struct {
	pushedBy, userEmail                   string
	runID, itemID                         int64
	runName, datasetName, spanID, traceID string
	scorer                                string
	score                                 float64
	passed                                bool
	ts, contentHash                       string
	input, expected, output, rationale    string
}

func seedEvalItem(t *testing.T, d *sql.DB, e evalItem) {
	t.Helper()
	passed := 0
	if e.passed {
		passed = 1
	}
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO obs_eval_items
		   (org_id, user_email, run_id, run_name, dataset_id, dataset_name, item_id,
		    span_id, trace_id, scorer, score, passed, source, duration_ms, ts, content_hash,
		    input_excerpt, expected_excerpt, output_excerpt, rationale,
		    pushed_at, pushed_by_user_id)
		 VALUES ('org1', ?, ?, ?, 3, ?, ?, ?, ?, ?, ?, ?, 'run', 120, ?, ?, ?, ?, ?, ?,
		         '2026-05-26T11:00:00Z', ?)`,
		e.userEmail, e.runID, e.runName, e.datasetName, e.itemID,
		e.spanID, e.traceID, e.scorer, e.score, passed, e.ts, e.contentHash,
		nullString(e.input), nullString(e.expected), nullString(e.output), nullString(e.rationale),
		e.pushedBy); err != nil {
		t.Fatalf("seed eval item run=%d item=%d/%s: %v", e.runID, e.itemID, e.scorer, err)
	}
}

// TestObsEvalRuns_ListAndDetail seeds one run with two items × two scorers and
// asserts the run-list aggregation (pass rate, scorer set, ref) and the
// per-item detail (score rows + summary). Ref round-trips through
// encode/decode; a bad ref is a clean not-found.
func TestObsEvalRuns_ListAndDetail(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	// Run 7 (dataset "greetings"): items 11 & 12, scorers json_valid + exact.
	// 3 of 4 scores pass.
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 11, runName: "run-A", datasetName: "greetings", spanID: "sp-11", traceID: "tr-11", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T06:00:00Z", contentHash: "h11", input: "in-11", output: "out-11", rationale: "ok"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 11, runName: "run-A", datasetName: "greetings", spanID: "sp-11", traceID: "tr-11", scorer: "exact", score: 0, passed: false, ts: "2026-05-26T06:00:00Z", contentHash: "h11"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 12, runName: "run-A", datasetName: "greetings", spanID: "sp-12", traceID: "tr-12", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T06:05:00Z", contentHash: "h12"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 12, runName: "run-A", datasetName: "greetings", spanID: "sp-12", traceID: "tr-12", scorer: "exact", score: 1, passed: true, ts: "2026-05-26T06:05:00Z", contentHash: "h12"})

	list, err := ObsEvalRuns(ctx, d, w30, adminScope, "u-a", fixedNow)
	if err != nil {
		t.Fatalf("ObsEvalRuns: %v", err)
	}
	if !list.Configured || len(list.Runs) != 1 {
		t.Fatalf("list configured=%v runs=%d, want 1 run", list.Configured, len(list.Runs))
	}
	run := list.Runs[0]
	if run.Ref != "u-a~7" {
		t.Errorf("ref = %q, want u-a~7", run.Ref)
	}
	if run.Items != 2 || run.Scores != 4 || run.Passed != 3 {
		t.Errorf("run counts = items%d scores%d passed%d, want 2/4/3", run.Items, run.Scores, run.Passed)
	}
	if run.PassRate < 0.74 || run.PassRate > 0.76 {
		t.Errorf("pass_rate = %v, want ~0.75", run.PassRate)
	}
	if len(run.Scorers) != 2 || run.Scorers[0] != "exact" || run.Scorers[1] != "json_valid" {
		t.Errorf("scorers = %v, want [exact json_valid]", run.Scorers)
	}

	// Detail via the ref.
	det, found, err := ObsEvalRunDetail(ctx, d, "u-a~7", w30, adminScope, "u-a")
	if err != nil || !found {
		t.Fatalf("ObsEvalRunDetail: found=%v err=%v", found, err)
	}
	if len(det.Scores) != 4 {
		t.Fatalf("detail scores = %d, want 4", len(det.Scores))
	}
	if det.Run.Ref != "u-a~7" || det.Run.DatasetName != "greetings" {
		t.Errorf("detail run = %+v, want ref u-a~7 dataset greetings", det.Run)
	}
	// TraceID rides for the trajectory link.
	if det.Scores[0].TraceID == "" {
		t.Errorf("detail score[0] missing trace_id (trajectory link): %+v", det.Scores[0])
	}

	// Malformed ref → clean not-found (no error).
	if _, found, err := ObsEvalRunDetail(ctx, d, "not-a-ref", w30, adminScope, "u-a"); err != nil || found {
		t.Errorf("bad ref: found=%v err=%v, want found=false err=nil", found, err)
	}
	// Unknown run → not found.
	if _, found, err := ObsEvalRunDetail(ctx, d, "u-a~999", w30, adminScope, "u-a"); err != nil || found {
		t.Errorf("unknown run: found=%v err=%v, want found=false err=nil", found, err)
	}
}

// TestObsEvalCompare_PerScorerDeltas seeds two runs of the same dataset and
// asserts the per-scorer paired deltas (compare − base), including a scorer
// present in only one run (zeroed other side).
func TestObsEvalCompare_PerScorerDeltas(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()

	// Base run 7: json_valid mean 0.5 (1 pass of 2), exact mean 1 (1 pass of 1).
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 11, runName: "base", datasetName: "ds", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T06:00:00Z", contentHash: "h"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 12, runName: "base", datasetName: "ds", scorer: "json_valid", score: 0, passed: false, ts: "2026-05-26T06:00:00Z", contentHash: "h"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 11, runName: "base", datasetName: "ds", scorer: "exact", score: 1, passed: true, ts: "2026-05-26T06:00:00Z", contentHash: "h"})
	// Compare run 8: json_valid mean 1 (2 pass of 2) — improvement; NEW scorer
	// "length" mean 0 — regression side present only in compare.
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 8, itemID: 11, runName: "cmp", datasetName: "ds", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T07:00:00Z", contentHash: "h"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 8, itemID: 12, runName: "cmp", datasetName: "ds", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T07:00:00Z", contentHash: "h"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 8, itemID: 11, runName: "cmp", datasetName: "ds", scorer: "length", score: 0, passed: false, ts: "2026-05-26T07:00:00Z", contentHash: "h"})

	res, found, err := ObsEvalCompare(ctx, d, "u-a~7", "u-a~8", adminScope, "u-a")
	if err != nil || !found {
		t.Fatalf("ObsEvalCompare: found=%v err=%v", found, err)
	}
	if res.Base.RunName != "base" || res.Compare.RunName != "cmp" {
		t.Errorf("summaries = base %q / compare %q, want base/cmp", res.Base.RunName, res.Compare.RunName)
	}
	byScorer := map[string]ObsEvalScorerDelta{}
	for _, s := range res.Scorers {
		byScorer[s.Scorer] = s
	}
	// json_valid: base mean 0.5 → compare 1.0, delta +0.5; pass-rate +0.5.
	jv := byScorer["json_valid"]
	if jv.MeanDelta < 0.49 || jv.MeanDelta > 0.51 || jv.PassRateDelta < 0.49 || jv.PassRateDelta > 0.51 {
		t.Errorf("json_valid delta = %+v, want mean +0.5 passrate +0.5", jv)
	}
	// exact: only in base → compare side zeroed.
	if ex := byScorer["exact"]; ex.BaseCount != 1 || ex.CompareCount != 0 {
		t.Errorf("exact = %+v, want base only", ex)
	}
	// length: only in compare → base side zeroed.
	if ln := byScorer["length"]; ln.BaseCount != 0 || ln.CompareCount != 1 {
		t.Errorf("length = %+v, want compare only", ln)
	}
}

// TestObsEvalItemContent_OnlyExcerptBearing asserts the audited content export
// returns only rows carrying an excerpt (hash-only rows are omitted) and a bad
// ref is a clean not-found.
func TestObsEvalItemContent_OnlyExcerptBearing(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	// One item WITH excerpts, one hash-only (all excerpt columns NULL).
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 11, runName: "r", datasetName: "ds", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T06:00:00Z", contentHash: "h11", input: "the input", expected: "gold", output: "actual", rationale: "why"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 12, runName: "r", datasetName: "ds", scorer: "json_valid", score: 0, passed: false, ts: "2026-05-26T06:00:00Z", contentHash: "h12"})

	res, found, err := ObsEvalItemContent(ctx, d, "u-a~7", adminScope, "u-a")
	if err != nil || !found {
		t.Fatalf("ObsEvalItemContent: found=%v err=%v", found, err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("content items = %d, want 1 (hash-only row omitted)", len(res.Items))
	}
	it := res.Items[0]
	if it.InputExcerpt != "the input" || it.ExpectedExcerpt != "gold" || it.OutputExcerpt != "actual" || it.Rationale != "why" {
		t.Errorf("content row = %+v, want full excerpts", it)
	}
	if _, found, _ := ObsEvalItemContent(ctx, d, "bad", adminScope, "u-a"); found {
		t.Errorf("bad ref returned found=true")
	}
}

// TestObsEvalRuns_ScopedOut asserts a plain member (no self match) sees nothing,
// and a self-scoped member sees only their own node's runs.
func TestObsEvalRuns_ScopedOut(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	seedEvalItem(t, d, evalItem{pushedBy: "u-a", userEmail: "a@x", runID: 7, itemID: 11, runName: "mine", datasetName: "ds", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T06:00:00Z", contentHash: "h"})
	seedEvalItem(t, d, evalItem{pushedBy: "u-b", userEmail: "b@x", runID: 7, itemID: 11, runName: "theirs", datasetName: "ds", scorer: "json_valid", score: 1, passed: true, ts: "2026-05-26T06:00:00Z", contentHash: "h"})

	// Self-scope u-a → only u-a's run.
	self, err := ObsEvalRuns(ctx, d, w30, Scope{}, "u-a", fixedNow)
	if err != nil {
		t.Fatalf("ObsEvalRuns self: %v", err)
	}
	if len(self.Runs) != 1 || self.Runs[0].Ref != "u-a~7" {
		t.Fatalf("self scope runs = %+v, want only u-a~7", self.Runs)
	}
	// u-a cannot reach u-b's run even with a hand-crafted ref.
	if _, found, _ := ObsEvalRunDetail(ctx, d, "u-b~7", w30, Scope{}, "u-a"); found {
		t.Errorf("self-scoped u-a reached u-b's run via crafted ref")
	}
}
