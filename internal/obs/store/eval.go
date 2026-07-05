package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// This file is the obs/store persistence seam for the minimal eval plane (plan
// §8). It owns the four obs_eval/dataset tables. It deliberately does NOT
// import internal/obs/eval (the pure scorer core): it returns its own row
// types and the obs-root orchestrator maps them to eval.Sample, so the
// persistence layer and the scoring layer stay decoupled (one seam, no type
// leak — CLAUDE.md rule #2).

// DatasetRow is one dataset with its item count.
type DatasetRow struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	ItemCount   int64  `json:"item_count"`
}

// SampleRow is one dataset item joined with its span facts — the persistence
// shape the orchestrator maps to eval.Sample.
type SampleRow struct {
	ItemID       int64
	SpanID       string
	TraceID      string
	Input        string
	Output       string
	Reference    string
	Status       string
	DurationMS   int64
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Model        string
}

// EvalRunRow is one eval run's summary.
type EvalRunRow struct {
	ID        int64   `json:"id"`
	DatasetID int64   `json:"dataset_id"`
	Name      string  `json:"name"`
	Scorers   string  `json:"scorers"`
	StartedAt string  `json:"started_at"`
	EndedAt   string  `json:"ended_at"`
	Total     int     `json:"total"`
	Passed    int     `json:"passed"`
	MeanScore float64 `json:"mean_score"`
	Status    string  `json:"status"`
}

// ScoreRow is one persisted score. RunID/ItemID are nil for online-sampled
// scores (source='online'); set for an eval run (source='run').
type ScoreRow struct {
	RunID     *int64
	ItemID    *int64
	SpanID    string
	Scorer    string
	Score     float64
	Passed    bool
	Rationale string
	Source    string
}

