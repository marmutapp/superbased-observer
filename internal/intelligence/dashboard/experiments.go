package dashboard

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Profile experiments (usability arc P6.4 / review §8.2): productized
// A/B over Track-R profiles. Definitions live as [[experiments]] in
// config.toml (written through the shared WriteToml owner + the
// OnConfigSaved hot-reload hook, so a started experiment splits NEW
// sessions immediately); arm membership is a pure hash of the session
// ID, recomputed at report time — no arm state is persisted and
// nothing new enters the org-push wire.
//
// Reports are EVIDENCE, not verdicts: per-arm n / $ / CV / turns /
// cache causes / compression savings, presented next to the §2.4
// methodology's decision-rule guidance. The operator applies the rule.

// handleExperiments serves GET /api/experiments (list, fresh from
// config) and POST /api/experiments (start one).
func (s *Server) handleExperiments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
		if err != nil {
			writeErr(w, err)
			return
		}
		exps := cfg.Experiments
		if exps == nil {
			exps = []config.ExperimentConfig{}
		}
		writeJSON(w, map[string]any{"experiments": exps})
	case http.MethodPost:
		s.handleExperimentStart(w, r)
	default:
		http.Error(w, "GET or POST", http.StatusMethodNotAllowed)
	}
}

// handleExperimentStart validates and appends a RUNNING experiment.
// Refusals are loud: bad shape, unknown profiles, a duplicate name,
// or a second running experiment on the same class (the arm hash is
// salted per experiment — two simultaneous splits of one class would
// fight over sessions).
func (s *Server) handleExperimentStart(w http.ResponseWriter, r *http.Request) {
	if s.opts.ConfigPath == "" {
		http.Error(w, "no config path configured", http.StatusConflict)
		return
	}
	var body struct {
		Name      string `json:"name"`
		Class     string `json:"class"`
		Control   string `json:"control"`
		Candidate string `json:"candidate"`
		Note      string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	exp := config.ExperimentConfig{
		Name:      strings.TrimSpace(body.Name),
		Class:     strings.TrimSpace(body.Class),
		Control:   strings.TrimSpace(body.Control),
		Candidate: strings.TrimSpace(body.Candidate),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Note:      body.Note,
	}
	if err := config.ValidateExperiment(exp); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	store := config.ProfileStore{Dir: config.DefaultProfilesDir(s.opts.ConfigPath)}
	for _, p := range []string{exp.Control, exp.Candidate} {
		if err := store.Validate(p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, e := range cfg.Experiments {
		if e.Name == exp.Name {
			http.Error(w, fmt.Sprintf("experiment %q already exists (stopped experiments keep their record — pick a new name)", exp.Name), http.StatusConflict)
			return
		}
		if e.Running() && e.Class == exp.Class {
			http.Error(w, fmt.Sprintf("experiment %q is already running on class %q — stop it first", e.Name, e.Class), http.StatusConflict)
			return
		}
	}
	cfg.Experiments = append(cfg.Experiments, exp)
	if err := config.WriteToml(s.opts.ConfigPath, cfg); err != nil {
		writeErr(w, err)
		return
	}
	if s.opts.OnConfigSaved != nil {
		s.opts.OnConfigSaved()
	}
	writeJSON(w, map[string]any{"started": exp, "hot": s.opts.OnConfigSaved != nil})
}

// handleExperimentStop serves POST /api/experiments/stop {"name"} —
// stamps StoppedAt, freezing the reporting window. The record stays
// for its report.
func (s *Server) handleExperimentStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	stopped := false
	for i := range cfg.Experiments {
		if cfg.Experiments[i].Name == body.Name {
			if !cfg.Experiments[i].Running() {
				http.Error(w, fmt.Sprintf("experiment %q is not running", body.Name), http.StatusConflict)
				return
			}
			cfg.Experiments[i].StoppedAt = time.Now().UTC().Format(time.RFC3339)
			stopped = true
			break
		}
	}
	if !stopped {
		http.Error(w, fmt.Sprintf("unknown experiment %q", body.Name), http.StatusNotFound)
		return
	}
	if err := config.WriteToml(s.opts.ConfigPath, cfg); err != nil {
		writeErr(w, err)
		return
	}
	if s.opts.OnConfigSaved != nil {
		s.opts.OnConfigSaved()
	}
	writeJSON(w, map[string]any{"stopped": body.Name})
}

// handleExperimentReport serves GET /api/experiments/report?name=N.
func (s *Server) handleExperimentReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("name")
	cfg, err := loadConfigForDashboard(s.opts.ConfigPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	var exp *config.ExperimentConfig
	for i := range cfg.Experiments {
		if cfg.Experiments[i].Name == name {
			exp = &cfg.Experiments[i]
			break
		}
	}
	if exp == nil {
		http.Error(w, fmt.Sprintf("unknown experiment %q", name), http.StatusNotFound)
		return
	}
	report, err := ComputeExperimentReport(r.Context(), s.db(), s.opts.CostEngine, *exp)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, report)
}

// ExperimentArmReport is one arm's aggregate evidence.
type ExperimentArmReport struct {
	Arm                   string           `json:"arm"`
	Profile               string           `json:"profile"`
	Sessions              int              `json:"sessions"`
	TotalCostUSD          float64          `json:"total_cost_usd"`
	MeanCostUSD           float64          `json:"mean_cost_usd"`
	SDCostUSD             float64          `json:"sd_cost_usd"`
	CVPct                 float64          `json:"cv_pct"`
	Turns                 int64            `json:"turns"`
	MeanTurns             float64          `json:"mean_turns"`
	CacheReadTokens       int64            `json:"cache_read_tokens"`
	CacheWriteTokens      int64            `json:"cache_write_tokens"`
	CompressionSavedBytes int64            `json:"compression_saved_bytes"`
	CacheEventsByCause    map[string]int64 `json:"cache_events_by_cause"`
}

// ExperimentReport is the full evidence bundle for one experiment.
type ExperimentReport struct {
	Experiment config.ExperimentConfig `json:"experiment"`
	Running    bool                    `json:"running"`
	WindowFrom string                  `json:"window_from"`
	WindowTo   string                  `json:"window_to"`
	Control    ExperimentArmReport     `json:"control"`
	Candidate  ExperimentArmReport     `json:"candidate"`
	// DeltaCostPct is candidate-vs-control on per-session means
	// (negative = candidate cheaper). Zero when either arm is empty.
	DeltaCostPct  float64 `json:"delta_cost_pct"`
	DeltaTurnsPct float64 `json:"delta_turns_pct"`
}

// ComputeExperimentReport recomputes arm membership by hash and
// aggregates the §2.4 evidence per arm. Shared by the dashboard
// handler and `observer experiment report` so the two surfaces can
// never disagree.
//
//nolint:gocyclo // one sequential aggregation pass per evidence dimension; splitting would scatter the §2.4 report shape (P6 arc).
func ComputeExperimentReport(ctx context.Context, db *sql.DB, engine costLookup, exp config.ExperimentConfig) (*ExperimentReport, error) {
	now := time.Now().UTC()
	start, end, err := config.ExperimentWindow(exp, now)
	if err != nil {
		return nil, err
	}
	rep := &ExperimentReport{
		Experiment: exp,
		Running:    exp.Running(),
		WindowFrom: start.Format(time.RFC3339),
		WindowTo:   end.Format(time.RFC3339),
		Control:    ExperimentArmReport{Arm: "control", Profile: exp.Control, CacheEventsByCause: map[string]int64{}},
		Candidate:  ExperimentArmReport{Arm: "candidate", Profile: exp.Candidate, CacheEventsByCause: map[string]int64{}},
	}
	if db == nil {
		return rep, nil
	}

	// Population: sessions that STARTED inside the window and belong
	// to the class. Sessions started before the experiment resolved
	// their profile under the pre-experiment assignment — counting
	// them would attribute old-assignment traffic to an arm.
	startArg := start.Format(time.RFC3339Nano)
	endArg := end.Add(time.Second).Format(time.RFC3339Nano)
	var rows *sql.Rows
	if toolName, ok := strings.CutPrefix(exp.Class, "tool:"); ok {
		rows, err = db.QueryContext(ctx,
			`SELECT id FROM sessions WHERE tool = ? AND started_at >= ? AND started_at <= ?`,
			toolName, startArg, endArg)
	} else {
		rows, err = db.QueryContext(ctx,
			`SELECT DISTINCT s.id FROM sessions s
			 JOIN api_turns t ON t.session_id = s.id AND t.provider = ?
			 WHERE s.started_at >= ? AND s.started_at <= ?`,
			exp.Class, startArg, endArg)
	}
	if err != nil {
		return nil, err
	}
	armOf := map[string]string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			_, arm := config.ExperimentArm(exp, id)
			armOf[id] = arm
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Per-session rollups over the window (shared proxy-dedup shape).
	type sess struct {
		cost  float64
		turns int64
	}
	perSession := map[string]*sess{}
	crows, err := db.QueryContext(ctx,
		`WITH proxy_turn_ids AS (
			SELECT request_id FROM api_turns
			 WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ? AND timestamp <= ?
		),
		combined AS (
			SELECT at.session_id, at.model, at.input_tokens, at.output_tokens,
			       at.cache_read_tokens, at.cache_creation_tokens, at.cache_creation_1h_tokens,
			       0 AS reasoning_tokens, at.web_search_requests, at.cost_usd,
			       COALESCE(at.compression_original_bytes, 0) - COALESCE(at.compression_compressed_bytes, 0) AS comp_saved
			FROM api_turns at
			WHERE at.timestamp >= ? AND at.timestamp <= ?
			  AND (at.error_class IS NULL OR at.error_class = '')
			UNION ALL
			SELECT tu.session_id, tu.model, tu.input_tokens, tu.output_tokens,
			       tu.cache_read_tokens, tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
			       tu.reasoning_tokens, tu.web_search_requests, tu.estimated_cost_usd, 0
			FROM token_usage tu
			WHERE tu.timestamp >= ? AND tu.timestamp <= ?
			  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
			       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
		)
		SELECT COALESCE(session_id, ''), COALESCE(model, ''),
		       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
		       COALESCE(cache_creation_1h_tokens, 0), COALESCE(reasoning_tokens, 0),
		       COALESCE(web_search_requests, 0), COALESCE(cost_usd, 0), COALESCE(comp_saved, 0)
		FROM combined`,
		startArg, endArg, startArg, endArg, startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var (
			sid, model string
			bundle     cost.TokenBundle
			rec        float64
			compSaved  int64
		)
		if crows.Scan(&sid, &model, &bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning, &bundle.WebSearchRequests, &rec, &compSaved) != nil {
			continue
		}
		arm, ok := armOf[sid]
		if !ok {
			continue
		}
		ar := rep.armFor(arm)
		ps := perSession[sid]
		if ps == nil {
			ps = &sess{}
			perSession[sid] = ps
		}
		rowCost := rec
		if rowCost <= 0 && engine != nil {
			if p, ok := engine.Lookup(model); ok {
				rowCost = cost.Compute(p, bundle)
			}
		}
		ps.cost += rowCost
		ps.turns++
		ar.Turns++
		ar.TotalCostUSD += rowCost
		ar.CacheReadTokens += bundle.CacheRead
		ar.CacheWriteTokens += bundle.CacheCreation + bundle.CacheCreation1h
		ar.CompressionSavedBytes += compSaved
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}

	// Cache events by cause, window-scoped, attributed by session arm.
	erows, err := db.QueryContext(ctx,
		`SELECT COALESCE(session_id, ''), COALESCE(cause, '(none)'), COUNT(*)
		 FROM cache_events WHERE timestamp >= ? AND timestamp <= ?
		 GROUP BY session_id, cause`, startArg, endArg)
	if err == nil {
		for erows.Next() {
			var sid, cause string
			var n int64
			if erows.Scan(&sid, &cause, &n) == nil {
				if arm, ok := armOf[sid]; ok {
					rep.armFor(arm).CacheEventsByCause[cause] += n
				}
			}
		}
		_ = erows.Err()
		erows.Close()
	}

	// Per-arm session stats (mean / sd / CV over per-session cost).
	costsByArm := map[string][]float64{}
	turnsByArm := map[string]int64{}
	for sid, ps := range perSession {
		arm := armOf[sid]
		costsByArm[arm] = append(costsByArm[arm], ps.cost)
		turnsByArm[arm] += ps.turns
	}
	for _, arm := range []string{"control", "candidate"} {
		ar := rep.armFor(arm)
		costs := costsByArm[arm]
		ar.Sessions = len(costs)
		if len(costs) == 0 {
			continue
		}
		var sum float64
		for _, c := range costs {
			sum += c
		}
		ar.MeanCostUSD = sum / float64(len(costs))
		ar.MeanTurns = float64(turnsByArm[arm]) / float64(len(costs))
		if len(costs) >= 2 {
			var ss float64
			for _, c := range costs {
				d := c - ar.MeanCostUSD
				ss += d * d
			}
			ar.SDCostUSD = math.Sqrt(ss / float64(len(costs)-1))
			if ar.MeanCostUSD > 0 {
				ar.CVPct = ar.SDCostUSD / ar.MeanCostUSD * 100
			}
		}
	}
	if rep.Control.MeanCostUSD > 0 && rep.Candidate.Sessions > 0 {
		rep.DeltaCostPct = (rep.Candidate.MeanCostUSD - rep.Control.MeanCostUSD) / rep.Control.MeanCostUSD * 100
	}
	if rep.Control.MeanTurns > 0 && rep.Candidate.Sessions > 0 {
		rep.DeltaTurnsPct = (rep.Candidate.MeanTurns - rep.Control.MeanTurns) / rep.Control.MeanTurns * 100
	}
	return rep, nil
}

// armFor maps an arm label to its report slot.
func (r *ExperimentReport) armFor(arm string) *ExperimentArmReport {
	if arm == "candidate" {
		return &r.Candidate
	}
	return &r.Control
}

// costLookup is the cost-engine capability the report needs — the
// same Lookup the analysis surfaces use. Interface so the CLI can
// pass its own engine instance.
type costLookup interface {
	Lookup(model string) (cost.Pricing, bool)
}
