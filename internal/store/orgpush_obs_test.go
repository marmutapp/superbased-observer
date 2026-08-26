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

// stubEgressProviders wires the T8 egress seam with one canned decision row
// carrying both content-bearing columns (Tenant/User) so the strip path is
// observable.
func stubEgressProviders() ObsOrgProviders {
	return ObsOrgProviders{
		Egress: func(context.Context, orgcontract.ObsCursor, int) (orgcontract.ObsEgressBatch, error) {
			return orgcontract.ObsEgressBatch{Events: []orgcontract.ObsEgressRow{{
				TS: "2026-08-24T10:00:00Z", Mode: "enforce", RuleName: "block-frontier",
				Action: "deny", ReasonCode: "policy", RowHash: "rh-1",
				Tenant: "acme", User: "end-user-9",
			}}}, nil
		},
	}
}

// TestObsEgressComposeGating pins the T8 tier's behavioral contract (org-parity
// W5.3): the feed rides ONLY under ShareOptions.ObsEgress (its own node-side
// opt-in, independent of AdminManaged/FullContent), and within an opted-in
// tier the Tenant/User content columns ship raw only under shipsRawContent()
// — stripped otherwise, mirroring the T6 admission posture.
func TestObsEgressComposeGating(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		share       ShareOptions
		wantPresent bool
		wantContent bool
	}{
		{"nothing opted in", ShareOptions{}, false, false},
		{"admin managed but no egress opt-in", ShareOptions{AdminManaged: true}, false, false},
		{"egress opt-in, metadata-only", ShareOptions{ObsEgress: true}, true, false},
		{"egress opt-in + full content", ShareOptions{ObsEgress: true, FullContent: true}, true, true},
		{"egress opt-in + admin managed", ShareOptions{ObsEgress: true, AdminManaged: true}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			s.SetObsOrgProviders(stubEgressProviders())
			batch, err := s.SelectUnpushedSince(ctx, PushCursor{}, 1<<20, "org-1", "dev@acme.example", tc.share, ScopeOptions{})
			if err != nil {
				t.Fatalf("SelectUnpushedSince: %v", err)
			}
			got := len(batch.ObsEgressDecisions) > 0
			if got != tc.wantPresent {
				t.Fatalf("ObsEgressDecisions present = %v, want %v", got, tc.wantPresent)
			}
			if !tc.wantPresent {
				return
			}
			r := batch.ObsEgressDecisions[0]
			if r.OrgID != "org-1" || r.UserEmail != "dev@acme.example" {
				t.Errorf("row not stamped with org/pusher: %+v", r)
			}
			if r.Action != "deny" || r.RowHash != "rh-1" {
				t.Errorf("decision fields not carried verbatim: %+v", r)
			}
			if tc.wantContent {
				if r.Tenant != "acme" || r.User != "end-user-9" {
					t.Errorf("tenant/user should ship raw under shipsRawContent(): %+v", r)
				}
			} else if r.Tenant != "" || r.User != "" {
				t.Errorf("tenant/user leaked without shipsRawContent(): tenant=%q user=%q", r.Tenant, r.User)
			}
		})
	}
}