// CreateDataset inserts a dataset by name, returning its id. An existing name
// returns the existing id (idempotent — re-creating a dataset is a no-op).
func (s *Store) CreateDataset(ctx context.Context, name, description string) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("obs/store.CreateDataset: empty name")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO obs_datasets (name, description, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET description = COALESCE(NULLIF(excluded.description,''), obs_datasets.description)`,
		name, description, ts(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("obs/store.CreateDataset: %w", err)
	}
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		// LastInsertId is unreliable on the upsert path; confirm by name.
		var got int64
		if e := s.db.QueryRowContext(ctx, `SELECT id FROM obs_datasets WHERE name = ?`, name).Scan(&got); e == nil {
			return got, nil
		}
		return id, nil
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM obs_datasets WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, fmt.Errorf("obs/store.CreateDataset: resolve id: %w", err)
	}
	return id, nil
}

// AddDatasetItemsFromTraces snapshots recent LLM spans into a dataset: one
// item per span, capturing the prompt/response bodies from obs_span_content
// (where present) plus a content hash. When includeContent is false (the ContentGate denies
// raw bodies, plan §10), the raw input/output are NOT persisted into the item —
// only the content_hash is, computed from the source content so it still
// reflects what was there. Idempotent on (dataset_id, span_id). Returns the
// number of new items added. limit is clamped to [1,1000].
func (s *Store) AddDatasetItemsFromTraces(ctx context.Context, datasetID int64, limit int, includeContent bool) (int, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT sp.span_id, sp.trace_id,
       COALESCE((SELECT content FROM obs_span_content c WHERE c.span_id = sp.span_id AND c.kind = 'prompt'   ORDER BY c.id DESC LIMIT 1), ''),
       COALESCE((SELECT content FROM obs_span_content c WHERE c.span_id = sp.span_id AND c.kind = 'response' ORDER BY c.id DESC LIMIT 1), '')
  FROM obs_spans sp
 WHERE sp.kind = 'llm'
 ORDER BY sp.started_at DESC
 LIMIT ?`, limit)
	if err != nil {
		return 0, fmt.Errorf("obs/store.AddDatasetItemsFromTraces: select: %w", err)
	}
	defer rows.Close()

	type item struct{ spanID, traceID, input, output string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.spanID, &it.traceID, &it.input, &it.output); err != nil {
			return 0, fmt.Errorf("obs/store.AddDatasetItemsFromTraces: scan: %w", err)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("obs/store.AddDatasetItemsFromTraces: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO obs_dataset_items (dataset_id, trace_id, span_id, input, output, reference, content_hash, created_at)
VALUES (?, ?, ?, ?, ?, '', ?, ?)
ON CONFLICT(dataset_id, span_id) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("obs/store.AddDatasetItemsFromTraces: prepare: %w", err)
	}
	defer stmt.Close()
	added := 0
	now := ts(time.Now())
	for _, it := range items {
		hash := hashContent(it.input, it.output)
		storedIn, storedOut := it.input, it.output
		if !includeContent {
			storedIn, storedOut = "", "" // metadata-only: keep the hash, drop raw bodies
		}
		res, err := stmt.ExecContext(ctx, datasetID, it.traceID, it.spanID, storedIn, storedOut, hash, now)
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("obs/store.AddDatasetItemsFromTraces: insert: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("obs/store.AddDatasetItemsFromTraces: commit: %w", err)
	}
	return added, nil
}

// ListDatasets returns datasets newest-first with their item counts.
func (s *Store) ListDatasets(ctx context.Context) ([]DatasetRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.name, COALESCE(d.description,''), d.created_at, COUNT(i.id)
  FROM obs_datasets d
  LEFT JOIN obs_dataset_items i ON i.dataset_id = d.id
 GROUP BY d.id
 ORDER BY d.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ListDatasets: %w", err)
	}
	defer rows.Close()
	var out []DatasetRow
	for rows.Next() {
		var d DatasetRow
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.CreatedAt, &d.ItemCount); err != nil {
			return nil, fmt.Errorf("obs/store.ListDatasets: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DatasetIDByName resolves a dataset name to its id (found=false when absent).
func (s *Store) DatasetIDByName(ctx context.Context, name string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM obs_datasets WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("obs/store.DatasetIDByName: %w", err)
	}
	return id, true, nil
}

// LoadSamples returns a dataset's items joined with their span facts, ordered
// by item id. Missing spans (item with no surviving span) still return with
// zeroed facts so a code scorer over Output/Reference still runs.
func (s *Store) LoadSamples(ctx context.Context, datasetID int64) ([]SampleRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT i.id, COALESCE(i.span_id,''), COALESCE(i.trace_id,''),
       COALESCE(i.input,''), COALESCE(i.output,''), COALESCE(i.reference,''),
       COALESCE(sp.status,''), COALESCE(sp.started_at,''), COALESCE(sp.ended_at,''),
       COALESCE(sp.input_tokens,0), COALESCE(sp.output_tokens,0), COALESCE(sp.cost_usd,0), COALESCE(sp.model,'')
  FROM obs_dataset_items i
  LEFT JOIN obs_spans sp ON sp.span_id = i.span_id
 WHERE i.dataset_id = ?
 ORDER BY i.id`, datasetID)
	if err != nil {
		return nil, fmt.Errorf("obs/store.LoadSamples: %w", err)
	}
	defer rows.Close()
	var out []SampleRow
	for rows.Next() {
		var (
			r              SampleRow
			started, ended string
		)
		if err := rows.Scan(&r.ItemID, &r.SpanID, &r.TraceID, &r.Input, &r.Output, &r.Reference,
			&r.Status, &started, &ended, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.Model); err != nil {
			return nil, fmt.Errorf("obs/store.LoadSamples: scan: %w", err)
		}
		r.DurationMS = durationMS(started, ended)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateEvalRun opens a run row (status 'running') and returns its id.
func (s *Store) CreateEvalRun(ctx context.Context, datasetID int64, name, scorersJSON string) (int64, error) {
	if scorersJSON == "" {
		scorersJSON = "[]"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO obs_eval_runs (dataset_id, name, scorers, started_at, status) VALUES (?, ?, ?, ?, 'running')`,
		datasetID, name, scorersJSON, ts(time.Now()))
	if err != nil {
		return 0, fmt.Errorf("obs/store.CreateEvalRun: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("obs/store.CreateEvalRun: id: %w", err)
	}
	return id, nil
}

// FinishEvalRun records the aggregate + closes a run.
func (s *Store) FinishEvalRun(ctx context.Context, runID int64, total, passed int, mean float64, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE obs_eval_runs SET ended_at = ?, total = ?, passed = ?, mean_score = ?, status = ? WHERE id = ?`,
		ts(time.Now()), total, passed, mean, status, runID)
	if err != nil {
		return fmt.Errorf("obs/store.FinishEvalRun: %w", err)
	}
	return nil
}

// InsertScores writes a batch of scores in one transaction.
func (s *Store) InsertScores(ctx context.Context, scores []ScoreRow) error {
	if len(scores) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("obs/store.InsertScores: begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO obs_eval_scores (run_id, item_id, span_id, scorer, score, passed, rationale, source, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("obs/store.InsertScores: prepare: %w", err)
	}
	defer stmt.Close()
	now := ts(time.Now())
	for _, sc := range scores {
		src := sc.Source
		if src == "" {
			src = "run"
		}
		if _, err := stmt.ExecContext(ctx, sc.RunID, sc.ItemID, sc.SpanID, sc.Scorer, sc.Score, boolInt(sc.Passed), sc.Rationale, src, now); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("obs/store.InsertScores: exec: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("obs/store.InsertScores: commit: %w", err)
	}
	return nil
}

// ListEvalRuns returns a dataset's runs newest-first (limit clamped [1,200]).
func (s *Store) ListEvalRuns(ctx context.Context, datasetID int64, limit int) ([]EvalRunRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, dataset_id, COALESCE(name,''), scorers, started_at, COALESCE(ended_at,''),
       total, passed, mean_score, status
  FROM obs_eval_runs WHERE dataset_id = ? ORDER BY id DESC LIMIT ?`, datasetID, limit)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ListEvalRuns: %w", err)
	}
	defer rows.Close()
	var out []EvalRunRow
	for rows.Next() {
		var r EvalRunRow
		if err := rows.Scan(&r.ID, &r.DatasetID, &r.Name, &r.Scorers, &r.StartedAt, &r.EndedAt,
			&r.Total, &r.Passed, &r.MeanScore, &r.Status); err != nil {
			return nil, fmt.Errorf("obs/store.ListEvalRuns: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EvalRunListRow is one run joined with its dataset name — the shape the
// dashboard Evals list renders (a run, not a dataset, is the primary artifact
// a user compares). DatasetName is empty only if the dataset was deleted.
type EvalRunListRow struct {
	EvalRunRow
	DatasetName string `json:"dataset_name"`
}

// RunScoreRow is one persisted score joined with its dataset-item context
// (span/trace identity for cross-linking back to the trajectory). It carries
// no raw body — the obs_eval_scores rationale is a bounded scorer verdict, and
// item content is only ever stored under the ContentGate (plan §10).
type RunScoreRow struct {
	ItemID    int64   `json:"item_id"`
	SpanID    string  `json:"span_id"`
	TraceID   string  `json:"trace_id"`
	Scorer    string  `json:"scorer"`
	Score     float64 `json:"score"`
	Passed    bool    `json:"passed"`
	Rationale string  `json:"rationale"`
}

// ListAllEvalRuns returns runs across every dataset, newest-first, each joined
// with its dataset name (limit clamped [1,200]). This is the dashboard's
// top-level Evals feed; per-dataset drill-down uses ListEvalRuns.
func (s *Store) ListAllEvalRuns(ctx context.Context, limit int) ([]EvalRunListRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.dataset_id, COALESCE(r.name,''), r.scorers, r.started_at, COALESCE(r.ended_at,''),
       r.total, r.passed, r.mean_score, r.status, COALESCE(d.name,'')
  FROM obs_eval_runs r
  LEFT JOIN obs_datasets d ON d.id = r.dataset_id
 ORDER BY r.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("obs/store.ListAllEvalRuns: %w", err)
	}
	defer rows.Close()
	var out []EvalRunListRow
	for rows.Next() {
		var r EvalRunListRow
		if err := rows.Scan(&r.ID, &r.DatasetID, &r.Name, &r.Scorers, &r.StartedAt, &r.EndedAt,
			&r.Total, &r.Passed, &r.MeanScore, &r.Status, &r.DatasetName); err != nil {
			return nil, fmt.Errorf("obs/store.ListAllEvalRuns: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetEvalRun returns one run (with its dataset name), found=false when absent.
func (s *Store) GetEvalRun(ctx context.Context, runID int64) (EvalRunListRow, bool, error) {
	var r EvalRunListRow
	err := s.db.QueryRowContext(ctx, `
SELECT r.id, r.dataset_id, COALESCE(r.name,''), r.scorers, r.started_at, COALESCE(r.ended_at,''),
       r.total, r.passed, r.mean_score, r.status, COALESCE(d.name,'')
  FROM obs_eval_runs r
  LEFT JOIN obs_datasets d ON d.id = r.dataset_id
 WHERE r.id = ?`, runID).Scan(&r.ID, &r.DatasetID, &r.Name, &r.Scorers, &r.StartedAt, &r.EndedAt,
		&r.Total, &r.Passed, &r.MeanScore, &r.Status, &r.DatasetName)
	if errors.Is(err, sql.ErrNoRows) {
		return EvalRunListRow{}, false, nil
	}
	if err != nil {
		return EvalRunListRow{}, false, fmt.Errorf("obs/store.GetEvalRun: %w", err)
	}
	return r, true, nil
}

// LoadRunScores returns every score a run wrote (source='run'), joined to its
// dataset item for span/trace identity, ordered by item then scorer so the UI
// can align two runs cell-for-cell. Online-sampled scores (run_id NULL) are
// never returned here.
func (s *Store) LoadRunScores(ctx context.Context, runID int64) ([]RunScoreRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT sc.span_id, COALESCE(i.id, 0), COALESCE(i.trace_id,''),
       sc.scorer, sc.score, sc.passed, COALESCE(sc.rationale,'')
  FROM obs_eval_scores sc
  LEFT JOIN obs_dataset_items i ON i.id = sc.item_id
 WHERE sc.run_id = ?
 ORDER BY COALESCE(sc.item_id, 0), sc.scorer`, runID)
	if err != nil {
		return nil, fmt.Errorf("obs/store.LoadRunScores: %w", err)
	}
	defer rows.Close()
	var out []RunScoreRow
	for rows.Next() {
		var (
			r      RunScoreRow
			passed int
		)
		if err := rows.Scan(&r.SpanID, &r.ItemID, &r.TraceID, &r.Scorer, &r.Score, &passed, &r.Rationale); err != nil {
			return nil, fmt.Errorf("obs/store.LoadRunScores: scan: %w", err)
		}
		r.Passed = passed != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func hashContent(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
