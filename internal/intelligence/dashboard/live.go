package dashboard

import (
	"net/http"
	"sort"
	"time"

	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
)

// Live session view (usability arc P6.1 / review §8.2): the "now
// playing" panel. One endpoint aggregates everything the page needs
// per poll — sessions with activity inside the window, each with
// lifetime token/cost rollups and its most recent actions — so the
// 5-second polling loop costs one round trip, not five.

// liveAction is one feed row on a live session card.
type liveAction struct {
	ID         int64  `json:"id"`
	Timestamp  string `json:"timestamp"`
	ActionType string `json:"action_type"`
	Target     string `json:"target,omitempty"`
	Success    bool   `json:"success"`
}

// liveTokens carries the four billing buckets for a session lifetime.
type liveTokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// liveSession is one active session: identity, lifetime rollups
// (cost/tokens tick up while the session runs), and the recent-action
// feed. Window only decides WHICH sessions are active; the rollups are
// session-lifetime so the numbers read as "this session so far".
type liveSession struct {
	SessionID    string       `json:"session_id"`
	Tool         string       `json:"tool"`
	ProjectRoot  string       `json:"project_root,omitempty"`
	StartedAt    string       `json:"started_at"`
	LastActivity string       `json:"last_activity"`
	ActionsTotal int64        `json:"actions_total"`
	Turns        int64        `json:"turns"`
	Models       []string     `json:"models,omitempty"`
	Tokens       liveTokens   `json:"tokens"`
	CostUSD      float64      `json:"cost_usd"`
	Recent       []liveAction `json:"recent_actions"`
}

// handleLive serves GET /api/live?window_minutes=N (default 15,
// clamped 1–240). A session is active when any action, api_turn, or
// token_usage row landed inside the window; the newest 8 by last
// activity are returned. Read-only; designed for 5s polling from the
// Live page (visibility-gated client-side).
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	windowMin := intArg(r, "window_minutes", 15, 1, 240)
	now := time.Now().UTC()
	since := now.Add(-time.Duration(windowMin) * time.Minute).Format(time.RFC3339Nano)

	active := []liveSession{}
	if s.db() != nil {
		// Active sessions, newest activity first. last_activity is the
		// max timestamp across the three activity tables; all are
		// RFC3339 UTC strings, so MAX() string comparison is correct.
		rows, err := s.db().QueryContext(r.Context(),
			`SELECT s.id, s.tool, COALESCE(p.root_path, ''), s.started_at,
			        MAX(COALESCE((SELECT MAX(a.timestamp) FROM actions a WHERE a.session_id = s.id), ''),
			            COALESCE((SELECT MAX(t.timestamp) FROM api_turns t WHERE t.session_id = s.id), ''),
			            COALESCE((SELECT MAX(u.timestamp) FROM token_usage u WHERE u.session_id = s.id), '')) AS last_activity,
			        (SELECT COUNT(*) FROM actions a WHERE a.session_id = s.id) AS actions_total
			 FROM sessions s
			 LEFT JOIN projects p ON p.id = s.project_id
			 WHERE EXISTS (SELECT 1 FROM actions a WHERE a.session_id = s.id AND a.timestamp >= ?)
			    OR EXISTS (SELECT 1 FROM api_turns t WHERE t.session_id = s.id AND t.timestamp >= ?)
			    OR EXISTS (SELECT 1 FROM token_usage u WHERE u.session_id = s.id AND u.timestamp >= ?)
			 ORDER BY last_activity DESC
			 LIMIT 8`, since, since, since)
		if err != nil {
			writeErr(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var ls liveSession
			if rows.Scan(&ls.SessionID, &ls.Tool, &ls.ProjectRoot, &ls.StartedAt,
				&ls.LastActivity, &ls.ActionsTotal) == nil {
				active = append(active, ls)
			}
		}
		_ = rows.Err()

		for i := range active {
			s.attachLiveRollup(r, &active[i])
			s.attachLiveRecent(r, &active[i])
		}
	}

	writeJSON(w, map[string]any{
		"generated_at":   now.Format(time.RFC3339Nano),
		"window_minutes": windowMin,
		"active":         active,
	})
}

