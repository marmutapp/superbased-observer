package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/marmutapp/superbased-observer/internal/intelligence/advisor"
)

// -----------------------------------------------------------------------------
// get_suggestions — the advisor engine's in-session surface (spec §15.7,
// plan Phase 3). Point-reads the daemon-refreshed advisor_digest snapshot;
// it NEVER runs the engine, so a call costs one SELECT. Schema kept terse:
// this tool must not become its own E1 finding.
// -----------------------------------------------------------------------------

type getSuggestionsTool struct{ db *sql.DB }

// NewGetSuggestionsTool exposes the advisor digest as an MCP tool.
func NewGetSuggestionsTool(db *sql.DB) Tool { return &getSuggestionsTool{db: db} }

func (*getSuggestionsTool) Name() string { return "get_suggestions" }
func (*getSuggestionsTool) Description() string {
	return "Top cost/quality suggestions from the advisor (dollar-quantified, locally computed). Use to learn how this user's sessions waste spend (ballooned context, expired caches, model routing) before starting heavy work."
}

func (*getSuggestionsTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *getSuggestionsTool) Invoke(ctx context.Context, _ json.RawMessage) (any, error) {
	rep, ok, err := advisor.LoadDigest(ctx, t.db)
	if err != nil {
		return nil, err
	}
	if !ok || len(rep.Suggestions) == 0 {
		return map[string]any{"suggestions": []any{}, "note": "no suggestions above the savings/confidence floors (or the daemon digest hasn't refreshed yet)"}, nil
	}
	type slim struct {
		Title      string  `json:"title"`
		Nudge      string  `json:"nudge"`
		SavingsUSD float64 `json:"savings_usd,omitempty"`
		Confidence float64 `json:"confidence"`
	}
	outRows := make([]slim, 0, len(rep.Suggestions))
	for _, s := range rep.Suggestions {
		outRows = append(outRows, slim{Title: s.Title, Nudge: s.Nudge, SavingsUSD: s.SavingsUSD, Confidence: s.Confidence})
	}
	return map[string]any{
		"generated_at":      rep.GeneratedAt,
		"window_days":       rep.WindowDays,
		"total_savings_usd": rep.TotalSavingsUSD,
		"suggestions":       outRows,
	}, nil
}

// advisoryForPath returns a one-line advisory suffix when the queried path
// appears in the active digest's evidence (plan Phase-3 freshness-tool
// enrichment — zero new schema cost). Empty when nothing matches; always
// best-effort (P6).
func advisoryForPath(ctx context.Context, db *sql.DB, path string) string {
	if path == "" {
		return ""
	}
	rep, ok, err := advisor.LoadDigest(ctx, db)
	if err != nil || !ok {
		return ""
	}
	for _, s := range rep.Suggestions {
		for _, it := range s.Evidence.Items {
			if it.Label == path || strings.HasSuffix(path, it.Label) || strings.HasSuffix(it.Label, path) {
				return fmt.Sprintf("advisor: %s", s.Title)
			}
		}
	}
	return ""
}
