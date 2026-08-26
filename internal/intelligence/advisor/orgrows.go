// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package advisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// orgrows.go is the W3.2 org-wire mapping for the Suggestions/Advisor
// feature (docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W3.2").
// It lives HERE, not in internal/store, because the mapping reuses the
// node's own digest read-side (LoadDigest) and the advisor-owned
// advisor_state table — and internal/store importing this package created a
// test-only import cycle (advisor's internal tests exercise store-backed
// fixtures). The push path reaches it through the injected provider seam
// store.Store.SetAdvisorOrgProvider, the same shape as store.ObsOrgProviders
// (module-boundary discipline: internal/store never imports intelligence
// packages).

// OrgSuggestionRows snapshots the node's current advisor digest (LoadDigest)
// into wire rows, reusing the node's own read-side rather than re-deriving
// it — so the org panel's suggestions are guaranteed to match the node
// dashboard's Suggestions page for the same node. Returns an empty slice
// (not an error) when no digest has been generated yet.
func OrgSuggestionRows(ctx context.Context, db *sql.DB) ([]orgcontract.AdvisorSuggestionRow, error) {
	rep, found, err := LoadDigest(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("advisor.OrgSuggestionRows: load digest: %w", err)
	}
	if !found || len(rep.Suggestions) == 0 {
		return nil, nil
	}

	statuses, err := loadOrgStatuses(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("advisor.OrgSuggestionRows: %w", err)
	}

	out := make([]orgcontract.AdvisorSuggestionRow, 0, len(rep.Suggestions))
	for _, sug := range rep.Suggestions {
		row, err := buildOrgSuggestionRow(sug, statuses[sug.DedupKey], rep.GeneratedAt)
		if err != nil {
			return nil, fmt.Errorf("advisor.OrgSuggestionRows: %s: %w", sug.DedupKey, err)
		}
		out = append(out, row)
	}
	return out, nil
}

// loadOrgStatuses reads the node-local advisor_state table into a
// dedup_key -> status map, so a currently-active suggestion whose state just
// transitioned (e.g. marked Acted moments before this push) reports its real
// status rather than always shipping empty.
func loadOrgStatuses(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT dedup_key, status FROM advisor_state`)
	if err != nil {
		return nil, fmt.Errorf("advisor_state: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var key, status string
		if err := rows.Scan(&key, &status); err != nil {
			return nil, fmt.Errorf("advisor_state scan: %w", err)
		}
		out[key] = status
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("advisor_state: %w", err)
	}
	return out, nil
}

// buildOrgSuggestionRow shapes one Suggestion into the wire row,
// JSON-encoding Evidence and flattening the optional Action pointer.
// Evidence/Title/Nudge/ScopeID are carried verbatim (enterprise-raw posture
// — see orgcontract.AdvisorSuggestionRow's doc comment).
func buildOrgSuggestionRow(sug Suggestion, status, generatedAt string) (orgcontract.AdvisorSuggestionRow, error) {
	evJSON, err := json.Marshal(sug.Evidence)
	if err != nil {
		return orgcontract.AdvisorSuggestionRow{}, fmt.Errorf("marshal evidence: %w", err)
	}

	row := orgcontract.AdvisorSuggestionRow{
		SuggestionKey: sug.DedupKey,
		Detector:      sug.Detector,
		Category:      sug.Category,
		Scope:         sug.Scope,
		ScopeID:       sug.ScopeID,
		Severity:      sug.Severity,
		Title:         sug.Title,
		Nudge:         sug.Nudge,
		SavingsUSD:    sug.SavingsUSD,
		SavingsMin:    sug.SavingsMin,
		Confidence:    sug.Confidence,
		EvidenceJSON:  string(evJSON),
		Status:        status,
		ComputedAt:    sug.ComputedAt,
		WindowDays:    sug.WindowDays,
		GeneratedAt:   generatedAt,
	}
	if sug.Action != nil {
		row.ActionKind = sug.Action.Kind
		row.ActionTarget = sug.Action.Target
		row.ActionLabel = sug.Action.Label
	}
	return row, nil
}
