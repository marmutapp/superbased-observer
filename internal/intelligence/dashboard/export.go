package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"
)

// handleExportXLSX serves /api/export.xlsx — a multi-sheet workbook
// covering Cost / Sessions / Actions / Tools / Compression / Discovery
// / Patterns. Honors ?days=N&tool=X&project=Y exactly like the JSON
// endpoints.
//
// Implementation reuses the in-process Handler so each sheet's row
// shape stays automatically aligned with the JSON contract — no
// duplicate SQL.
func (s *Server) handleExportXLSX(w http.ResponseWriter, r *http.Request) {
	days := intArg(r, "days", 30, 1, 36500)
	tool := r.URL.Query().Get("tool")
	project := r.URL.Query().Get("project")

	// Common query string for the inner JSON calls.
	commonQS := url.Values{}
	commonQS.Set("days", strconv.Itoa(days))
	if tool != "" {
		commonQS.Set("tool", tool)
	}
	if project != "" {
		commonQS.Set("project", project)
	}

	handler := s.Handler()
	fetch := func(path string, extra url.Values) (map[string]any, error) {
		qs := url.Values{}
		for k, v := range commonQS {
			qs[k] = append(qs[k], v...)
		}
		for k, v := range extra {
			qs[k] = append(qs[k], v...)
		}
		u := path
		if encoded := qs.Encode(); encoded != "" {
			u += "?" + encoded
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, u, nil)
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			return nil, fmt.Errorf("inner %s -> %d: %s", u, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode %s: %w", u, err)
		}
		return out, nil
	}

	f := excelize.NewFile()
	defer f.Close()

	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill: excelize.Fill{Type: "pattern", Pattern: 1, Color: []string{"#1F2937"}},
	})

	writeSheet := func(name string, header []string, rows [][]any) {
		idx, err := f.NewSheet(name)
		if err != nil {
			return
		}
		for i, h := range header {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			f.SetCellValue(name, cell, h)
		}
		if len(header) > 0 {
			endCell, _ := excelize.CoordinatesToCellName(len(header), 1)
			startCell, _ := excelize.CoordinatesToCellName(1, 1)
			f.SetCellStyle(name, startCell, endCell, headerStyle)
		}
		for ri, row := range rows {
			for ci, v := range row {
				cell, _ := excelize.CoordinatesToCellName(ci+1, ri+2)
				f.SetCellValue(name, cell, v)
			}
		}
		f.SetActiveSheet(idx)
	}

	// --- Sheet: Overview (filter context + KPI tiles) ---
	if status, err := fetch("/api/status", nil); err == nil {
		counts, _ := status["counts"].(map[string]any)
		writeSheet("Overview", []string{"Metric", "Value", "Note"}, [][]any{
			{"Window (days)", days, "filter applied to all sheets"},
			{"Tool filter", coalesceStr(tool, "all"), ""},
			{"Project filter", coalesceStr(project, "all"), ""},
			{"DB schema version", status["schema_version"], ""},
			{"DB path", status["db_path"], ""},
			{"Sessions (total)", numFromAny(counts["sessions"]), "all-time"},
			{"API turns (proxy)", numFromAny(counts["api_turns"]), "accurate token source"},
			{"Token rows (jsonl)", numFromAny(counts["token_usage"]), "plentiful, deduped at rollup"},
			{"Failures (24h)", numFromAny(status["recent_failures_24h"]), ""},
			{"Last action", status["last_action_at"], ""},
			{"Exported at", time.Now().UTC().Format(time.RFC3339), ""},
		})
	}

	// --- Sheet: Cost (per model) ---
	if costJSON, err := fetch("/api/cost", url.Values{"group_by": []string{"model"}}); err == nil {
		header := []string{"Model", "Net Input", "Cache Read", "Cache Write", "Output", "Cost (USD)", "Turns", "Source", "Reliability"}
		var out [][]any
		if rows, ok := costJSON["rows"].([]any); ok {
			for _, r := range rows {
				row, _ := r.(map[string]any)
				tokens, _ := row["tokens"].(map[string]any)
				out = append(out, []any{
					row["key"],
					numFromAny(tokens["input"]),
					numFromAny(tokens["cache_read"]),
					numFromAny(tokens["cache_creation"]),
					numFromAny(tokens["output"]),
					numFromAny(row["cost_usd"]),
					numFromAny(row["turn_count"]),
					row["source"],
					row["reliability"],
				})
			}
		}
		writeSheet("Cost", header, out)
	}

	// --- Sheet: Sessions ---
	{
		var rowsAll []map[string]any
		page := 1
		for page <= 20 { // cap at 5000 rows total
			s, err := fetch("/api/sessions", url.Values{"page": []string{strconv.Itoa(page)}, "limit": []string{"250"}})
			if err != nil {
				break
			}
			rowsArr, _ := s["rows"].([]any)
			if len(rowsArr) == 0 {
				break
			}
			for _, r := range rowsArr {
				if m, ok := r.(map[string]any); ok {
					rowsAll = append(rowsAll, m)
				}
			}
			total, _ := s["total"].(float64)
			if len(rowsAll) >= int(total) {
				break
			}
			page++
		}
		header := []string{"ID", "Tool", "Project", "Started", "Ended", "Total actions", "Quality", "Errors", "Redundancy"}
		var out [][]any
		for _, r := range rowsAll {
			out = append(out, []any{
				r["id"], r["tool"], r["project"],
				r["started_at"], r["ended_at"],
				numFromAny(r["total_actions"]),
				numFromAny(r["quality_score"]),
				numFromAny(r["error_rate"]),
				numFromAny(r["redundancy_ratio"]),
			})
		}
		writeSheet("Sessions", header, out)
	}

	// --- Sheet: Actions (capped at 5000 rows) ---
	{
		var rowsAll []map[string]any
		page := 1
		for page <= 100 {
			a, err := fetch("/api/actions", url.Values{"page": []string{strconv.Itoa(page)}, "limit": []string{"500"}})
			if err != nil {
				break
			}
			rowsArr, _ := a["rows"].([]any)
			if len(rowsArr) == 0 {
				break
			}
			for _, r := range rowsArr {
				if m, ok := r.(map[string]any); ok {
					rowsAll = append(rowsAll, m)
				}
			}
			if len(rowsAll) >= 5000 {
				rowsAll = rowsAll[:5000]
				break
			}
			total, _ := a["total"].(float64)
			if len(rowsAll) >= int(total) {
				break
			}
			page++
		}
		header := []string{"Timestamp", "Tool", "Type", "Tool name", "Target", "Session", "OK", "Error"}
		var out [][]any
		for _, r := range rowsAll {
			ok := "yes"
			if v, _ := r["success"].(bool); !v {
				ok = "no"
			}
			out = append(out, []any{
				r["timestamp"], r["tool"], r["action_type"],
				r["raw_tool_name"], r["target"], r["session_id"],
				ok, r["error_message"],
			})
		}
		writeSheet("Actions", header, out)
	}

	// --- Sheet: Tools ---
	if toolsJSON, err := fetch("/api/tools", nil); err == nil {
		header := []string{"Tool", "Actions", "Failures", "Success rate", "Sessions", "First seen", "Last seen"}
		var out [][]any
		if rows, ok := toolsJSON["tools"].([]any); ok {
			for _, r := range rows {
				row, _ := r.(map[string]any)
				out = append(out, []any{
					row["tool"],
					numFromAny(row["action_count"]),
					numFromAny(row["failure_count"]),
					numFromAny(row["success_rate"]),
					numFromAny(row["session_count"]),
					row["first_seen"],
					row["last_seen"],
				})
			}
		}
		writeSheet("Tools", header, out)
	}

	// --- Sheet: Compression (per model with original_bytes > 0) ---
	if costJSON, err := fetch("/api/cost", url.Values{"group_by": []string{"model"}}); err == nil {
		header := []string{"Model", "Original bytes", "Compressed bytes", "Saved bytes", "Saved %", "Turns", "Tool results", "Dropped", "Markers"}
		var out [][]any
		if rows, ok := costJSON["rows"].([]any); ok {
			for _, r := range rows {
				row, _ := r.(map[string]any)
				comp, _ := row["compression"].(map[string]any)
				if comp == nil {
					continue
				}
				orig := numFromAny(comp["original_bytes"])
				cmp := numFromAny(comp["compressed_bytes"])
				if orig == 0 {
					continue
				}
				savedPct := 0.0
				if orig > 0 {
					savedPct = float64(orig-cmp) / float64(orig)
				}
				out = append(out, []any{
					row["key"], orig, cmp, orig - cmp, savedPct,
					numFromAny(comp["turns"]),
					numFromAny(comp["compressed_count"]),
					numFromAny(comp["dropped_count"]),
					numFromAny(comp["marker_count"]),
				})
			}
		}
		writeSheet("Compression", header, out)
	}

	// --- Sheet: Discovery (stale rereads + repeated commands) ---
	if discJSON, err := fetch("/api/discover", url.Values{"limit": []string{"500"}}); err == nil {
		// stale rereads
		header := []string{"File", "Reads", "Stale", "Est. wasted tokens", "Project"}
		var out [][]any
		if rows, ok := discJSON["stale_reads"].([]any); ok {
			for _, r := range rows {
				row, _ := r.(map[string]any)
				out = append(out, []any{
					row["file_path"],
					numFromAny(row["total_reads"]),
					numFromAny(row["stale_count"]),
					numFromAny(row["est_wasted_tokens"]),
					row["project"],
				})
			}
		}
		writeSheet("Discovery — stale rereads", header, out)

		header2 := []string{"Command", "Total runs", "No-change reruns", "Successful", "Failed", "Project"}
		var out2 [][]any
		if rows, ok := discJSON["repeated_commands"].([]any); ok {
			for _, r := range rows {
				row, _ := r.(map[string]any)
				out2 = append(out2, []any{
					row["command"],
					numFromAny(row["total_runs"]),
					numFromAny(row["no_change_reruns"]),
					numFromAny(row["successful_runs"]),
					numFromAny(row["failed_runs"]),
					row["project"],
				})
			}
		}
		writeSheet("Discovery — repeated commands", header2, out2)
	}

	// --- Sheet: Patterns ---
	if pat, err := fetch("/api/patterns", url.Values{"limit": []string{"500"}}); err == nil {
		// /api/patterns returns a bare array, not an object — handle both.
		var rows []any
		switch v := any(pat).(type) {
		case map[string]any:
			if r, ok := v["rows"].([]any); ok {
				rows = r
			}
		}
		// fallback: re-fetch as raw decoded array
		if rows == nil {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/patterns?limit=500", nil))
			if rec.Code == 200 {
				var arr []any
				if err := json.NewDecoder(rec.Body).Decode(&arr); err == nil {
					rows = arr
				}
			}
		}
		header := []string{"Type", "Project", "Confidence", "Observations", "Data"}
		var out [][]any
		for _, r := range rows {
			row, _ := r.(map[string]any)
			out = append(out, []any{
				row["pattern_type"], row["project"],
				numFromAny(row["confidence"]),
				numFromAny(row["observation_count"]),
				stringifyData(row["data"]),
			})
		}
		writeSheet("Patterns", header, out)
	}

	// excelize creates a default "Sheet1" that we never populate; remove
	// it so the workbook opens on Overview.
	if idx, _ := f.GetSheetIndex("Sheet1"); idx >= 0 {
		f.DeleteSheet("Sheet1")
	}
	if idx, _ := f.GetSheetIndex("Overview"); idx >= 0 {
		f.SetActiveSheet(idx)
	}

	filename := fmt.Sprintf("observer-export-%s.xlsx", time.Now().UTC().Format("20060102-1504"))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if err := f.Write(w); err != nil {
		s.opts.Logger.Error("export.xlsx write", "err", err)
	}
}

// numFromAny coerces a JSON number (float64) or numeric string into an
// int64 for sheet rendering. Returns 0 for nil/unrecognised.
func numFromAny(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func coalesceStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func stringifyData(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
