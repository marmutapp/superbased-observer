package store

import (
	"context"
	"fmt"
	"time"
)

// RecentToolModel is one recently-used model for a tool, aggregated from
// token_usage — the store-side half of the New Terminal model picker (B5).
// Count is the number of token_usage rows observed for the model within the
// query window; LastUsed is the most recent row's timestamp, formatted
// RFC3339 (the same string shape token_usage.timestamp is stored as).
type RecentToolModel struct {
	Model    string
	Count    int
	LastUsed string
}

// LoadRecentModelsForTool returns the models used by tool in the last
// window, most-recently-used first, capped at limit. It excludes rows with
// no model and sentinel placeholder models (e.g. "<synthetic>", stored with
// a leading '<' — never a real model identifier). Used to seed the New
// Terminal model picker's "recent" suggestions (B5) — see
// internal/intelligence/dashboard's modelSuggestionsFor, which combines this
// history with the tool's grounded ModelSpec.Known examples.
func (s *Store) LoadRecentModelsForTool(ctx context.Context, tool string, window time.Duration, limit int) ([]RecentToolModel, error) {
	// token_usage.timestamp rows are stored RFC3339Nano (see store.go's
	// timestamp helper); format the cutoff the same way so the string
	// comparison sql performs is never skewed by a shorter suffix.
	since := time.Now().UTC().Add(-window).Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `
		SELECT model, COUNT(*), MAX(timestamp)
		  FROM token_usage
		 WHERE tool = ?
		   AND model IS NOT NULL AND model != ''
		   AND model NOT LIKE '<%'
		   AND timestamp >= ?
		 GROUP BY model
		 ORDER BY MAX(timestamp) DESC
		 LIMIT ?`, tool, since, limit)
	if err != nil {
		return nil, fmt.Errorf("store.LoadRecentModelsForTool: %w", err)
	}
	defer rows.Close()

	var out []RecentToolModel
	for rows.Next() {
		var m RecentToolModel
		if err := rows.Scan(&m.Model, &m.Count, &m.LastUsed); err != nil {
			return nil, fmt.Errorf("store.LoadRecentModelsForTool: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.LoadRecentModelsForTool: rows: %w", err)
	}
	return out, nil
}
