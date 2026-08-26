// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package store

import (
	"context"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// advisororgrows.go is the W3.2 org-wire seam for the Suggestions/Advisor
// feature (docs/plans/org-parity-full-depth-plan-2026-08-24.md §4 "W3.2").
// The digest snapshot + advisor_state read live in
// internal/intelligence/advisor/orgrows.go (advisor.OrgSuggestionRows) and
// reach the push path through the injected provider below — internal/store
// never imports intelligence packages (advisor's own internal tests import
// store, so a direct import is a test-build cycle). Same seam shape as
// ObsOrgProviders.
//
// Unlike SessionVerbosityRow (session-scoped), this wire is per-DEVELOPER:
// one snapshot-recompute of the node's CURRENT advisor digest, re-pushed
// whole on every push (idempotent server-side upsert on
// (org_id, user_email, suggestion_key), same "windowed/snapshot-recompute,
// server-upsert-idempotent" model as the Arc-4 aggregates).

// AdvisorOrgProvider snapshots the node's current advisor digest into wire
// rows. Wired by cmd/observer's buildOrgBundle to advisor.OrgSuggestionRows.
type AdvisorOrgProvider func(ctx context.Context) ([]orgcontract.AdvisorSuggestionRow, error)

// SetAdvisorOrgProvider wires the advisor org-wire provider seam.
func (s *Store) SetAdvisorOrgProvider(p AdvisorOrgProvider) { s.advisorOrg = p }

// SelectAdvisorSuggestionRows returns the node's current advisor suggestions
// as org wire rows via the injected provider. Fail-open: an unwired provider
// (a host that never enrolled the seam) ships nothing rather than erroring.
func (s *Store) SelectAdvisorSuggestionRows(ctx context.Context) ([]orgcontract.AdvisorSuggestionRow, error) {
	if s.advisorOrg == nil {
		return nil, nil
	}
	return s.advisorOrg(ctx)
}
