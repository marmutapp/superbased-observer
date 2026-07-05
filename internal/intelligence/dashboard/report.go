package dashboard

import (
	"net/http"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Monthly cost statement (usability arc P6.6 / review §8.2): one
// endpoint returning everything the printable statement renders —
// totals, spend by model / tool / project, savings telemetry, and the
// most expensive sessions, for one calendar month and (optionally)
// one project. The web page renders it as a clean document; printing
// to PDF is the browser's job. Read-only, computed on demand.

type reportRow struct {
	Key      string  `json:"key"`
	CostUSD  float64 `json:"cost_usd"`
	Turns    int64   `json:"turns"`
	Sessions int     `json:"sessions,omitempty"`
}

type reportSession struct {
	ID        string  `json:"id"`
	Tool      string  `json:"tool"`
	Project   string  `json:"project,omitempty"`
	StartedAt string  `json:"started_at"`
	CostUSD   float64 `json:"cost_usd"`
	Turns     int64   `json:"turns"`
}

// handleReportMonthly serves GET /api/report/monthly?month=YYYY-MM
// [&project=<root>]. month defaults to the current calendar month;
// an unparseable month is a 400.
func (s *Server) handleReportMonthly(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	monthStr := r.URL.Query().Get("month")
	if monthStr == "" {
		monthStr = now.Format("2006-01")
	}
	month, err := time.Parse("2006-01", monthStr)
	if err != nil {
		http.Error(w, "month must be YYYY-MM", http.StatusBadRequest)
		return
	}
	project := r.URL.Query().Get("project")
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	startArg := start.Format(time.RFC3339Nano)
	endArg := end.Format(time.RFC3339Nano)

	resp := map[string]any{
		"month":        monthStr,
		"project":      project,
		"generated_at": now.Format(time.RFC3339),
	}
	if s.db() == nil {
		writeJSON(w, resp)
		return
	}

	// Combined dedup rows for the month, joined through sessions for
	// tool/project attribution (both arms bucket identically).
	args := []any{startArg, endArg, startArg, endArg, startArg, endArg}
	projectWhere := ""
	if project != "" {
		projectWhere = " WHERE COALESCE(p.root_path, '') = ?"
		args = append(args, project)
	}
	//nolint:gosec // G202: SQL fragments are code constants; values bound via args.
	rows, err := s.db().QueryContext(r.Context(),
		`WITH proxy_turn_ids AS (
			SELECT request_id FROM api_turns
			 WHERE request_id IS NOT NULL AND request_id != '' AND timestamp >= ? AND timestamp < ?
		),
		combined AS (
			SELECT at.session_id, at.model, at.input_tokens, at.output_tokens,
			       at.cache_read_tokens, at.cache_creation_tokens, at.cache_creation_1h_tokens,
			       0 AS reasoning_tokens, at.web_search_requests, at.cost_usd,
			       COALESCE(at.compression_original_bytes, 0) - COALESCE(at.compression_compressed_bytes, 0) AS comp_saved
			FROM api_turns at
			WHERE at.timestamp >= ? AND at.timestamp < ?
			  AND (at.error_class IS NULL OR at.error_class = '')
			UNION ALL
			SELECT tu.session_id, tu.model, tu.input_tokens, tu.output_tokens,
			       tu.cache_read_tokens, tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
			       tu.reasoning_tokens, tu.web_search_requests, tu.estimated_cost_usd, 0
			FROM token_usage tu
			WHERE tu.timestamp >= ? AND tu.timestamp < ?
			  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
			       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
		)
		SELECT COALESCE(c.session_id, ''), COALESCE(s.tool, ''), COALESCE(p.root_path, ''),
		       COALESCE(s.started_at, ''), COALESCE(c.model, ''),
		       COALESCE(c.input_tokens, 0), COALESCE(c.output_tokens, 0),
		       COALESCE(c.cache_read_tokens, 0), COALESCE(c.cache_creation_tokens, 0),
		       COALESCE(c.cache_creation_1h_tokens, 0), COALESCE(c.reasoning_tokens, 0),
		       COALESCE(c.web_search_requests, 0), COALESCE(c.cost_usd, 0), COALESCE(c.comp_saved, 0)
		FROM combined c
		LEFT JOIN sessions s ON s.id = c.session_id
		LEFT JOIN projects p ON p.id = s.project_id`+projectWhere,
		args...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()

	type tot struct {
		cost                                 float64
		turns                                int64
		input, output, cacheRead, cacheWrite int64
		compSaved                            int64
	}
	var totals tot
	byModel := map[string]*reportRow{}
	byTool := map[string]*reportRow{}
	byProject := map[string]*reportRow{}
	type sessAgg struct {
		tool, project, started string
		cost                   float64
		turns                  int64
	}
	bySession := map[string]*sessAgg{}

	for rows.Next() {
		var (
			sid, tool, root, started, model string
			bundle                          cost.TokenBundle
			rec                             float64
			compSaved                       int64
		)
		if rows.Scan(&sid, &tool, &root, &started, &model,
			&bundle.Input, &bundle.Output, &bundle.CacheRead, &bundle.CacheCreation,
			&bundle.CacheCreation1h, &bundle.Reasoning, &bundle.WebSearchRequests,
			&rec, &compSaved) != nil {
			continue
		}
		rowCost := rec
		if rowCost <= 0 && s.opts.CostEngine != nil {
			if p, ok := s.opts.CostEngine.Lookup(model); ok {
				rowCost = cost.Compute(p, bundle)
			}
		}
		totals.cost += rowCost
		totals.turns++
		totals.input += bundle.Input
		totals.output += bundle.Output
		totals.cacheRead += bundle.CacheRead
		totals.cacheWrite += bundle.CacheCreation + bundle.CacheCreation1h
		totals.compSaved += compSaved
		bump := func(m map[string]*reportRow, key string) {
			if key == "" {
				key = "(unattributed)"
			}
			row := m[key]
			if row == nil {
				row = &reportRow{Key: key}
				m[key] = row
			}
			row.CostUSD += rowCost
			row.Turns++
		}
		bump(byModel, model)
		bump(byTool, tool)
		bump(byProject, root)
		if sid != "" {
			sa := bySession[sid]
			if sa == nil {
				sa = &sessAgg{tool: tool, project: root, started: started}
				bySession[sid] = sa
			}
			sa.cost += rowCost
			sa.turns++
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}

	// Session counts per tool/project ride the per-session aggregation.
	for _, sa := range bySession {
		key := sa.tool
		if key == "" {
			key = "(unattributed)"
		}
		if row := byTool[key]; row != nil {
			row.Sessions++
		}
		key = sa.project
		if key == "" {
			key = "(unattributed)"
		}
		if row := byProject[key]; row != nil {
			row.Sessions++
		}
	}

	flat := func(m map[string]*reportRow) []reportRow {
		out := make([]reportRow, 0, len(m))
		for _, r := range m {
			out = append(out, *r)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
		return out
	}
	topSessions := make([]reportSession, 0, len(bySession))
	for id, sa := range bySession {
		topSessions = append(topSessions, reportSession{
			ID: id, Tool: sa.tool, Project: sa.project,
			StartedAt: sa.started, CostUSD: sa.cost, Turns: sa.turns,
		})
	}
	sort.Slice(topSessions, func(i, j int) bool { return topSessions[i].CostUSD > topSessions[j].CostUSD })
	if len(topSessions) > 25 {
		topSessions = topSessions[:25]
	}

	resp["totals"] = map[string]any{
		"cost_usd": totals.cost,
		"sessions": len(bySession),
		"turns":    totals.turns,
		"tokens": map[string]int64{
			"input": totals.input, "output": totals.output,
			"cache_read": totals.cacheRead, "cache_write": totals.cacheWrite,
		},
	}
	resp["by_model"] = flat(byModel)
	resp["by_tool"] = flat(byTool)
	if project == "" {
		resp["by_project"] = flat(byProject)
	}
	resp["savings"] = map[string]any{
		"compression_bytes":  totals.compSaved,
		"compression_tokens": totals.compSaved / 4,
		"cache_read_tokens":  totals.cacheRead,
	}
	resp["top_sessions"] = topSessions
	writeJSON(w, resp)
}
