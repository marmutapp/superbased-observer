package store

import (
	"context"
	"fmt"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/modelvalue"
	"github.com/marmutapp/superbased-observer/internal/models"
	"github.com/marmutapp/superbased-observer/internal/routing"
)

// LoadModelValueFacts is the store seam for the Model Value Report and
// the routing replay surfaces (model-routing spec §R17/§R18): it loads
// the deduped proxy∪JSONL turn substrate plus the normalized action
// stream into a modelvalue.Facts bundle. modelvalue itself imports no
// SQL — this file owns the queries; the boundary resolutions (command
// class, phase hint, subagent name) happen here so no content leaves
// the seam (§24.3).
//
// Dedup mirrors the advisor loader / cost summary two-level rule:
// api_turns rows load first and win; token_usage rows that duplicate a
// proxy turn by (request_id == source_event_id) or by the minute-less
// session shape key are dropped.
//
// Price and Tiers are left nil — callers inject them (PriceFn from
// cost.Engine.ComputeBreakdown; nil Tiers means the shipped seed table).
func (s *Store) LoadModelValueFacts(ctx context.Context, opts modelvalue.LoadOptions) (*modelvalue.Facts, error) {
	now := opts.EffectiveNow()
	windowDays := opts.EffectiveWindowDays()
	since := now.AddDate(0, 0, -windowDays).UTC().Format(time.RFC3339)

	f := &modelvalue.Facts{WindowDays: windowDays, GeneratedAt: now}

	turnIDs := map[string]map[string]bool{}   // session → request_id set
	shapeKeys := map[string]map[string]bool{} // session → shape-key set

	// Queries are assembled here (the only dynamic part is the optional
	// project predicate, always parameter-bound) and executed by the
	// per-arm loaders — the advisor-loader structure.
	proxyQ := `
		SELECT at.session_id, COALESCE(s.project_id, 0), COALESCE(p.root_path, ''),
		       at.timestamp, COALESCE(at.model, ''), COALESCE(at.request_id, ''),
		       COALESCE(at.input_tokens, 0), COALESCE(at.output_tokens, 0),
		       COALESCE(at.cache_read_tokens, 0), COALESCE(at.cache_creation_tokens, 0),
		       COALESCE(at.cache_creation_1h_tokens, 0), COALESCE(at.web_search_requests, 0),
		       COALESCE(at.fast, 0), COALESCE(at.cost_usd, 0),
		       COALESCE(at.message_count, 0), COALESCE(at.tool_use_count, 0),
		       COALESCE(at.time_to_first_token_ms, 0), COALESCE(at.total_response_ms, 0),
		       COALESCE(at.http_status, 0), COALESCE(at.error_class, '')
		FROM api_turns at
		JOIN sessions s ON s.id = at.session_id
		LEFT JOIN projects p ON p.id = s.project_id
		WHERE at.timestamp >= ?` + mvScope(opts) + `
		ORDER BY at.session_id, at.timestamp`
	jsonlQ := `
		SELECT tu.session_id, COALESCE(s.project_id, 0), COALESCE(p.root_path, ''),
		       tu.timestamp, COALESCE(tu.model, ''), COALESCE(tu.source_event_id, ''),
		       COALESCE(tu.input_tokens, 0), COALESCE(tu.output_tokens, 0),
		       COALESCE(tu.cache_read_tokens, 0), COALESCE(tu.cache_creation_tokens, 0),
		       COALESCE(tu.cache_creation_1h_tokens, 0), COALESCE(tu.web_search_requests, 0),
		       COALESCE(tu.reasoning_tokens, 0), COALESCE(tu.fast, 0)
		FROM token_usage tu
		JOIN sessions s ON s.id = tu.session_id
		LEFT JOIN projects p ON p.id = s.project_id
		WHERE tu.timestamp >= ?` + mvScope(opts) + `
		ORDER BY tu.session_id, tu.timestamp`
	actionsQ := `
		SELECT a.session_id, a.timestamp, a.action_type,
		       COALESCE(a.success, 1), COALESCE(a.is_sidechain, 0),
		       COALESCE(a.duration_ms, 0),
		       CASE WHEN a.action_type IN (?, ?, ?) THEN COALESCE(a.target, '') ELSE '' END
		FROM actions a
		JOIN sessions s ON s.id = a.session_id
		LEFT JOIN projects p ON p.id = s.project_id
		WHERE a.timestamp >= ?` + mvScope(opts) + `
		ORDER BY a.session_id, a.timestamp`

	if err := s.loadModelValueProxyTurns(ctx, proxyQ, since, opts, f, turnIDs, shapeKeys); err != nil {
		return nil, fmt.Errorf("store.LoadModelValueFacts: proxy rows: %w", err)
	}
	if err := s.loadModelValueJSONLTurns(ctx, jsonlQ, since, opts, f, turnIDs, shapeKeys); err != nil {
		return nil, fmt.Errorf("store.LoadModelValueFacts: jsonl rows: %w", err)
	}
	if err := s.loadModelValueActions(ctx, actionsQ, since, opts, f); err != nil {
		return nil, fmt.Errorf("store.LoadModelValueFacts: actions: %w", err)
	}
	return f, nil
}

// mvScope appends the optional project predicate; mvScopeArgs binds it.
func mvScope(opts modelvalue.LoadOptions) string {
	if opts.ProjectRoot != "" {
		return " AND p.root_path = ?"
	}
	return ""
}

