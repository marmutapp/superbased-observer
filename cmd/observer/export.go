package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newExportCmd wires `observer export` — dump actions / sessions / api_turns /
// failures as JSON or CSV for external analysis (spec §15, Phase 4).
func newExportCmd() *cobra.Command {
	var (
		configPath  string
		format      string
		table       string
		projectRoot string
		days        int
		output      string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Dump tables as JSON or CSV for external analysis",
		Long: "Streams rows from actions / sessions / api_turns / failures to stdout\n" +
			"or to --out. No aggregation, no scrubbing beyond what ingestion already\n" +
			"did — the raw row shape is what external analysts typically want.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, database, cleanup, err := loadConfigAndDB(cmd.Context(), configPath)
			if err != nil {
				return err
			}
			defer cleanup()

			var w io.Writer = cmd.OutOrStdout()
			if output != "" {
				f, err := os.Create(output)
				if err != nil {
					return fmt.Errorf("open --out: %w", err)
				}
				defer f.Close()
				w = f
			}

			return runExport(cmd.Context(), database, w, exportOptions{
				Format: format, Table: table, ProjectRoot: projectRoot, Days: days,
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to config.toml")
	cmd.Flags().StringVar(&format, "format", "json", "Output format: json (default, newline-delimited) or csv")
	cmd.Flags().StringVar(&table, "table", "actions", "Which table to export: actions | sessions | api_turns | failures")
	cmd.Flags().StringVar(&projectRoot, "project", "", "Filter to a project root path")
	cmd.Flags().IntVar(&days, "days", 0, "Restrict to the last N days (0 disables)")
	cmd.Flags().StringVar(&output, "out", "", "Write to file path instead of stdout")
	return cmd
}

type exportOptions struct {
	Format      string
	Table       string
	ProjectRoot string
	Days        int
}

func runExport(ctx context.Context, db *sql.DB, w io.Writer, opts exportOptions) error {
	var (
		query   string
		headers []string
	)
	var since time.Time
	if opts.Days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -opts.Days)
	}

	switch strings.ToLower(opts.Table) {
	case "actions":
		query, headers = actionsQuery(opts.ProjectRoot, since)
	case "sessions":
		query, headers = sessionsQuery(opts.ProjectRoot, since)
	case "api_turns":
		query, headers = apiTurnsQuery(opts.ProjectRoot, since)
	case "failures":
		query, headers = failuresQuery(opts.ProjectRoot, since)
	default:
		return fmt.Errorf("--table %q not in {actions, sessions, api_turns, failures}", opts.Table)
	}

	queryArgs := exportArgs(opts.ProjectRoot, since)
	rows, err := db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return fmt.Errorf("export: query: %w", err)
	}
	defer rows.Close()

	switch strings.ToLower(opts.Format) {
	case "json":
		return writeJSONLines(w, rows, headers)
	case "csv":
		return writeCSV(w, rows, headers)
	default:
		return fmt.Errorf("--format %q not in {json, csv}", opts.Format)
	}
}

func exportArgs(projectRoot string, since time.Time) []any {
	var args []any
	if projectRoot != "" {
		args = append(args, projectRoot)
	}
	if !since.IsZero() {
		args = append(args, since.Format(time.RFC3339Nano))
	}
	return args
}

// queryBuilder glues WHERE filters together.
func filterClause(projectRoot string, since time.Time, tsColumn, rootColumn string) string {
	var parts []string
	if projectRoot != "" {
		parts = append(parts, rootColumn+" = ?")
	}
	if !since.IsZero() {
		parts = append(parts, tsColumn+" >= ?")
	}
	if len(parts) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(parts, " AND ")
}

func actionsQuery(projectRoot string, since time.Time) (string, []string) {
	headers := []string{
		"id", "session_id", "project_root", "timestamp", "tool", "action_type",
		"target", "success", "error_message", "duration_ms", "content_hash",
		"freshness",
	}
	q := `SELECT a.id, a.session_id, COALESCE(p.root_path, ''), a.timestamp,
	             a.tool, a.action_type, COALESCE(a.target, ''),
	             a.success, COALESCE(a.error_message, ''),
	             COALESCE(a.duration_ms, 0), COALESCE(a.content_hash, ''),
	             COALESCE(a.freshness, '')
	      FROM actions a LEFT JOIN projects p ON p.id = a.project_id` +
		filterClause(projectRoot, since, "a.timestamp", "p.root_path") +
		" ORDER BY a.timestamp"
	return q, headers
}

