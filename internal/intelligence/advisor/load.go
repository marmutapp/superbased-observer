package advisor

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// minSessionRows is the floor below which a session doesn't enter Facts at
// all — no detector has anything to say about a 4-row session.
const minSessionRows = 5

// LoadFacts builds the Facts bundle for one engine run. The substrate is
// the deduped proxy∪JSONL union (calibration §1: an api_turns-only loader
// yields zero suggestions on a watcher-dominated corpus): api_turns rows
// load first (high fidelity — fast flag, proxy timing), then token_usage
// rows that duplicate a proxy turn are dropped by the same two-level rule
// the cost engine applies (request_id == source_event_id, then the
// minute-less session shape key — canonical semantics in
// cost/summary.go::loadRows, mirrored here read-only).
func LoadFacts(ctx context.Context, db *sql.DB, opts Options) (*Facts, error) {
	now := opts.now()
	since := now.AddDate(0, 0, -opts.WindowDays).UTC().Format(time.RFC3339)

	sessions := map[string]*SessionFacts{}

	// Proxy rows first — they win dedup.
	turnIDs := map[string]map[string]bool{}   // session → request_id set
	shapeKeys := map[string]map[string]bool{} // session → shape-key set
	proxyQ := `
		SELECT at.session_id, COALESCE(s.tool,''), COALESCE(s.model,''), COALESCE(p.root_path,''),
		       at.timestamp, COALESCE(at.model,''), COALESCE(at.request_id,''),
		       COALESCE(at.input_tokens,0), COALESCE(at.output_tokens,0),
		       COALESCE(at.cache_read_tokens,0), COALESCE(at.cache_creation_tokens,0),
		       COALESCE(at.cache_creation_1h_tokens,0), 0 AS reasoning_tokens, COALESCE(at.fast,0),
		       COALESCE(at.compression_original_bytes,0), COALESCE(at.compression_compressed_bytes,0)
		FROM api_turns at
		JOIN sessions s ON s.id = at.session_id
		LEFT JOIN projects p ON p.id = s.project_id
		WHERE at.timestamp >= ?` + scopeFilter(opts) + `
		ORDER BY at.session_id, at.timestamp`
	if err := loadRows(ctx, db, proxyQ, since, opts, func(sid, tool, smodel, root, ts, model, eventID string, in, out, cr, cc, cc1, reasoning, fast, compOrig, compOut int64) {
		s := ensureSession(sessions, sid, tool, smodel, root)
		t, ok := parseTS(ts)
		if !ok {
			return
		}
		row := TurnFact{TS: t, Model: model, Input: in, Output: out, CacheRead: cr, CacheCreation: cc, CacheCreation1h: cc1, Reasoning: reasoning, Fast: fast != 0, Source: "proxy"}
		s.Rows = append(s.Rows, row)
		s.CompressionOrig += compOrig
		s.CompressionOut += compOut
		if eventID != "" {
			if turnIDs[sid] == nil {
				turnIDs[sid] = map[string]bool{}
			}
			turnIDs[sid][eventID] = true
		}
		if shapeKeys[sid] == nil {
			shapeKeys[sid] = map[string]bool{}
		}
		shapeKeys[sid][shapeKeyOf(model, in, out, cr, cc)] = true
	}); err != nil {
		return nil, fmt.Errorf("advisor.LoadFacts: proxy rows: %w", err)
	}

	jsonlQ := `
		SELECT tu.session_id, COALESCE(s.tool,''), COALESCE(s.model,''), COALESCE(p.root_path,''),
		       tu.timestamp, COALESCE(tu.model,''), COALESCE(tu.source_event_id,''),
		       COALESCE(tu.input_tokens,0), COALESCE(tu.output_tokens,0),
		       COALESCE(tu.cache_read_tokens,0), COALESCE(tu.cache_creation_tokens,0),
		       COALESCE(tu.cache_creation_1h_tokens,0), COALESCE(tu.reasoning_tokens,0), COALESCE(tu.fast,0),
		       0, 0
		FROM token_usage tu
		JOIN sessions s ON s.id = tu.session_id
		LEFT JOIN projects p ON p.id = s.project_id
		WHERE tu.timestamp >= ?` + scopeFilter(opts) + `
		ORDER BY tu.session_id, tu.timestamp`
	if err := loadRows(ctx, db, jsonlQ, since, opts, func(sid, tool, smodel, root, ts, model, eventID string, in, out, cr, cc, cc1, reasoning, fast, compOrig, compOut int64) {
		if turnIDs[sid][eventID] {
			return // level-1 dedup: proxy captured this exact turn id
		}
		if shapeKeys[sid][shapeKeyOf(model, in, out, cr, cc)] {
			return // level-2 dedup: proxy twin by minute-less shape
		}
		s := ensureSession(sessions, sid, tool, smodel, root)
		t, ok := parseTS(ts)
		if !ok {
			return
		}
		s.Rows = append(s.Rows, TurnFact{TS: t, Model: model, Input: in, Output: out, CacheRead: cr, CacheCreation: cc, CacheCreation1h: cc1, Reasoning: reasoning, Fast: fast != 0, Source: "jsonl"})
	}); err != nil {
		return nil, fmt.Errorf("advisor.LoadFacts: jsonl rows: %w", err)
	}

	f := &Facts{WindowDays: opts.WindowDays, Now: now}
	for _, s := range sessions {
		if len(s.Rows) < minSessionRows {
			continue
		}
		sort.Slice(s.Rows, func(i, j int) bool { return s.Rows[i].TS.Before(s.Rows[j].TS) })
		f.Sessions = append(f.Sessions, *s)
	}
	sort.Slice(f.Sessions, func(i, j int) bool { return f.Sessions[i].ID < f.Sessions[j].ID })
	if err := loadPhase2(ctx, db, since, f); err != nil {
		return nil, err
	}
	return f, nil
}