func mvScopeArgs(since string, opts modelvalue.LoadOptions) []any {
	args := []any{since}
	if opts.ProjectRoot != "" {
		args = append(args, opts.ProjectRoot)
	}
	return args
}

func (s *Store) loadModelValueProxyTurns(
	ctx context.Context, q, since string, opts modelvalue.LoadOptions,
	f *modelvalue.Facts, turnIDs, shapeKeys map[string]map[string]bool,
) error {
	rows, err := s.db.QueryContext(ctx, q, mvScopeArgs(since, opts)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			sid, root, ts, model, reqID, errClass string
			projectID, in, out, cr, cc, cc1, ws   int64
			fast                                  int64
			costUSD                               float64
			msgCount, toolCount                   int
			ttft, totalMS                         int64
			httpStatus                            int
		)
		if err := rows.Scan(&sid, &projectID, &root, &ts, &model, &reqID,
			&in, &out, &cr, &cc, &cc1, &ws, &fast, &costUSD,
			&msgCount, &toolCount, &ttft, &totalMS, &httpStatus, &errClass); err != nil {
			return err
		}
		t := parseStamp(ts)
		if t.IsZero() {
			continue
		}
		f.Turns = append(f.Turns, modelvalue.TurnRow{
			SessionID: sid, ProjectID: projectID, ProjectRoot: root,
			Timestamp: t, Model: model,
			Input: in, Output: out, CacheRead: cr, CacheCreation: cc,
			CacheCreation1h: cc1, WebSearchRequests: ws, Fast: fast != 0,
			MessageCount: msgCount, ToolUseCount: toolCount,
			TTFTMs: ttft, TotalMs: totalMS, HasLatency: totalMS > 0,
			HTTPStatus: httpStatus, ErrorClass: errClass,
			HasStatus:     httpStatus > 0 || errClass != "",
			StoredCostUSD: costUSD,
		})
		if reqID != "" {
			if turnIDs[sid] == nil {
				turnIDs[sid] = map[string]bool{}
			}
			turnIDs[sid][reqID] = true
		}
		if shapeKeys[sid] == nil {
			shapeKeys[sid] = map[string]bool{}
		}
		shapeKeys[sid][mvShapeKey(model, in, out, cr, cc)] = true
	}
	return rows.Err()
}

func (s *Store) loadModelValueJSONLTurns(
	ctx context.Context, q, since string, opts modelvalue.LoadOptions,
	f *modelvalue.Facts, turnIDs, shapeKeys map[string]map[string]bool,
) error {
	rows, err := s.db.QueryContext(ctx, q, mvScopeArgs(since, opts)...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			sid, root, ts, model, eventID       string
			projectID, in, out, cr, cc, cc1, ws int64
			reasoning, fast                     int64
		)
		if err := rows.Scan(&sid, &projectID, &root, &ts, &model, &eventID,
			&in, &out, &cr, &cc, &cc1, &ws, &reasoning, &fast); err != nil {
			return err
		}
		if turnIDs[sid][eventID] {
			continue // level-1 dedup: proxy captured this exact turn id
		}
		if shapeKeys[sid][mvShapeKey(model, in, out, cr, cc)] {
			continue // level-2 dedup: proxy twin by minute-less shape
		}
		t := parseStamp(ts)
		if t.IsZero() {
			continue
		}
		f.Turns = append(f.Turns, modelvalue.TurnRow{
			SessionID: sid, ProjectID: projectID, ProjectRoot: root,
			Timestamp: t, Model: model,
			Input: in, Output: out, CacheRead: cr, CacheCreation: cc,
			CacheCreation1h: cc1, WebSearchRequests: ws, Reasoning: reasoning,
			Fast: fast != 0,
			// JSONL rows carry no latency or HTTP status — the
			// capability flags stay false so the report grades only
			// what was observed.
		})
	}
	return rows.Err()
}

// loadModelValueActions loads the normalized action stream with all
// content resolved away at this boundary: run_command targets become a
// CommandClass, permission-mode targets a PhaseHint, subagent_start
// targets the persona name. The target column never leaves this
// function for any other action type.
func (s *Store) loadModelValueActions(
	ctx context.Context, q, since string, opts modelvalue.LoadOptions, f *modelvalue.Facts,
) error {
	args := []any{models.ActionRunCommand, models.ActionPermissionMode, models.ActionSubagentStart}
	args = append(args, mvScopeArgs(since, opts)...)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			sid, ts, actionType, target string
			success, sidechain          int64
			durationMS                  int64
		)
		if err := rows.Scan(&sid, &ts, &actionType, &success, &sidechain, &durationMS, &target); err != nil {
			return err
		}
		t := parseStamp(ts)
		if t.IsZero() {
			continue
		}
		row := modelvalue.ActionRow{
			SessionID: sid, Timestamp: t, Type: actionType,
			Success: success != 0, IsSidechain: sidechain != 0, DurationMs: durationMS,
		}
		switch actionType {
		case models.ActionRunCommand:
			row.CommandClass = routing.ResolveCommandClass(target)
		case models.ActionPermissionMode:
			row.PhaseHint = target
		case models.ActionSubagentStart:
			row.SubagentName = target
		}
		f.Actions = append(f.Actions, row)
	}
	return rows.Err()
}

// mvShapeKey mirrors cost/summary.go's minute-less sessionShapeKey
// (deliberately NO minute bucket — codex's rollout flush lands ~10s
// after the proxy logs the request).
func mvShapeKey(model string, in, out, cr, cc int64) string {
	return fmt.Sprintf("%s|%d|%d|%d|%d", model, in, out, cr, cc)
}