func sessionsQuery(projectRoot string, since time.Time) (string, []string) {
	headers := []string{
		"id", "project_root", "tool", "model", "git_branch", "started_at", "ended_at",
		"total_actions", "quality_score", "error_rate", "redundancy_ratio",
		"turns_to_first_edit",
	}
	q := `SELECT s.id, COALESCE(p.root_path, ''), s.tool, COALESCE(s.model, ''),
	             COALESCE(s.git_branch, ''), s.started_at, COALESCE(s.ended_at, ''),
	             s.total_actions,
	             COALESCE(s.quality_score, 0), COALESCE(s.error_rate, 0),
	             COALESCE(s.redundancy_ratio, 0), COALESCE(s.turns_to_first_edit, -1)
	      FROM sessions s LEFT JOIN projects p ON p.id = s.project_id` +
		filterClause(projectRoot, since, "s.started_at", "p.root_path") +
		" ORDER BY s.started_at"
	return q, headers
}

func apiTurnsQuery(projectRoot string, since time.Time) (string, []string) {
	headers := []string{
		"id", "session_id", "project_root", "timestamp", "provider", "model",
		"input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens",
		"cost_usd", "message_count", "tool_use_count", "time_to_first_token_ms",
		"total_response_ms", "stop_reason",
	}
	q := `SELECT at.id, COALESCE(at.session_id, ''), COALESCE(p.root_path, ''),
	             at.timestamp, at.provider, at.model,
	             at.input_tokens, at.output_tokens,
	             COALESCE(at.cache_read_tokens, 0), COALESCE(at.cache_creation_tokens, 0),
	             COALESCE(at.cost_usd, 0),
	             COALESCE(at.message_count, 0), COALESCE(at.tool_use_count, 0),
	             COALESCE(at.time_to_first_token_ms, 0), COALESCE(at.total_response_ms, 0),
	             COALESCE(at.stop_reason, '')
	      FROM api_turns at LEFT JOIN projects p ON p.id = at.project_id` +
		filterClause(projectRoot, since, "at.timestamp", "p.root_path") +
		" ORDER BY at.timestamp"
	return q, headers
}

func failuresQuery(projectRoot string, since time.Time) (string, []string) {
	headers := []string{
		"id", "session_id", "project_root", "timestamp", "command_hash",
		"command_summary", "error_category", "error_message", "retry_count",
		"eventually_succeeded",
	}
	q := `SELECT fc.id, fc.session_id, COALESCE(p.root_path, ''), fc.timestamp,
	             fc.command_hash, fc.command_summary,
	             COALESCE(fc.error_category, ''), COALESCE(fc.error_message, ''),
	             COALESCE(fc.retry_count, 0), COALESCE(fc.eventually_succeeded, 0)
	      FROM failure_context fc LEFT JOIN projects p ON p.id = fc.project_id` +
		filterClause(projectRoot, since, "fc.timestamp", "p.root_path") +
		" ORDER BY fc.timestamp"
	return q, headers
}

// writeJSONLines writes NDJSON — one JSON object per row, newline-delimited.
// Easier to stream than a single-array JSON.
func writeJSONLines(w io.Writer, rows *sql.Rows, headers []string) error {
	enc := json.NewEncoder(w)
	for rows.Next() {
		m, err := scanToMap(rows, headers)
		if err != nil {
			return err
		}
		if err := enc.Encode(m); err != nil {
			return fmt.Errorf("export: write json: %w", err)
		}
	}
	return rows.Err()
}

func writeCSV(w io.Writer, rows *sql.Rows, headers []string) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()
	if err := cw.Write(headers); err != nil {
		return fmt.Errorf("export: write csv header: %w", err)
	}
	for rows.Next() {
		m, err := scanToMap(rows, headers)
		if err != nil {
			return err
		}
		record := make([]string, len(headers))
		for i, h := range headers {
			record[i] = toString(m[h])
		}
		if err := cw.Write(record); err != nil {
			return fmt.Errorf("export: write csv row: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}

// scanToMap reflects the row columns into a generic map keyed by the header
// list. Works because we only ship values that fit into the small set of
// types below (no blob / json.RawMessage / structured SQLite types).
func scanToMap(rows *sql.Rows, headers []string) (map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, fmt.Errorf("export: scan: %w", err)
	}
	m := make(map[string]any, len(cols))
	key := headers
	if len(key) != len(cols) {
		key = cols
	}
	for i, c := range cols {
		k := c
		if i < len(key) {
			k = key[i]
		}
		m[k] = normalize(vals[i])
	}
	return m, nil
}

func normalize(v any) any {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case nil:
		return nil
	default:
		return t
	}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		return fmt.Sprintf("%g", t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}