// scopeFilter appends the optional project + tool predicates; scopeArgs
// (below) binds them in the same order.
func scopeFilter(opts Options) string {
	var sb strings.Builder
	if opts.ProjectRoot != "" {
		sb.WriteString(" AND p.root_path = ?")
	}
	if opts.Tool != "" {
		sb.WriteString(" AND s.tool = ?")
	}
	return sb.String()
}

func scopeArgs(opts Options) []any {
	var args []any
	if opts.ProjectRoot != "" {
		args = append(args, opts.ProjectRoot)
	}
	if opts.Tool != "" {
		args = append(args, opts.Tool)
	}
	return args
}

// loadRows runs one of the two union-arm queries, binding the optional
// project filter, and feeds each row to fn.
func loadRows(ctx context.Context, db *sql.DB, q, since string, opts Options,
	fn func(sid, tool, smodel, root, ts, model, eventID string, in, out, cr, cc, cc1, reasoning, fast, compOrig, compOut int64),
) error {
	args := append([]any{since}, scopeArgs(opts)...)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var sid, tool, smodel, root, ts, model, eventID string
		var in, out, cr, cc, cc1, reasoning, fast, compOrig, compOut int64
		if err := rows.Scan(&sid, &tool, &smodel, &root, &ts, &model, &eventID, &in, &out, &cr, &cc, &cc1, &reasoning, &fast, &compOrig, &compOut); err != nil {
			return err
		}
		fn(sid, tool, smodel, root, ts, model, eventID, in, out, cr, cc, cc1, reasoning, fast, compOrig, compOut)
	}
	return rows.Err()
}

func ensureSession(m map[string]*SessionFacts, sid, tool, smodel, root string) *SessionFacts {
	s, ok := m[sid]
	if !ok {
		s = &SessionFacts{ID: sid, Tool: tool, Model: smodel, ProjectRoot: root}
		m[sid] = s
	}
	return s
}

// shapeKeyOf mirrors cost/summary.go's minute-less sessionShapeKey —
// deliberately NO minute bucket (codex's rollout flush lands ~10s after the
// proxy logs the request; ~15% of turns straddle a minute boundary).
func shapeKeyOf(model string, in, out, cr, cc int64) string {
	return fmt.Sprintf("%s|%d|%d|%d|%d", model, in, out, cr, cc)
}

func parseTS(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
