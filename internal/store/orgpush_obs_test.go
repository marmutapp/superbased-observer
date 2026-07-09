// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 SuperBased

package store

import (
	"context"
	"testing"

	"github.com/marmutapp/superbased-observer/internal/orgcontract"
)

// stubEndUserProviders wires just enough of the obs seam to exercise the T5
// per-end-user compose path: a Summaries stub (so an aggregate-only batch is
// non-empty) plus an EndUserSpend stub returning one canned end-user row.
func stubEndUserProviders() ObsOrgProviders {
	return ObsOrgProviders{
		Summaries: func(context.Context, int) ([]orgcontract.ObsSummaryRow, error) {
			return []orgcontract.ObsSummaryRow{{Day: "2026-07-06", Model: "gpt-4o", Traces: 1}}, nil
		},
		EndUserSpend: func(context.Context, int) ([]orgcontract.ObsEndUserSpendRow, error) {
			return []orgcontract.ObsEndUserSpendRow{{Day: "2026-07-06", EndUser: "cust-42", CostUSD: 1.25, Traces: 3, TotalTokens: 900}}, nil
		},
	}
}

// TestObsEndUserSpendGatedOffByDefault confirms the T5 per-end-user tier is PII
// that rides ONLY under ObsSummary AND shipsRawContent(): absent otherwise,
// present + stamped when both gates are satisfied.
func TestObsEndUserSpendGatedOffByDefault(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		share ShareOptions
		want  bool
	}{
		{"nothing opted in", ShareOptions{}, false},
		{"obs summary only, no raw content", ShareOptions{ObsSummary: true}, false},
		{"full content but no obs summary", ShareOptions{FullContent: true}, false},
		{"obs summary + full content", ShareOptions{ObsSummary: true, FullContent: true}, true},
		{"obs summary + admin managed", ShareOptions{ObsSummary: true, AdminManaged: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			s.SetObsOrgProviders(stubEndUserProviders())
			batch, err := s.SelectUnpushedSince(ctx, PushCursor{}, 1<<20, "org-1", "dev@acme.example", tc.share, ScopeOptions{})
			if err != nil {
				t.Fatalf("SelectUnpushedSince: %v", err)
			}
			got := len(batch.ObsEndUserSpend) > 0
			if got != tc.want {
				t.Fatalf("ObsEndUserSpend present = %v, want %v (rows=%+v)", got, tc.want, batch.ObsEndUserSpend)
			}
			if tc.want {
				r := batch.ObsEndUserSpend[0]
				if r.OrgID != "org-1" || r.UserEmail != "dev@acme.example" {
					t.Errorf("row not stamped with org/pusher: %+v", r)
				}
				if r.EndUser != "cust-42" {
					t.Errorf("end_user = %q, want cust-42 (app-shared, not overwritten)", r.EndUser)
				}
			}
		})
	}
}

// TestObsEndUserSpendKeepsBatchNonEmpty confirms an aggregate-only end-user
// batch (no row data) is not treated as empty, so it still ships.
func TestObsEndUserSpendKeepsBatchNonEmpty(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(t)
	s.SetObsOrgProviders(ObsOrgProviders{
		EndUserSpend: func(context.Context, int) ([]orgcontract.ObsEndUserSpendRow, error) {
			return []orgcontract.ObsEndUserSpendRow{{Day: "2026-07-06", EndUser: "cust-42", CostUSD: 1}}, nil
		},
	})
	batch, err := s.SelectUnpushedSince(ctx, PushCursor{}, 1<<20, "org-1", "dev@acme.example",
		ShareOptions{ObsSummary: true, FullContent: true}, ScopeOptions{})
	if err != nil {
		t.Fatalf("SelectUnpushedSince: %v", err)
	}
	if batch.RowCount() != 0 {
		t.Fatalf("expected no counted rows, got %d", batch.RowCount())
	}
	if batch.Empty() {
		t.Fatalf("aggregate-only end-user batch reported Empty; it would be dropped")
	}
}