// attachLiveRollup fills one session's lifetime token/cost/model
// rollup. Same proxy-dedup as the cost engine surfaces (top-sessions,
// headline): api_turns rows win; token_usage rows whose
// source_event_id matches a proxy turn's request_id are skipped so
// proxied traffic isn't double-counted.
func (s *Server) attachLiveRollup(r *http.Request, ls *liveSession) {
	rows, err := s.db().QueryContext(r.Context(),
		`WITH proxy_turn_ids AS (
			SELECT request_id FROM api_turns
			 WHERE session_id = ? AND request_id IS NOT NULL AND request_id != ''
		),
		combined AS (
			SELECT at.model, at.input_tokens, at.output_tokens, at.cache_read_tokens,
			       at.cache_creation_tokens, at.cache_creation_1h_tokens,
			       0 AS reasoning_tokens, at.web_search_requests, at.cost_usd
			FROM api_turns at
			WHERE at.session_id = ?
			UNION ALL
			SELECT tu.model, tu.input_tokens, tu.output_tokens, tu.cache_read_tokens,
			       tu.cache_creation_tokens, tu.cache_creation_1h_tokens,
			       tu.reasoning_tokens, tu.web_search_requests, tu.estimated_cost_usd
			FROM token_usage tu
			WHERE tu.session_id = ?
			  AND (tu.source_event_id IS NULL OR tu.source_event_id = ''
			       OR tu.source_event_id NOT IN (SELECT request_id FROM proxy_turn_ids))
		)
		SELECT COALESCE(model, ''),
		       COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		       COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
		       COALESCE(cache_creation_1h_tokens, 0), COALESCE(reasoning_tokens, 0),
		       COALESCE(web_search_requests, 0), COALESCE(cost_usd, 0)
		FROM combined`,
		ls.SessionID, ls.SessionID, ls.SessionID)
	if err != nil {
		return
	}
	defer rows.Close()
	models := map[string]bool{}
	for rows.Next() {
		var (
			model  string
			bundle cost.TokenBundle
			rec    float64
		)
		if rows.Scan(&model, &bundle.Input, &bundle.Output,
			&bundle.CacheRead, &bundle.CacheCreation, &bundle.CacheCreation1h,
			&bundle.Reasoning, &bundle.WebSearchRequests, &rec) != nil {
			continue
		}
		ls.Turns++
		ls.Tokens.Input += bundle.Input
		ls.Tokens.Output += bundle.Output
		ls.Tokens.CacheRead += bundle.CacheRead
		ls.Tokens.CacheWrite += bundle.CacheCreation + bundle.CacheCreation1h
		if model != "" {
			models[model] = true
		}
		switch {
		case rec > 0:
			ls.CostUSD += rec
		case s.opts.CostEngine != nil:
			if p, ok := s.opts.CostEngine.Lookup(model); ok {
				ls.CostUSD += cost.Compute(p, bundle)
			}
		}
	}
	_ = rows.Err()
	for m := range models {
		ls.Models = append(ls.Models, m)
	}
	sort.Strings(ls.Models)
	if len(ls.Models) > 3 {
		ls.Models = ls.Models[:3]
	}
}

// attachLiveRecent fills the newest 8 actions for one session card's
// feed. Newest first; the client renders them top-down as a ticker.
func (s *Server) attachLiveRecent(r *http.Request, ls *liveSession) {
	rows, err := s.db().QueryContext(r.Context(),
		`SELECT id, timestamp, action_type, COALESCE(target, ''), COALESCE(success, 1)
		 FROM actions WHERE session_id = ?
		 ORDER BY timestamp DESC, id DESC LIMIT 8`, ls.SessionID)
	if err != nil {
		return
	}
	defer rows.Close()
	ls.Recent = []liveAction{}
	for rows.Next() {
		var a liveAction
		var success int64
		if rows.Scan(&a.ID, &a.Timestamp, &a.ActionType, &a.Target, &success) == nil {
			a.Success = success > 0
			ls.Recent = append(ls.Recent, a)
		}
	}
	_ = rows.Err()
}
