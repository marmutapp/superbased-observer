package mcp

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/marmutapp/superbased-observer/internal/cachewarmsvc"
	"github.com/marmutapp/superbased-observer/internal/config"
	"github.com/marmutapp/superbased-observer/internal/intelligence/cost"
	"github.com/marmutapp/superbased-observer/internal/store"
)

// cacheStatusTool surfaces the cache-expiry warning system to the AI
// client (Part A/A3 of the cache-warm plan). The assistant can call it to
// learn whether a valuable prompt cache is about to go cold and, when
// keep-warm is in advise/enforce mode, the cheapest content-free lever to
// keep it warm (for Anthropic: switch to the 1h TTL tier). During an
// active agentic loop the assistant's own next turn re-sends the prefix
// and keeps the cache warm for free — this tool is what lets it decide to.
type cacheStatusTool struct {
	db        *sql.DB
	engine    *cost.Engine
	cacheWarm config.CacheWarmConfig
}

func newCacheStatusTool(db *sql.DB, engine *cost.Engine, cacheWarm config.CacheWarmConfig) Tool {
	return &cacheStatusTool{db: db, engine: engine, cacheWarm: cacheWarm}
}

func (*cacheStatusTool) Name() string { return "cache_status" }

func (*cacheStatusTool) Description() string {
	return "Report which prompt caches are live and how soon they expire, with the dollars at risk if each goes cold and (when keep-warm is enabled) the cheapest content-free way to keep it warm. Call this to decide whether to keep a session's cache warm — e.g. by continuing promptly, or by switching to the 1h cache tier on Anthropic. Read-only; node-local."
}

func (*cacheStatusTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "Optional. Restrict to one session id; omit for all live caches on this machine.",
			},
			"include_warm": map[string]any{
				"type":        "boolean",
				"description": "Optional. Include caches with comfortable headroom (default false: only those expiring soon or already cold).",
			},
		},
	}
}

type cacheStatusToolArgs struct {
	SessionID   string `json:"session_id"`
	IncludeWarm bool   `json:"include_warm"`
}

func (t *cacheStatusTool) Invoke(ctx context.Context, raw json.RawMessage) (any, error) {
	var args cacheStatusToolArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, err
		}
	}

	if !t.cacheWarm.Enabled {
		return map[string]any{
			"enabled": false,
			"message": "cache-expiry warnings are disabled ([cachewarm].enabled = false)",
		}, nil
	}

	var lookup cachewarmsvc.PriceLookup
	if t.engine != nil {
		lookup = t.engine.Lookup
	}
	statuses, err := cachewarmsvc.Load(ctx, store.New(t.db), lookup, t.cacheWarm, cachewarmsvc.LoadOpts{
		SessionID:   args.SessionID,
		IncludeCold: true,
		Limit:       50,
	})
	if err != nil {
		return nil, err
	}

	// Filter warm rows unless asked, so the assistant sees only actionable
	// caches by default.
	out := make([]cachewarmsvc.WindowStatus, 0, len(statuses))
	for _, st := range statuses {
		if !args.IncludeWarm && st.Severity == "ok" {
			continue
		}
		out = append(out, st)
	}

	return map[string]any{
		"enabled":       true,
		"keepwarm_mode": t.cacheWarm.Keepwarm.Mode,
		"count":         len(out),
		"windows":       out,
	}, nil
}
